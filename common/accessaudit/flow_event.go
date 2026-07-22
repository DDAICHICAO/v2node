package accessaudit

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	FlowEventType        = "flow_traffic"
	FlowSampleCheckpoint = "checkpoint"
	FlowSampleFinal      = "final"
	maxDirectionalBytes  = uint64(1) << 50 // 1 PiB per checkpoint/final delta.
)

// FlowEvent is one idempotent traffic delta for a routed proxy session.
// UploadBytes and DownloadBytes are interval deltas rather than cumulative
// counters, so replaying the same EventID is safe for downstream aggregation.
type FlowEvent struct {
	EventID           string    `json:"event_id"`
	SessionID         string    `json:"session_id"`
	Sequence          uint32    `json:"sequence"`
	SampleType        string    `json:"sample_type"`
	EventTime         time.Time `json:"event_time"`
	IntervalStartedAt time.Time `json:"interval_started_at"`
	NodeID            uint32    `json:"node_id"`
	NodeTag           string    `json:"node_tag,omitempty"`
	UID               uint64    `json:"uid"`
	UUID              string    `json:"uuid,omitempty"`
	SourceIP          string    `json:"source_ip,omitempty"`
	TargetHost        string    `json:"target_host"`
	TargetPort        uint16    `json:"target_port"`
	Network           string    `json:"network"`
	InboundTag        string    `json:"inbound_tag,omitempty"`
	OutboundTag       string    `json:"outbound_tag,omitempty"`
	UploadBytes       uint64    `json:"upload_bytes"`
	DownloadBytes     uint64    `json:"download_bytes"`
	DurationMS        uint64    `json:"duration_ms"`
	Completed         bool      `json:"completed"`
}

// Normalize validates the event and derives fields that must remain stable
// across retries. now is intentionally not used in EventID generation.
func (e *FlowEvent) Normalize(now time.Time) error {
	_ = now
	if e == nil {
		return errors.New("flow event is nil")
	}

	e.SessionID = strings.TrimSpace(e.SessionID)
	if e.NodeID == 0 {
		return errors.New("node_id is required")
	}
	if e.SessionID == "" {
		return errors.New("session_id is required")
	}
	if e.Sequence == 0 {
		return errors.New("sequence is required")
	}
	if e.EventTime.IsZero() {
		return errors.New("event_time is required")
	}
	if e.IntervalStartedAt.IsZero() {
		return errors.New("interval_started_at is required")
	}
	if e.EventTime.Before(e.IntervalStartedAt) {
		return errors.New("event_time cannot precede interval_started_at")
	}
	if _, err := e.TotalBytes(); err != nil {
		return err
	}

	switch e.SampleType {
	case FlowSampleCheckpoint:
		e.Completed = false
	case FlowSampleFinal:
		e.Completed = true
	default:
		return fmt.Errorf("unsupported sample_type %q", e.SampleType)
	}

	e.EventID = fmt.Sprintf("%d:%s:%08d", e.NodeID, e.SessionID, e.Sequence)
	return nil
}

func (e FlowEvent) TotalBytes() (uint64, error) {
	if e.UploadBytes > maxDirectionalBytes || e.DownloadBytes > maxDirectionalBytes {
		return 0, errors.New("directional traffic delta exceeds 1 PiB")
	}
	if ^uint64(0)-e.UploadBytes < e.DownloadBytes {
		return 0, errors.New("upload_bytes + download_bytes overflows uint64")
	}
	return e.UploadBytes + e.DownloadBytes, nil
}
