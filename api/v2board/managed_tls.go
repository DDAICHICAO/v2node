package panel

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
)

const managedTLSCertificatePath = "/api/v2/server/tls-certificate"

type ManagedTLSCertificate struct {
	Status            string `json:"status"`
	ScopeID           uint64 `json:"scope_id"`
	Version           uint64 `json:"version"`
	Domain            string `json:"domain"`
	FullchainPEM      string `json:"fullchain_pem"`
	PrivateKeyPEM     string `json:"private_key_pem"`
	NotAfter          int64  `json:"not_after"`
	RenewAt           int64  `json:"renew_at"`
	CertificateSHA256 string `json:"certificate_sha256"`
	ForceInstall      bool   `json:"force_install"`
	SyncRequestID     string `json:"sync_request_id"`
}

type ManagedTLSACMEAccount struct {
	Email         string          `json:"email"`
	Registration  json.RawMessage `json:"registration"`
	PrivateKeyPEM string          `json:"private_key_pem"`
}

type ManagedTLSLeaseRequest struct {
	Domain  string `json:"domain"`
	LeaseID string `json:"lease_id,omitempty"`
}

type ManagedTLSLeaseResponse struct {
	Status         string                 `json:"status"`
	ScopeID        uint64                 `json:"scope_id"`
	LeaseID        string                 `json:"lease_id"`
	LeaseExpiresAt int64                  `json:"lease_expires_at"`
	RetryAfter     int                    `json:"retry_after"`
	Provider       string                 `json:"provider"`
	DNSEnv         map[string]string      `json:"dns_env"`
	ACMEAccount    *ManagedTLSACMEAccount `json:"acme_account"`
}

type ManagedTLSPublishRequest struct {
	LeaseID       string                 `json:"lease_id"`
	Domain        string                 `json:"domain"`
	FullchainPEM  string                 `json:"fullchain_pem"`
	PrivateKeyPEM string                 `json:"private_key_pem"`
	ACMEAccount   *ManagedTLSACMEAccount `json:"acme_account"`
}

type ManagedTLSFailRequest struct {
	LeaseID   string `json:"lease_id"`
	Domain    string `json:"domain"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

type ManagedTLSError struct {
	StatusCode int
	Code       string
	RetryAfter time.Duration
}

func (e *ManagedTLSError) Error() string {
	if e == nil || e.Code == "" {
		return "managed_tls_request_failed"
	}
	return e.Code
}

func (c *Client) GetManagedTLSCertificate(ctx context.Context, domain string, version uint64) (*ManagedTLSCertificate, error) {
	response, err := c.managedTLSRequest(ctx, http.MethodGet, managedTLSCertificatePath, nil, map[string]string{
		"version": strconv.FormatUint(version, 10),
	})
	if err != nil {
		return nil, err
	}
	if response.StatusCode() == http.StatusNotModified {
		return &ManagedTLSCertificate{Status: "not_modified", Version: version}, nil
	}
	if response.IsError() {
		return nil, managedTLSErrorFromResponse(response)
	}
	var certificate ManagedTLSCertificate
	if err := json.Unmarshal(response.Body(), &certificate); err != nil {
		return nil, &ManagedTLSError{StatusCode: response.StatusCode(), Code: "managed_tls_response_invalid"}
	}
	if certificate.Status == "ready" {
		if certificate.ScopeID == 0 || !strings.EqualFold(strings.TrimSuffix(certificate.Domain, "."), strings.TrimSuffix(strings.TrimSpace(domain), ".")) {
			return nil, &ManagedTLSError{StatusCode: http.StatusConflict, Code: "managed_tls_domain_mismatch"}
		}
	}
	return &certificate, nil
}

func (c *Client) LeaseManagedTLSCertificate(ctx context.Context, request ManagedTLSLeaseRequest) (*ManagedTLSLeaseResponse, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, &ManagedTLSError{Code: "managed_tls_request_invalid"}
	}
	response, err := c.managedTLSRequest(ctx, http.MethodPost, managedTLSCertificatePath+"/lease", body, nil)
	if err != nil {
		return nil, err
	}
	var lease ManagedTLSLeaseResponse
	if err := json.Unmarshal(response.Body(), &lease); err != nil {
		return nil, &ManagedTLSError{StatusCode: response.StatusCode(), Code: "managed_tls_response_invalid"}
	}
	if response.StatusCode() == http.StatusLocked && lease.Status == "cooldown" {
		return &lease, nil
	}
	if response.IsError() {
		return nil, managedTLSErrorFromResponse(response)
	}
	return &lease, nil
}

func (c *Client) PublishManagedTLSCertificate(ctx context.Context, request ManagedTLSPublishRequest) error {
	body, err := json.Marshal(request)
	if err != nil {
		return &ManagedTLSError{Code: "managed_tls_request_invalid"}
	}
	response, err := c.managedTLSRequest(ctx, http.MethodPost, managedTLSCertificatePath+"/publish", body, nil)
	if err != nil {
		return err
	}
	if response.IsError() {
		return managedTLSErrorFromResponse(response)
	}
	return nil
}

func (c *Client) FailManagedTLSCertificate(ctx context.Context, request ManagedTLSFailRequest) error {
	body, err := json.Marshal(request)
	if err != nil {
		return &ManagedTLSError{Code: "managed_tls_request_invalid"}
	}
	response, err := c.managedTLSRequest(ctx, http.MethodPost, managedTLSCertificatePath+"/fail", body, nil)
	if err != nil {
		return err
	}
	if response.IsError() {
		return managedTLSErrorFromResponse(response)
	}
	return nil
}

func (c *Client) TLSCertificateTokenConfigured() bool {
	if c == nil {
		return false
	}
	c.managedTLSCredentialMu.RLock()
	defer c.managedTLSCredentialMu.RUnlock()
	return strings.TrimSpace(c.tlsCertificateToken) != ""
}

func (c *Client) TLSCertificateTokenFingerprint() string {
	if c == nil {
		return ""
	}
	c.managedTLSCredentialMu.RLock()
	defer c.managedTLSCredentialMu.RUnlock()
	if strings.TrimSpace(c.tlsCertificateToken) == "" {
		return ""
	}
	return managedTLSTokenFingerprint(c.tlsCertificateToken)
}

func (c *Client) managedTLSRequest(ctx context.Context, method, path string, body []byte, query map[string]string) (*resty.Response, error) {
	if c == nil || c.NodeId <= 0 || strings.TrimSpace(c.instanceID) == "" {
		return nil, &ManagedTLSError{StatusCode: http.StatusUnauthorized, Code: "managed_tls_credential_missing"}
	}
	if !c.TLSCertificateTokenConfigured() {
		if err := c.EnsureManagedTLSCredential(ctx, false); err != nil {
			return nil, err
		}
	}
	rejectedToken := c.managedTLSCredentialToken()
	response, err := c.managedTLSRequestOnce(ctx, method, path, body, query, rejectedToken)
	if err != nil || response == nil || response.StatusCode() != http.StatusUnauthorized || c.tlsCertificateTokenIsExplicit() {
		return response, err
	}
	if err := c.refreshManagedTLSCredentialAfterUnauthorized(ctx, rejectedToken); err != nil {
		return nil, err
	}
	return c.managedTLSRequestOnce(ctx, method, path, body, query, c.managedTLSCredentialToken())
}

func (c *Client) managedTLSRequestOnce(ctx context.Context, method, path string, body []byte, query map[string]string, token string) (*resty.Response, error) {
	if token == "" {
		return nil, &ManagedTLSError{StatusCode: http.StatusUnauthorized, Code: "managed_tls_credential_missing"}
	}
	now := time.Now()
	if c.managedTLSNow != nil {
		now = c.managedTLSNow()
	}
	nonce, err := randomManagedTLSNonce()
	if c.managedTLSNonce != nil {
		nonce, err = c.managedTLSNonce()
	}
	if err != nil || !validManagedTLSNonce(nonce) {
		return nil, &ManagedTLSError{Code: "managed_tls_nonce_unavailable"}
	}
	timestamp := now.Unix()
	canonical := managedTLSCanonical(method, path, c.NodeId, c.instanceID, timestamp, nonce, body)

	httpClient := c.managedTLSClient
	if httpClient == nil {
		httpClient = c.client
	}
	if httpClient == nil {
		return nil, &ManagedTLSError{Code: "managed_tls_client_unavailable"}
	}
	request := httpClient.R().
		SetContext(ctx).
		SetHeader("X-SNTP-Node-ID", strconv.Itoa(c.NodeId)).
		SetHeader("X-SNTP-Instance-ID", c.instanceID).
		SetHeader("X-SNTP-Timestamp", strconv.FormatInt(timestamp, 10)).
		SetHeader("X-SNTP-Nonce", nonce).
		SetHeader("X-SNTP-Signature", managedTLSSignature(token, canonical)).
		SetHeader("Accept", "application/json")
	for key, value := range query {
		request.SetQueryParam(key, value)
	}
	if body != nil {
		request.SetHeader("Content-Type", "application/json")
		request.SetBody(body)
	}
	response, requestErr := request.Execute(method, path)
	if requestErr != nil {
		return nil, &ManagedTLSError{Code: "managed_tls_transport_error"}
	}
	if response == nil {
		return nil, &ManagedTLSError{Code: "managed_tls_response_missing"}
	}
	return response, nil
}

func (c *Client) managedTLSCredentialToken() string {
	if c == nil {
		return ""
	}
	c.managedTLSCredentialMu.RLock()
	defer c.managedTLSCredentialMu.RUnlock()
	return strings.TrimSpace(c.tlsCertificateToken)
}

func (c *Client) tlsCertificateTokenIsExplicit() bool {
	if c == nil {
		return false
	}
	c.managedTLSCredentialMu.RLock()
	defer c.managedTLSCredentialMu.RUnlock()
	return c.tlsCertificateTokenExplicit
}

func managedTLSCanonical(method, path string, nodeID int, instanceID string, timestamp int64, nonce string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	return strings.ToUpper(method) + "\n" + path + "\n" + strconv.Itoa(nodeID) + "\n" + instanceID + "\n" +
		strconv.FormatInt(timestamp, 10) + "\n" + nonce + "\n" + hex.EncodeToString(bodyHash[:])
}

func managedTLSSignature(token, canonical string) string {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func randomManagedTLSNonce() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validManagedTLSNonce(value string) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func managedTLSErrorFromResponse(response *resty.Response) error {
	if response == nil {
		return &ManagedTLSError{Code: "managed_tls_response_missing"}
	}
	payload := struct {
		Code       string `json:"code"`
		Status     string `json:"status"`
		RetryAfter int    `json:"retry_after"`
	}{}
	_ = json.Unmarshal(response.Body(), &payload)
	code := strings.TrimSpace(payload.Code)
	if code == "" && response.StatusCode() == http.StatusLocked {
		code = "managed_tls_cooldown"
	}
	if code == "" {
		code = fmt.Sprintf("managed_tls_http_%d", response.StatusCode())
	}
	return &ManagedTLSError{
		StatusCode: response.StatusCode(),
		Code:       code,
		RetryAfter: time.Duration(max(payload.RetryAfter, 0)) * time.Second,
	}
}
