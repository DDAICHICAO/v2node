package panel

import (
	"context"
	"fmt"
)

type NodeRuntimeStatus struct {
	Hostname                   string   `json:"hostname,omitempty"`
	Interfaces                 []string `json:"interfaces"`
	RxBps                      int64    `json:"rx_bps"`
	TxBps                      int64    `json:"tx_bps"`
	RxBytes                    uint64   `json:"rx_bytes"`
	TxBytes                    uint64   `json:"tx_bytes"`
	SampleInterval             float64  `json:"sample_interval"`
	SampledAt                  int64    `json:"sampled_at"`
	AccessAuditReported        bool     `json:"access_audit_reported,omitempty"`
	AccessAuditEnabled         bool     `json:"access_audit_enabled"`
	AccessAuditEndpoint        string   `json:"access_audit_endpoint,omitempty"`
	AccessAuditTokenConfigured bool     `json:"access_audit_token_configured"`
}

func (c *Client) ReportNodeRuntimeStatus(ctx context.Context, status NodeRuntimeStatus) error {
	const path = "/api/v2/server/status"
	r, err := c.client.R().
		SetContext(ctx).
		SetBody(status).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("received nil response")
	}
	if r.StatusCode() >= 400 {
		return fmt.Errorf("runtime status report failed: status %d", r.StatusCode())
	}
	return nil
}
