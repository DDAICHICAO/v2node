package panel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const transportTokenVersion = "sntp-native-token-v1"

type TransportTokenVerifyResult struct {
	Valid           bool   `json:"valid"`
	Code            string `json:"code"`
	UID             int    `json:"uid"`
	DeviceID        int    `json:"device_id"`
	DeviceUUID      string `json:"device_uuid"`
	NodeID          string `json:"node_id"`
	ProfileRevision string `json:"profile_revision"`
	ExpiresAt       int64  `json:"expires_at"`
}

type transportTokenPayload struct {
	Version         string `json:"ver"`
	UID             int    `json:"uid"`
	DeviceID        int    `json:"device_id"`
	DeviceUUID      string `json:"device_uuid"`
	NodeID          string `json:"node_id"`
	ProfileRevision string `json:"profile_revision"`
	ExpiresAt       int64  `json:"exp"`
}

type transportTokenVerifyBody struct {
	Data TransportTokenVerifyResult `json:"data"`
}

var transportTokenCache sync.Map

func (c *Client) VerifyTransportToken(ctx context.Context, token string) (*TransportTokenVerifyResult, error) {
	if result, err := c.verifyTransportTokenLocal(token); err == nil {
		return result, nil
	}
	if result := cachedTransportToken(token); result != nil {
		return result, nil
	}

	const path = "/api/v2/server/transport/token/verify"
	body := &transportTokenVerifyBody{}
	r, err := c.client.R().
		SetContext(ctx).
		SetBody(map[string]string{"access_token": token}).
		SetResult(body).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return nil, err
	}
	if r == nil || r.StatusCode() >= 400 {
		return nil, fmt.Errorf("verify transport token failed")
	}
	if body.Data.Valid {
		cacheTransportToken(token, &body.Data)
	}
	return &body.Data, nil
}

func (c *Client) verifyTransportTokenLocal(token string) (*TransportTokenVerifyResult, error) {
	secret := c.AppTransportTokenSecret
	if secret == "" {
		secret = c.Token
	}
	if secret == "" {
		return nil, fmt.Errorf("empty local token secret")
	}

	parts := strings.SplitN(strings.TrimSpace(token), ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid token format")
	}

	signingKey := hmacSHA256([]byte(secret), []byte(transportTokenVersion))
	expected := base64.RawURLEncoding.EncodeToString(
		hmacSHA256(signingKey, []byte(parts[0])),
	)
	if !hmac.Equal([]byte(expected), []byte(parts[1])) {
		return nil, fmt.Errorf("invalid local token signature")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var payload transportTokenPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, err
	}
	if payload.Version != transportTokenVersion {
		return nil, fmt.Errorf("invalid token version")
	}
	if payload.ExpiresAt <= time.Now().Unix() {
		return nil, fmt.Errorf("token expired")
	}

	result := &TransportTokenVerifyResult{
		Valid:           true,
		Code:            "OK_LOCAL",
		UID:             payload.UID,
		DeviceID:        payload.DeviceID,
		DeviceUUID:      payload.DeviceUUID,
		NodeID:          payload.NodeID,
		ProfileRevision: payload.ProfileRevision,
		ExpiresAt:       payload.ExpiresAt,
	}
	cacheTransportToken(token, result)
	return result, nil
}

func hmacSHA256(key, value []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(value)
	return mac.Sum(nil)
}

func cachedTransportToken(token string) *TransportTokenVerifyResult {
	value, ok := transportTokenCache.Load(token)
	if !ok {
		return nil
	}
	result, ok := value.(*TransportTokenVerifyResult)
	if !ok || result == nil || !result.Valid {
		transportTokenCache.Delete(token)
		return nil
	}
	if result.ExpiresAt <= time.Now().Unix()+5 {
		transportTokenCache.Delete(token)
		return nil
	}
	return result
}

func cacheTransportToken(token string, result *TransportTokenVerifyResult) {
	if result == nil || !result.Valid || result.ExpiresAt <= time.Now().Unix()+5 {
		return
	}
	transportTokenCache.Store(token, result)
}
