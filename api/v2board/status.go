package panel

import (
	"context"
	"fmt"
)

type NodeRuntimeStatus struct {
	Hostname                     string   `json:"hostname,omitempty"`
	MachineInstanceID            string   `json:"machine_instance_id,omitempty"`
	Interfaces                   []string `json:"interfaces"`
	RxBps                        int64    `json:"rx_bps"`
	TxBps                        int64    `json:"tx_bps"`
	RxBytes                      uint64   `json:"rx_bytes"`
	TxBytes                      uint64   `json:"tx_bytes"`
	SampleInterval               float64  `json:"sample_interval"`
	SampledAt                    int64    `json:"sampled_at"`
	TLSCertSHA256                string   `json:"tls_cert_sha256,omitempty"`
	TLSCertFile                  string   `json:"tls_cert_file,omitempty"`
	TLSVerifyPeerCertByName      string   `json:"tls_verify_peer_cert_by_name,omitempty"`
	AccessAuditReported          bool     `json:"access_audit_reported,omitempty"`
	AccessAuditEnabled           bool     `json:"access_audit_enabled"`
	AccessAuditEndpoint          string   `json:"access_audit_endpoint,omitempty"`
	AccessAuditTokenConfigured   bool     `json:"access_audit_token_configured"`
	FlowTrafficConfigReported    bool     `json:"flow_traffic_config_reported,omitempty"`
	FlowTrafficEnabled           bool     `json:"flow_traffic_enabled"`
	FlowTrafficLastSuccessAt     int64    `json:"flow_traffic_last_success_at,omitempty"`
	FlowTrafficLastErrorAt       int64    `json:"flow_traffic_last_error_at,omitempty"`
	FlowTrafficLastErrorCode     string   `json:"flow_traffic_last_error_code,omitempty"`
	FlowTrafficRetryCount        uint64   `json:"flow_traffic_retry_count"`
	FlowTrafficPendingEvents     uint64   `json:"flow_traffic_pending_events"`
	FlowTrafficPendingBytes      uint64   `json:"flow_traffic_pending_bytes"`
	FlowTrafficOldestEventAt     int64    `json:"flow_traffic_oldest_event_at,omitempty"`
	FlowTrafficDroppedEvents     uint64   `json:"flow_traffic_dropped_events"`
	FlowTrafficDroppedBytes      uint64   `json:"flow_traffic_dropped_bytes"`
	FlowTrafficDroppedEventFrom  int64    `json:"flow_traffic_dropped_event_from,omitempty"`
	FlowTrafficDroppedEventTo    int64    `json:"flow_traffic_dropped_event_to,omitempty"`
	FlowTrafficRejectedEvents    uint64   `json:"flow_traffic_rejected_events"`
	FlowTrafficRejectedBytes     uint64   `json:"flow_traffic_rejected_bytes"`
	FlowTrafficRejectedEventFrom int64    `json:"flow_traffic_rejected_event_from,omitempty"`
	FlowTrafficRejectedEventTo   int64    `json:"flow_traffic_rejected_event_to,omitempty"`
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
