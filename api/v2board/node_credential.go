package panel

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// IsNodeAPICredentialFor checks the wire format and scope, not the panel's HMAC.
// The panel must independently accept the credential before it is persisted.
func IsNodeAPICredentialFor(key string, nodeID int) bool {
	prefix := "sntp-node-v1.v2node." + strconv.Itoa(nodeID) + "."
	if nodeID < 1 || !strings.HasPrefix(key, prefix) {
		return false
	}
	signature := strings.TrimPrefix(key, prefix)
	if len(signature) != 64 || signature != strings.ToLower(signature) {
		return false
	}
	_, err := hex.DecodeString(signature)
	return err == nil
}

// ExchangeNodeAPICredential does not mutate the client or configuration. Both
// the existing and the new credential must authenticate over verified HTTPS.
func (c *Client) ExchangeNodeAPICredential(ctx context.Context) (string, error) {
	if c == nil {
		return "", errors.New("credential_config_invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	key, err := c.fetchNodeAPICredential(ctx, c.Token)
	if err != nil {
		return "", err
	}
	verified, err := c.fetchNodeAPICredential(ctx, key)
	if err != nil || verified != key {
		return "", errors.New("credential_verification_failed")
	}
	return key, nil
}

func (c *Client) fetchNodeAPICredential(ctx context.Context, token string) (string, error) {
	if c == nil || c.client == nil || c.NodeId < 1 || token == "" {
		return "", errors.New("credential_config_invalid")
	}
	endpoint, err := url.Parse(c.APIHost)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", errors.New("credential_https_required")
	}
	endpoint.Path, endpoint.RawPath = strings.TrimRight(endpoint.Path, "/")+"/api/v2/server/credential", ""
	query := url.Values{"node_type": {"v2node"}, "node_id": {strconv.Itoa(c.NodeId)}}
	if c.instanceID != "" {
		query.Set("instance_id", c.instanceID)
	}
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		return "", errors.New("credential_config_invalid")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	// Copy the transport (including certificate trust), and independently forbid
	// redirects even when this client was constructed outside New.
	client := *c.client.GetClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("credential_transport_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("credential_request_rejected")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if err != nil || len(body) > 8192 {
		return "", errors.New("credential_response_invalid")
	}
	var envelope struct {
		Data struct {
			NodeType string `json:"node_type"`
			NodeID   int    `json:"node_id"`
			APIKey   string `json:"api_key"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Data.NodeType != "v2node" || envelope.Data.NodeID != c.NodeId || !IsNodeAPICredentialFor(envelope.Data.APIKey, c.NodeId) {
		return "", errors.New("credential_response_invalid")
	}
	return envelope.Data.APIKey, nil
}
