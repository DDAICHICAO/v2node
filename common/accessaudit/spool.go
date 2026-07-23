package accessaudit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
)

var ErrSpoolOpen = errors.New("access audit spool open failed")

const (
	defaultFlowSpoolMaxBytes = int64(256 << 20)
	defaultFlowSpoolMaxAge   = 24 * time.Hour
)

type SpoolConfig struct {
	Path                    string
	MaxBytes                int64
	MaxAge                  time.Duration
	Now                     func() time.Time
	MkdirAll                func(string, os.FileMode) error
	MigrationRecordObserver func()
}

type SpoolItem struct {
	Key        uint64
	Event      FlowEvent
	EnqueuedAt time.Time
}

type SpoolStats struct {
	PendingEvents           uint64 `json:"pending_events"`
	PendingBytes            uint64 `json:"pending_bytes"`
	DroppedEvents           uint64 `json:"dropped_events"`
	DroppedBytes            uint64 `json:"dropped_bytes"`
	RejectedEvents          uint64 `json:"rejected_events"`
	RejectedBytes           uint64 `json:"rejected_bytes"`
	PersistenceFailures     uint64 `json:"persistence_failures"`
	PersistenceFailureBytes uint64 `json:"persistence_failure_bytes"`
	DroppedEventFrom        int64  `json:"dropped_event_from"`
	DroppedEventTo          int64  `json:"dropped_event_to"`
	RejectedEventFrom       int64  `json:"rejected_event_from"`
	RejectedEventTo         int64  `json:"rejected_event_to"`
	PersistenceFailureFrom  int64  `json:"persistence_failure_from"`
	PersistenceFailureTo    int64  `json:"persistence_failure_to"`
	OldestEventAt           int64  `json:"oldest_event_at"`
	LastMigrationAt         int64  `json:"last_migration_at"`
	LastMigrationRecords    uint64 `json:"last_migration_records"`
	LastMigrationMillis     int64  `json:"last_migration_millis"`
}

type FlowSpool interface {
	Enqueue(FlowEvent) error
	EnqueueBatch([]FlowEvent) error
	Peek(limit int) ([]SpoolItem, error)
	Ack(keys []uint64) error
	Reject(keys []uint64) error
	Stats() (SpoolStats, error)
	RecordPersistenceFailure(eventAt time.Time, estimatedBytes uint64)
	Close() error
}

type BoltFlowSpool struct {
	core *boltSpool[FlowEvent]
}

type storedFlowEvent = storedRecord[FlowEvent]

func NewBoltFlowSpool(config SpoolConfig) (*BoltFlowSpool, error) {
	if config.MaxBytes <= 0 {
		config.MaxBytes = defaultFlowSpoolMaxBytes
	}
	if config.MaxAge <= 0 {
		config.MaxAge = defaultFlowSpoolMaxAge
	}
	core, err := openBoltSpool(config, spoolCodec[FlowEvent]{
		Normalize: func(event *FlowEvent, now time.Time) error {
			return event.Normalize(now)
		},
		EventTime: func(event FlowEvent) time.Time {
			return event.EventTime
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: flow: %v", ErrSpoolOpen, err)
	}
	return &BoltFlowSpool{core: core}, nil
}

func (s *BoltFlowSpool) Enqueue(event FlowEvent) error {
	return s.EnqueueBatch([]FlowEvent{event})
}

func (s *BoltFlowSpool) EnqueueBatch(events []FlowEvent) error {
	return s.core.enqueueBatch(events)
}

func (s *BoltFlowSpool) Peek(limit int) ([]SpoolItem, error) {
	persisted, err := s.core.peek(limit)
	if err != nil {
		return nil, err
	}
	items := make([]SpoolItem, 0, len(persisted))
	for _, item := range persisted {
		items = append(items, SpoolItem{
			Key:        item.Key,
			Event:      item.Event,
			EnqueuedAt: item.EnqueuedAt,
		})
	}
	return items, nil
}

func (s *BoltFlowSpool) Ack(keys []uint64) error {
	return s.core.ack(keys)
}

func (s *BoltFlowSpool) Reject(keys []uint64) error {
	return s.core.reject(keys)
}

func (s *BoltFlowSpool) Stats() (SpoolStats, error) {
	return s.core.stats()
}

func (s *BoltFlowSpool) RecordPersistenceFailure(eventAt time.Time, estimatedBytes uint64) {
	s.core.recordPersistenceFailure(eventAt, estimatedBytes)
}

func (s *BoltFlowSpool) Close() error {
	return s.core.close()
}

func updateTimeRange(from, to *int64, eventAt int64) {
	if eventAt <= 0 {
		return
	}
	if *from == 0 || eventAt < *from {
		*from = eventAt
	}
	if eventAt > *to {
		*to = eventAt
	}
}

func uint64Key(value uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, value)
	return key
}
