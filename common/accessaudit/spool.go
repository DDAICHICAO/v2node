package accessaudit

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	defaultFlowSpoolMaxBytes = int64(256 << 20)
	defaultFlowSpoolMaxAge   = 24 * time.Hour
)

var (
	flowPendingBucket = []byte("pending")
	flowMetaBucket    = []byte("meta")
	flowStatsKey      = []byte("stats")
)

type SpoolConfig struct {
	Path     string
	MaxBytes int64
	MaxAge   time.Duration
	Now      func() time.Time
}

type SpoolItem struct {
	Key        uint64
	Event      FlowEvent
	EnqueuedAt time.Time
}

type SpoolStats struct {
	PendingEvents     uint64 `json:"pending_events"`
	PendingBytes      uint64 `json:"pending_bytes"`
	DroppedEvents     uint64 `json:"dropped_events"`
	DroppedBytes      uint64 `json:"dropped_bytes"`
	RejectedEvents    uint64 `json:"rejected_events"`
	RejectedBytes     uint64 `json:"rejected_bytes"`
	DroppedEventFrom  int64  `json:"dropped_event_from"`
	DroppedEventTo    int64  `json:"dropped_event_to"`
	RejectedEventFrom int64  `json:"rejected_event_from"`
	RejectedEventTo   int64  `json:"rejected_event_to"`
	OldestEventAt     int64  `json:"oldest_event_at"`
}

type FlowSpool interface {
	Enqueue(FlowEvent) error
	Peek(limit int) ([]SpoolItem, error)
	Ack(keys []uint64) error
	Reject(keys []uint64) error
	Stats() (SpoolStats, error)
	Close() error
}

type BoltFlowSpool struct {
	db       *bolt.DB
	maxBytes int64
	maxAge   time.Duration
	now      func() time.Time
}

type storedFlowEvent struct {
	Event      FlowEvent `json:"event"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	Size       uint64    `json:"size"`
}

func NewBoltFlowSpool(config SpoolConfig) (*BoltFlowSpool, error) {
	if config.Path == "" {
		return nil, errors.New("flow spool path is required")
	}
	if config.MaxBytes <= 0 {
		config.MaxBytes = defaultFlowSpoolMaxBytes
	}
	if config.MaxAge <= 0 {
		config.MaxAge = defaultFlowSpoolMaxAge
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(config.Path), 0o700); err != nil {
		return nil, fmt.Errorf("create flow spool directory: %w", err)
	}
	db, err := bolt.Open(config.Path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open flow spool: %w", err)
	}
	spool := &BoltFlowSpool{
		db:       db,
		maxBytes: config.MaxBytes,
		maxAge:   config.MaxAge,
		now:      config.Now,
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(flowPendingBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(flowMetaBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize flow spool: %w", err)
	}
	return spool, nil
}

func (s *BoltFlowSpool) Enqueue(event FlowEvent) error {
	if err := event.Normalize(s.now()); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := s.pruneExpired(tx); err != nil {
			return err
		}
		pending := tx.Bucket(flowPendingBucket)
		key, err := pending.NextSequence()
		if err != nil {
			return err
		}
		record := storedFlowEvent{Event: event, EnqueuedAt: s.now().UTC()}
		encoded, err := encodeStoredFlowEvent(record)
		if err != nil {
			return err
		}
		record.Size = uint64(len(encoded))
		encoded, err = encodeStoredFlowEvent(record)
		if err != nil {
			return err
		}
		record.Size = uint64(len(encoded))
		encoded, err = encodeStoredFlowEvent(record)
		if err != nil {
			return err
		}
		if err := pending.Put(uint64Key(key), encoded); err != nil {
			return err
		}
		return s.pruneCapacity(tx)
	})
}

func (s *BoltFlowSpool) Peek(limit int) ([]SpoolItem, error) {
	if limit <= 0 {
		return []SpoolItem{}, nil
	}
	if err := s.db.Update(s.pruneExpired); err != nil {
		return nil, err
	}
	items := make([]SpoolItem, 0, limit)
	err := s.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(flowPendingBucket).Cursor()
		for key, value := cursor.First(); key != nil && len(items) < limit; key, value = cursor.Next() {
			record, err := decodeStoredFlowEvent(value)
			if err != nil {
				return err
			}
			items = append(items, SpoolItem{
				Key:        binary.BigEndian.Uint64(key),
				Event:      record.Event,
				EnqueuedAt: record.EnqueuedAt,
			})
		}
		return nil
	})
	return items, err
}

func (s *BoltFlowSpool) Ack(keys []uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		pending := tx.Bucket(flowPendingBucket)
		for _, key := range keys {
			if err := pending.Delete(uint64Key(key)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *BoltFlowSpool) Reject(keys []uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		pending := tx.Bucket(flowPendingBucket)
		stats, err := readPersistentStats(tx)
		if err != nil {
			return err
		}
		for _, key := range keys {
			encoded := pending.Get(uint64Key(key))
			if encoded == nil {
				continue
			}
			record, err := decodeStoredFlowEvent(encoded)
			if err != nil {
				return err
			}
			stats.RejectedEvents++
			stats.RejectedBytes += record.Size
			updateTimeRange(&stats.RejectedEventFrom, &stats.RejectedEventTo, record.Event.EventTime.Unix())
			if err := pending.Delete(uint64Key(key)); err != nil {
				return err
			}
		}
		return writePersistentStats(tx, stats)
	})
}

func (s *BoltFlowSpool) Stats() (SpoolStats, error) {
	if err := s.db.Update(s.pruneExpired); err != nil {
		return SpoolStats{}, err
	}
	var stats SpoolStats
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		stats, err = readPersistentStats(tx)
		if err != nil {
			return err
		}
		cursor := tx.Bucket(flowPendingBucket).Cursor()
		for _, value := cursor.First(); value != nil; _, value = cursor.Next() {
			record, err := decodeStoredFlowEvent(value)
			if err != nil {
				return err
			}
			stats.PendingEvents++
			stats.PendingBytes += record.Size
			eventAt := record.Event.EventTime.Unix()
			if stats.OldestEventAt == 0 || eventAt < stats.OldestEventAt {
				stats.OldestEventAt = eventAt
			}
		}
		return nil
	})
	return stats, err
}

func (s *BoltFlowSpool) Close() error {
	return s.db.Close()
}

func (s *BoltFlowSpool) pruneExpired(tx *bolt.Tx) error {
	cutoff := s.now().Add(-s.maxAge)
	pending := tx.Bucket(flowPendingBucket)
	stats, err := readPersistentStats(tx)
	if err != nil {
		return err
	}
	changed := false
	cursor := pending.Cursor()
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		record, err := decodeStoredFlowEvent(value)
		if err != nil {
			return err
		}
		if !record.EnqueuedAt.Before(cutoff) {
			continue
		}
		stats.DroppedEvents++
		stats.DroppedBytes += record.Size
		updateTimeRange(&stats.DroppedEventFrom, &stats.DroppedEventTo, record.Event.EventTime.Unix())
		if err := cursor.Delete(); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return writePersistentStats(tx, stats)
	}
	return nil
}

func (s *BoltFlowSpool) pruneCapacity(tx *bolt.Tx) error {
	pending := tx.Bucket(flowPendingBucket)
	var total uint64
	cursor := pending.Cursor()
	for _, value := cursor.First(); value != nil; _, value = cursor.Next() {
		record, err := decodeStoredFlowEvent(value)
		if err != nil {
			return err
		}
		total += record.Size
	}
	if total <= uint64(s.maxBytes) {
		return nil
	}
	stats, err := readPersistentStats(tx)
	if err != nil {
		return err
	}
	for key, value := cursor.First(); key != nil && total > uint64(s.maxBytes); key, value = cursor.Next() {
		record, err := decodeStoredFlowEvent(value)
		if err != nil {
			return err
		}
		stats.DroppedEvents++
		stats.DroppedBytes += record.Size
		updateTimeRange(&stats.DroppedEventFrom, &stats.DroppedEventTo, record.Event.EventTime.Unix())
		if record.Size <= total {
			total -= record.Size
		} else {
			total = 0
		}
		if err := cursor.Delete(); err != nil {
			return err
		}
	}
	return writePersistentStats(tx, stats)
}

func encodeStoredFlowEvent(record storedFlowEvent) ([]byte, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode flow spool event: %w", err)
	}
	return encoded, nil
}

func decodeStoredFlowEvent(encoded []byte) (storedFlowEvent, error) {
	var record storedFlowEvent
	if err := json.Unmarshal(encoded, &record); err != nil {
		return record, fmt.Errorf("decode flow spool event: %w", err)
	}
	if record.Size == 0 {
		record.Size = uint64(len(encoded))
	}
	return record, nil
}

func readPersistentStats(tx *bolt.Tx) (SpoolStats, error) {
	var stats SpoolStats
	encoded := tx.Bucket(flowMetaBucket).Get(flowStatsKey)
	if encoded == nil {
		return stats, nil
	}
	if err := json.Unmarshal(encoded, &stats); err != nil {
		return stats, fmt.Errorf("decode flow spool stats: %w", err)
	}
	stats.PendingEvents = 0
	stats.PendingBytes = 0
	stats.OldestEventAt = 0
	return stats, nil
}

func writePersistentStats(tx *bolt.Tx, stats SpoolStats) error {
	stats.PendingEvents = 0
	stats.PendingBytes = 0
	stats.OldestEventAt = 0
	encoded, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	return tx.Bucket(flowMetaBucket).Put(flowStatsKey, encoded)
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
