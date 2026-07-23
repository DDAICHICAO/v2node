package accessaudit

import (
	"fmt"
	"time"
)

const (
	defaultAccessSpoolMaxBytes = int64(1 << 30)
	defaultAccessSpoolMaxAge   = 7 * 24 * time.Hour
	defaultAccessSpoolPath     = "/var/lib/v2node/access-audit-spool/access.db"
)

type AccessSpoolItem struct {
	Key        uint64
	Event      Event
	EnqueuedAt time.Time
}

type AccessSpool interface {
	EnqueueBatch([]Event) error
	Peek(int) ([]AccessSpoolItem, error)
	Ack([]uint64) error
	Reject([]uint64) error
	Stats() (SpoolStats, error)
	RecordPersistenceFailure(time.Time, uint64)
	Close() error
}

type BoltAccessSpool struct {
	core *boltSpool[Event]
}

func NewBoltAccessSpool(config SpoolConfig) (*BoltAccessSpool, error) {
	if config.Path == "" {
		config.Path = defaultAccessSpoolPath
	}
	if config.MaxBytes <= 0 {
		config.MaxBytes = defaultAccessSpoolMaxBytes
	}
	if config.MaxAge <= 0 {
		config.MaxAge = defaultAccessSpoolMaxAge
	}
	core, err := openBoltSpool(config, spoolCodec[Event]{
		Normalize: normalizeAccessEvent,
		EventTime: func(event Event) time.Time {
			return event.EventTime
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: access: %v", ErrSpoolOpen, err)
	}
	return &BoltAccessSpool{core: core}, nil
}

func (s *BoltAccessSpool) EnqueueBatch(events []Event) error {
	return s.core.enqueueBatch(events)
}

func (s *BoltAccessSpool) Peek(limit int) ([]AccessSpoolItem, error) {
	persisted, err := s.core.peek(limit)
	if err != nil {
		return nil, err
	}
	items := make([]AccessSpoolItem, 0, len(persisted))
	for _, item := range persisted {
		items = append(items, AccessSpoolItem{
			Key:        item.Key,
			Event:      item.Event,
			EnqueuedAt: item.EnqueuedAt,
		})
	}
	return items, nil
}

func (s *BoltAccessSpool) Ack(keys []uint64) error {
	return s.core.ack(keys)
}

func (s *BoltAccessSpool) Reject(keys []uint64) error {
	return s.core.reject(keys)
}

func (s *BoltAccessSpool) Stats() (SpoolStats, error) {
	return s.core.stats()
}

func (s *BoltAccessSpool) RecordPersistenceFailure(eventAt time.Time, estimatedBytes uint64) {
	s.core.recordPersistenceFailure(eventAt, estimatedBytes)
}

func (s *BoltAccessSpool) Close() error {
	return s.core.close()
}
