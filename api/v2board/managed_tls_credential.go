package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	managedTLSCredentialPath           = "/api/v2/server/tls-certificate/credential"
	managedTLSAutoCredentialCapability = "managed_tls_auto_credential_v1"
)

var managedTLSCredentialBaseDir = "/etc/v2node/credentials"

type managedTLSCredentialRecord struct {
	Version     int    `json:"version"`
	APIHost     string `json:"api_host"`
	NodeID      int    `json:"node_id"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
}

type managedTLSCredentialStore struct {
	path    string
	apiHost string
	nodeID  int
}

func newManagedTLSCredentialStore(baseDir, apiHost string, nodeID int) *managedTLSCredentialStore {
	normalizedHost := normalizeManagedTLSAPIHost(apiHost)
	hostHash := sha256.Sum256([]byte(normalizedHost))
	filename := fmt.Sprintf("node-%d-%s.json", nodeID, hex.EncodeToString(hostHash[:])[:16])
	return &managedTLSCredentialStore{
		path:    filepath.Join(baseDir, filename),
		apiHost: normalizedHost,
		nodeID:  nodeID,
	}
}

func (s *managedTLSCredentialStore) Load() (managedTLSCredentialRecord, error) {
	var record managedTLSCredentialRecord
	if s == nil || s.nodeID <= 0 || s.apiHost == "" {
		return record, errors.New("managed_tls_credential_store_invalid")
	}
	content, err := os.ReadFile(s.path)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(content, &record); err != nil {
		return managedTLSCredentialRecord{}, errors.New("managed_tls_credential_file_invalid")
	}
	if err := s.validate(record); err != nil {
		return managedTLSCredentialRecord{}, err
	}
	return record, nil
}

func (s *managedTLSCredentialStore) Save(record managedTLSCredentialRecord) error {
	if s == nil || s.nodeID <= 0 || s.apiHost == "" {
		return errors.New("managed_tls_credential_store_invalid")
	}
	record.Version = 1
	record.APIHost = s.apiHost
	record.NodeID = s.nodeID
	if err := s.validate(record); err != nil {
		return err
	}
	content, err := json.Marshal(record)
	if err != nil {
		return errors.New("managed_tls_credential_encode_failed")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return errors.New("managed_tls_credential_directory_failed")
	}
	if err := os.Chmod(filepath.Dir(s.path), 0o700); err != nil {
		return errors.New("managed_tls_credential_permission_failed")
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".managed-tls-credential-*")
	if err != nil {
		return errors.New("managed_tls_credential_write_failed")
	}
	temporaryPath := temporary.Name()
	cleanup := func() { _ = os.Remove(temporaryPath) }
	defer cleanup()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return errors.New("managed_tls_credential_permission_failed")
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return errors.New("managed_tls_credential_write_failed")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("managed_tls_credential_sync_failed")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("managed_tls_credential_write_failed")
	}
	if err := replaceManagedTLSCredentialFile(temporaryPath, s.path); err != nil {
		return err
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return errors.New("managed_tls_credential_permission_failed")
	}
	return nil
}

func (s *managedTLSCredentialStore) validate(record managedTLSCredentialRecord) error {
	if record.Version != 1 || normalizeManagedTLSAPIHost(record.APIHost) != s.apiHost || record.NodeID != s.nodeID {
		return errors.New("managed_tls_credential_identity_mismatch")
	}
	record.Token = strings.TrimSpace(record.Token)
	if len(record.Token) < 12 || record.Fingerprint != managedTLSTokenFingerprint(record.Token) {
		return errors.New("managed_tls_credential_invalid")
	}
	return nil
}

func replaceManagedTLSCredentialFile(source, target string) error {
	if err := os.Rename(source, target); err == nil {
		return nil
	}
	backup := target + ".previous"
	_ = os.Remove(backup)
	if err := os.Rename(target, backup); err != nil && !os.IsNotExist(err) {
		return errors.New("managed_tls_credential_replace_failed")
	}
	if err := os.Rename(source, target); err != nil {
		_ = os.Rename(backup, target)
		return errors.New("managed_tls_credential_replace_failed")
	}
	_ = os.Remove(backup)
	return nil
}

func normalizeManagedTLSAPIHost(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}

func managedTLSTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])[:16]
}

type managedTLSCredentialEnvelope struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Code    string `json:"code"`
	Data    struct {
		Configured  bool   `json:"configured"`
		Fingerprint string `json:"fingerprint"`
		Token       string `json:"token"`
	} `json:"data"`
}

func (c *Client) EnsureManagedTLSCredential(ctx context.Context, force bool) error {
	if c == nil || c.NodeId <= 0 || strings.TrimSpace(c.instanceID) == "" || c.client == nil {
		return &ManagedTLSError{StatusCode: http.StatusUnauthorized, Code: "managed_tls_credential_missing"}
	}
	c.managedTLSCredentialMu.Lock()
	defer c.managedTLSCredentialMu.Unlock()
	if c.tlsCertificateTokenExplicit {
		return nil
	}
	if !force && strings.TrimSpace(c.tlsCertificateToken) != "" {
		return nil
	}
	return c.fetchManagedTLSCredentialLocked(ctx)
}

func (c *Client) refreshManagedTLSCredentialAfterUnauthorized(ctx context.Context, rejectedToken string) error {
	if c == nil {
		return &ManagedTLSError{StatusCode: http.StatusUnauthorized, Code: "managed_tls_credential_missing"}
	}
	c.managedTLSCredentialMu.Lock()
	defer c.managedTLSCredentialMu.Unlock()
	if c.tlsCertificateTokenExplicit {
		return nil
	}
	currentToken := strings.TrimSpace(c.tlsCertificateToken)
	if currentToken != "" && currentToken != strings.TrimSpace(rejectedToken) {
		return nil
	}
	return c.fetchManagedTLSCredentialLocked(ctx)
}

func (c *Client) fetchManagedTLSCredentialLocked(ctx context.Context) error {
	response, err := c.client.R().
		SetContext(ctx).
		SetHeader("Accept", "application/json").
		Post(managedTLSCredentialPath)
	if err != nil || response == nil {
		return &ManagedTLSError{Code: "managed_tls_credential_transport_error"}
	}
	var envelope managedTLSCredentialEnvelope
	if err := json.Unmarshal(response.Body(), &envelope); err != nil {
		return &ManagedTLSError{StatusCode: response.StatusCode(), Code: "managed_tls_credential_response_invalid"}
	}
	if response.IsError() || strings.EqualFold(strings.TrimSpace(envelope.Status), "fail") {
		return &ManagedTLSError{
			StatusCode: response.StatusCode(),
			Code:       managedTLSCredentialErrorCode(envelope.Code, envelope.Message),
		}
	}
	token := strings.TrimSpace(envelope.Data.Token)
	fingerprint := strings.TrimSpace(envelope.Data.Fingerprint)
	if !envelope.Data.Configured || len(token) < 12 || fingerprint == "" || fingerprint != managedTLSTokenFingerprint(token) {
		return &ManagedTLSError{StatusCode: response.StatusCode(), Code: "managed_tls_credential_invalid"}
	}
	if c.managedTLSCredentialStore == nil {
		c.managedTLSCredentialStore = newManagedTLSCredentialStore(managedTLSCredentialBaseDir, c.APIHost, c.NodeId)
	}
	if err := c.managedTLSCredentialStore.Save(managedTLSCredentialRecord{
		Version: 1, APIHost: normalizeManagedTLSAPIHost(c.APIHost), NodeID: c.NodeId,
		Token: token, Fingerprint: fingerprint,
	}); err != nil {
		return &ManagedTLSError{Code: err.Error()}
	}
	c.tlsCertificateToken = token
	return nil
}

func managedTLSCredentialErrorCode(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !strings.HasPrefix(value, "managed_tls_") || len(value) > 96 {
			continue
		}
		valid := true
		for _, char := range value {
			if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' {
				continue
			}
			valid = false
			break
		}
		if valid {
			return value
		}
	}
	return "managed_tls_credential_unavailable"
}
