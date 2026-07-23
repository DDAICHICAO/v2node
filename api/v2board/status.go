package panel

import (
	"context"
	"fmt"
)

type NodeRuntimeStatus struct {
	Hostname                             string   `json:"hostname,omitempty"`
	MachineInstanceID                    string   `json:"machine_instance_id,omitempty"`
	Interfaces                           []string `json:"interfaces"`
	RxBps                                int64    `json:"rx_bps"`
	TxBps                                int64    `json:"tx_bps"`
	RxBytes                              uint64   `json:"rx_bytes"`
	TxBytes                              uint64   `json:"tx_bytes"`
	SampleInterval                       float64  `json:"sample_interval"`
	SampledAt                            int64    `json:"sampled_at"`
	TLSCertSHA256                        string   `json:"tls_cert_sha256,omitempty"`
	TLSCertFile                          string   `json:"tls_cert_file,omitempty"`
	TLSVerifyPeerCertByName              string   `json:"tls_verify_peer_cert_by_name,omitempty"`
	AccessAuditReported                  bool     `json:"access_audit_reported,omitempty"`
	AccessAuditEnabled                   bool     `json:"access_audit_enabled"`
	AccessAuditEndpoint                  string   `json:"access_audit_endpoint,omitempty"`
	AccessAuditTokenConfigured           bool     `json:"access_audit_token_configured"`
	AccessAuditLastSuccessAt             int64    `json:"access_audit_last_success_at,omitempty"`
	AccessAuditLastErrorAt               int64    `json:"access_audit_last_error_at,omitempty"`
	AccessAuditLastErrorCode             string   `json:"access_audit_last_error_code,omitempty"`
	AccessAuditRetryCount                uint64   `json:"access_audit_retry_count"`
	AccessAuditPendingEvents             uint64   `json:"access_audit_pending_events"`
	AccessAuditPendingBytes              uint64   `json:"access_audit_pending_bytes"`
	AccessAuditOldestEventAt             int64    `json:"access_audit_oldest_event_at,omitempty"`
	AccessAuditDroppedEvents             uint64   `json:"access_audit_dropped_events"`
	AccessAuditDroppedBytes              uint64   `json:"access_audit_dropped_bytes"`
	AccessAuditDroppedEventFrom          int64    `json:"access_audit_dropped_event_from,omitempty"`
	AccessAuditDroppedEventTo            int64    `json:"access_audit_dropped_event_to,omitempty"`
	AccessAuditRejectedEvents            uint64   `json:"access_audit_rejected_events"`
	AccessAuditRejectedBytes             uint64   `json:"access_audit_rejected_bytes"`
	AccessAuditRejectedEventFrom         int64    `json:"access_audit_rejected_event_from,omitempty"`
	AccessAuditRejectedEventTo           int64    `json:"access_audit_rejected_event_to,omitempty"`
	AccessAuditPersistenceFailures       uint64   `json:"access_audit_persistence_failures"`
	AccessAuditPersistenceFailureBytes   uint64   `json:"access_audit_persistence_failure_bytes"`
	AccessAuditPersistenceFailureFrom    int64    `json:"access_audit_persistence_failure_from,omitempty"`
	AccessAuditPersistenceFailureTo      int64    `json:"access_audit_persistence_failure_to,omitempty"`
	AccessAuditPersistQueueDepth         uint64   `json:"access_audit_persist_queue_depth"`
	AccessAuditPersistQueueHighWatermark uint64   `json:"access_audit_persist_queue_high_watermark"`
	AccessAuditPersistTimeouts           uint64   `json:"access_audit_persist_timeouts"`
	AccessAuditLastMigrationAt           int64    `json:"access_audit_last_migration_at,omitempty"`
	AccessAuditLastMigrationRecords      uint64   `json:"access_audit_last_migration_records"`
	AccessAuditLastMigrationMillis       int64    `json:"access_audit_last_migration_millis"`
	FlowTrafficConfigReported            bool     `json:"flow_traffic_config_reported,omitempty"`
	FlowTrafficEnabled                   bool     `json:"flow_traffic_enabled"`
	FlowTrafficLastSuccessAt             int64    `json:"flow_traffic_last_success_at,omitempty"`
	FlowTrafficLastErrorAt               int64    `json:"flow_traffic_last_error_at,omitempty"`
	FlowTrafficLastErrorCode             string   `json:"flow_traffic_last_error_code,omitempty"`
	FlowTrafficRetryCount                uint64   `json:"flow_traffic_retry_count"`
	FlowTrafficPendingEvents             uint64   `json:"flow_traffic_pending_events"`
	FlowTrafficPendingBytes              uint64   `json:"flow_traffic_pending_bytes"`
	FlowTrafficOldestEventAt             int64    `json:"flow_traffic_oldest_event_at,omitempty"`
	FlowTrafficDroppedEvents             uint64   `json:"flow_traffic_dropped_events"`
	FlowTrafficDroppedBytes              uint64   `json:"flow_traffic_dropped_bytes"`
	FlowTrafficDroppedEventFrom          int64    `json:"flow_traffic_dropped_event_from,omitempty"`
	FlowTrafficDroppedEventTo            int64    `json:"flow_traffic_dropped_event_to,omitempty"`
	FlowTrafficRejectedEvents            uint64   `json:"flow_traffic_rejected_events"`
	FlowTrafficRejectedBytes             uint64   `json:"flow_traffic_rejected_bytes"`
	FlowTrafficRejectedEventFrom         int64    `json:"flow_traffic_rejected_event_from,omitempty"`
	FlowTrafficRejectedEventTo           int64    `json:"flow_traffic_rejected_event_to,omitempty"`
	FlowTrafficPersistenceFailures       uint64   `json:"flow_traffic_persistence_failures"`
	FlowTrafficPersistenceFailureBytes   uint64   `json:"flow_traffic_persistence_failure_bytes"`
	FlowTrafficPersistenceFailureFrom    int64    `json:"flow_traffic_persistence_failure_from,omitempty"`
	FlowTrafficPersistenceFailureTo      int64    `json:"flow_traffic_persistence_failure_to,omitempty"`
	FlowTrafficPersistQueueDepth         uint64   `json:"flow_traffic_persist_queue_depth"`
	FlowTrafficPersistQueueHighWatermark uint64   `json:"flow_traffic_persist_queue_high_watermark"`
	FlowTrafficPersistTimeouts           uint64   `json:"flow_traffic_persist_timeouts"`
	FlowTrafficLastMigrationAt           int64    `json:"flow_traffic_last_migration_at,omitempty"`
	FlowTrafficLastMigrationRecords      uint64   `json:"flow_traffic_last_migration_records"`
	FlowTrafficLastMigrationMillis       int64    `json:"flow_traffic_last_migration_millis"`
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
