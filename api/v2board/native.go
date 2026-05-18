package panel

import (
	"context"
	"fmt"
)

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

type transportTokenVerifyBody struct {
	Data TransportTokenVerifyResult `json:"data"`
}

func (c *Client) VerifyTransportToken(ctx context.Context, token string) (*TransportTokenVerifyResult, error) {
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
	return &body.Data, nil
}
