package panel

// NIC telemetry is display-only and is deliberately separate from user traffic.
type NodeTelemetryMessage struct {
	Type           string  `json:"type"`
	Seq            int64   `json:"seq"`
	RxBps          int64   `json:"rx_bps"`
	TxBps          int64   `json:"tx_bps"`
	RxBytes        uint64  `json:"rx_bytes"`
	TxBytes        uint64  `json:"tx_bytes"`
	SampleInterval float64 `json:"sample_interval"`
	SampledAt      int64   `json:"sampled_at"`
}

func (c *UserSyncWakeupConnection) SendTelemetry(message NodeTelemetryMessage) error {
	message.Type = "node_telemetry"
	return c.writeJSON(message)
}
