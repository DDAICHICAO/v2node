package accessaudit

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const spoolSchemaVersion uint64 = 1

var (
	spoolPendingBucket    = []byte("pending")
	spoolMetaBucket       = []byte("meta")
	spoolStatsKey         = []byte("stats")
	spoolSchemaVersionKey = []byte("schema_version")
)

type storedRecord[T any] struct {
	Event      T         `json:"event"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	Size       uint64    `json:"size"`
}

type spoolCodec[T any] struct {
	Normalize func(*T, time.Time) error
	EventTime func(T) time.Time
}

type persistedItem[T any] struct {
	Key        uint64
	Event      T
	EnqueuedAt time.Time
}

type boltSpool[T any] struct {
	db                      *bolt.DB
	maxBytes                int64
	maxAge                  time.Duration
	now                     func() time.Time
	codec                   spoolCodec[T]
	migrationRecordObserver func()

	gapMu      sync.Mutex
	pendingGap SpoolStats
}

func openBoltSpool[T any](config SpoolConfig, codec spoolCodec[T]) (*boltSpool[T], error) {
	if config.Path == "" {
		return nil, errors.New("spool path is required")
	}
	if config.MaxBytes <= 0 {
		return nil, errors.New("spool max bytes must be positive")
	}
	if config.MaxAge <= 0 {
		return nil, errors.New("spool max age must be positive")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MkdirAll == nil {
		config.MkdirAll = os.MkdirAll
	}
	if err := config.MkdirAll(filepath.Dir(config.Path), 0o700); err != nil {
		return nil, fmt.Errorf("create spool directory: %w", err)
	}
	db, err := bolt.Open(config.Path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open spool: %w", err)
	}
	spool := &boltSpool[T]{
		db:                      db,
		maxBytes:                config.MaxBytes,
		maxAge:                  config.MaxAge,
		now:                     config.Now,
		codec:                   codec,
		migrationRecordObserver: config.MigrationRecordObserver,
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(spoolPendingBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(spoolMetaBucket); err != nil {
			return err
		}
		if err := spool.migrate(tx); err != nil {
			return fmt.Errorf("%w: %v", ErrSpoolMigration, err)
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize spool: %w", err)
	}
	return spool, nil
}

func (s *boltSpool[T]) migrate(tx *bolt.Tx) error {
	meta := tx.Bucket(spoolMetaBucket)
	if decodeUint64(meta.Get(spoolSchemaVersionKey)) == spoolSchemaVersion {
		return nil
	}
	started := time.Now()
	stats, err := readSpoolStats(tx)
	if err != nil {
		return err
	}
	stats.PendingEvents = 0
	stats.PendingBytes = 0
	stats.OldestEventAt = 0
	stats.LastMigrationRecords = 0

	pending := tx.Bucket(spoolPendingBucket)
	var maxKey uint64
	cursor := pending.Cursor()
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		record, err := decodeStoredRecord[T](value)
		if err != nil {
			return fmt.Errorf("migrate pending key %x: %w", key, err)
		}
		stats.PendingEvents++
		stats.PendingBytes += record.Size
		updateOldest(&stats.OldestEventAt, s.codec.EventTime(record.Event).Unix())
		stats.LastMigrationRecords++
		if len(key) == 8 {
			current := binary.BigEndian.Uint64(key)
			if current > maxKey {
				maxKey = current
			}
		}
		if s.migrationRecordObserver != nil {
			s.migrationRecordObserver()
		}
	}
	if pending.Sequence() < maxKey {
		if err := pending.SetSequence(maxKey); err != nil {
			return err
		}
	}
	stats.LastMigrationAt = s.now().Unix()
	stats.LastMigrationMillis = time.Since(started).Milliseconds()
	if err := writeSpoolStats(tx, stats); err != nil {
		return err
	}
	return meta.Put(spoolSchemaVersionKey, encodeUint64(spoolSchemaVersion))
}

func (s *boltSpool[T]) enqueueBatch(events []T) error {
	if len(events) == 0 {
		return nil
	}
	now := s.now()
	normalized := append([]T(nil), events...)
	for index := range normalized {
		if err := s.codec.Normalize(&normalized[index], now); err != nil {
			return err
		}
	}
	gap := s.takePendingGap()
	err := s.db.Update(func(tx *bolt.Tx) error {
		stats, err := readSpoolStats(tx)
		if err != nil {
			return err
		}
		mergePersistenceGap(&stats, gap)
		if _, err := s.pruneExpired(tx, &stats, now); err != nil {
			return err
		}
		pending := tx.Bucket(spoolPendingBucket)
		for _, event := range normalized {
			key, err := pending.NextSequence()
			if err != nil {
				return err
			}
			record := storedRecord[T]{Event: event, EnqueuedAt: now.UTC()}
			encoded, err := encodeStoredRecord(record)
			if err != nil {
				return err
			}
			record.Size = uint64(len(encoded))
			encoded, err = encodeStoredRecord(record)
			if err != nil {
				return err
			}
			record.Size = uint64(len(encoded))
			encoded, err = encodeStoredRecord(record)
			if err != nil {
				return err
			}
			if err := pending.Put(uint64Key(key), encoded); err != nil {
				return err
			}
			stats.PendingEvents++
			stats.PendingBytes += record.Size
			updateOldest(&stats.OldestEventAt, s.codec.EventTime(event).Unix())
		}
		if err := s.pruneCapacity(tx, &stats); err != nil {
			return err
		}
		return writeSpoolStats(tx, stats)
	})
	if err != nil {
		s.restorePendingGap(gap)
	}
	return err
}

func (s *boltSpool[T]) peek(limit int) ([]persistedItem[T], error) {
	if limit <= 0 {
		return []persistedItem[T]{}, nil
	}
	now := s.now()
	gap := s.takePendingGap()
	err := s.db.Update(func(tx *bolt.Tx) error {
		stats, err := readSpoolStats(tx)
		if err != nil {
			return err
		}
		mergePersistenceGap(&stats, gap)
		changed, err := s.pruneExpired(tx, &stats, now)
		if err != nil {
			return err
		}
		if changed || gap.PersistenceFailures > 0 {
			return writeSpoolStats(tx, stats)
		}
		return nil
	})
	if err != nil {
		s.restorePendingGap(gap)
		return nil, err
	}

	items := make([]persistedItem[T], 0, limit)
	err = s.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(spoolPendingBucket).Cursor()
		for key, value := cursor.First(); key != nil && len(items) < limit; key, value = cursor.Next() {
			record, err := decodeStoredRecord[T](value)
			if err != nil {
				return err
			}
			items = append(items, persistedItem[T]{
				Key:        binary.BigEndian.Uint64(key),
				Event:      record.Event,
				EnqueuedAt: record.EnqueuedAt,
			})
		}
		return nil
	})
	return items, err
}

func (s *boltSpool[T]) ack(keys []uint64) error {
	return s.remove(keys, false)
}

func (s *boltSpool[T]) reject(keys []uint64) error {
	return s.remove(keys, true)
}

func (s *boltSpool[T]) remove(keys []uint64, rejected bool) error {
	if len(keys) == 0 {
		return nil
	}
	gap := s.takePendingGap()
	err := s.db.Update(func(tx *bolt.Tx) error {
		stats, err := readSpoolStats(tx)
		if err != nil {
			return err
		}
		mergePersistenceGap(&stats, gap)
		pending := tx.Bucket(spoolPendingBucket)
		for _, key := range keys {
			encoded := pending.Get(uint64Key(key))
			if encoded == nil {
				continue
			}
			record, err := decodeStoredRecord[T](encoded)
			if err != nil {
				return err
			}
			subPending(&stats, record.Size)
			if rejected {
				stats.RejectedEvents++
				stats.RejectedBytes += record.Size
				updateTimeRange(
					&stats.RejectedEventFrom,
					&stats.RejectedEventTo,
					s.codec.EventTime(record.Event).Unix(),
				)
			}
			if err := pending.Delete(uint64Key(key)); err != nil {
				return err
			}
		}
		if err := s.refreshOldest(tx, &stats); err != nil {
			return err
		}
		return writeSpoolStats(tx, stats)
	})
	if err != nil {
		s.restorePendingGap(gap)
	}
	return err
}

func (s *boltSpool[T]) stats() (SpoolStats, error) {
	var stats SpoolStats
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		stats, err = readSpoolStats(tx)
		return err
	})
	if err != nil {
		return SpoolStats{}, err
	}
	mergePersistenceGap(&stats, s.pendingGapSnapshot())
	return stats, nil
}

func (s *boltSpool[T]) recordPersistenceFailure(eventAt time.Time, estimatedBytes uint64) {
	s.gapMu.Lock()
	defer s.gapMu.Unlock()
	s.pendingGap.PersistenceFailures++
	s.pendingGap.PersistenceFailureBytes += estimatedBytes
	updateTimeRange(
		&s.pendingGap.PersistenceFailureFrom,
		&s.pendingGap.PersistenceFailureTo,
		eventAt.Unix(),
	)
}

func (s *boltSpool[T]) close() error {
	return s.db.Close()
}

func (s *boltSpool[T]) pruneExpired(tx *bolt.Tx, stats *SpoolStats, now time.Time) (bool, error) {
	cutoff := now.Add(-s.maxAge)
	pending := tx.Bucket(spoolPendingBucket)
	cursor := pending.Cursor()
	changed := false
	for key, value := cursor.First(); key != nil; key, value = cursor.First() {
		record, err := decodeStoredRecord[T](value)
		if err != nil {
			return false, err
		}
		if !record.EnqueuedAt.Before(cutoff) {
			break
		}
		s.recordDropped(stats, record)
		if err := cursor.Delete(); err != nil {
			return false, err
		}
		changed = true
	}
	if changed {
		if err := s.refreshOldest(tx, stats); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func (s *boltSpool[T]) pruneCapacity(tx *bolt.Tx, stats *SpoolStats) error {
	if stats.PendingBytes <= uint64(s.maxBytes) {
		return nil
	}
	pending := tx.Bucket(spoolPendingBucket)
	cursor := pending.Cursor()
	for key, value := cursor.First(); key != nil && stats.PendingBytes > uint64(s.maxBytes); key, value = cursor.First() {
		record, err := decodeStoredRecord[T](value)
		if err != nil {
			return err
		}
		s.recordDropped(stats, record)
		if err := cursor.Delete(); err != nil {
			return err
		}
	}
	return s.refreshOldest(tx, stats)
}

func (s *boltSpool[T]) recordDropped(stats *SpoolStats, record storedRecord[T]) {
	subPending(stats, record.Size)
	stats.DroppedEvents++
	stats.DroppedBytes += record.Size
	updateTimeRange(
		&stats.DroppedEventFrom,
		&stats.DroppedEventTo,
		s.codec.EventTime(record.Event).Unix(),
	)
}

func (s *boltSpool[T]) refreshOldest(tx *bolt.Tx, stats *SpoolStats) error {
	_, value := tx.Bucket(spoolPendingBucket).Cursor().First()
	if value == nil {
		stats.OldestEventAt = 0
		return nil
	}
	record, err := decodeStoredRecord[T](value)
	if err != nil {
		return err
	}
	stats.OldestEventAt = s.codec.EventTime(record.Event).Unix()
	return nil
}

func (s *boltSpool[T]) pendingGapSnapshot() SpoolStats {
	s.gapMu.Lock()
	defer s.gapMu.Unlock()
	return s.pendingGap
}

func (s *boltSpool[T]) takePendingGap() SpoolStats {
	s.gapMu.Lock()
	defer s.gapMu.Unlock()
	gap := s.pendingGap
	s.pendingGap = SpoolStats{}
	return gap
}

func (s *boltSpool[T]) restorePendingGap(gap SpoolStats) {
	if gap.PersistenceFailures == 0 {
		return
	}
	s.gapMu.Lock()
	defer s.gapMu.Unlock()
	mergePersistenceGap(&s.pendingGap, gap)
}

func encodeStoredRecord[T any](record storedRecord[T]) ([]byte, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode spool event: %w", err)
	}
	return encoded, nil
}

func decodeStoredRecord[T any](encoded []byte) (storedRecord[T], error) {
	var record storedRecord[T]
	if err := json.Unmarshal(encoded, &record); err != nil {
		return record, fmt.Errorf("decode spool event: %w", err)
	}
	if record.Size == 0 {
		record.Size = uint64(len(encoded))
	}
	return record, nil
}

func readSpoolStats(tx *bolt.Tx) (SpoolStats, error) {
	var stats SpoolStats
	encoded := tx.Bucket(spoolMetaBucket).Get(spoolStatsKey)
	if encoded == nil {
		return stats, nil
	}
	if err := json.Unmarshal(encoded, &stats); err != nil {
		return stats, fmt.Errorf("decode spool stats: %w", err)
	}
	return stats, nil
}

func writeSpoolStats(tx *bolt.Tx, stats SpoolStats) error {
	encoded, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("encode spool stats: %w", err)
	}
	return tx.Bucket(spoolMetaBucket).Put(spoolStatsKey, encoded)
}

func mergePersistenceGap(stats *SpoolStats, gap SpoolStats) {
	stats.PersistenceFailures += gap.PersistenceFailures
	stats.PersistenceFailureBytes += gap.PersistenceFailureBytes
	updateTimeRange(&stats.PersistenceFailureFrom, &stats.PersistenceFailureTo, gap.PersistenceFailureFrom)
	updateTimeRange(&stats.PersistenceFailureFrom, &stats.PersistenceFailureTo, gap.PersistenceFailureTo)
}

func subPending(stats *SpoolStats, size uint64) {
	if stats.PendingEvents > 0 {
		stats.PendingEvents--
	}
	if size <= stats.PendingBytes {
		stats.PendingBytes -= size
	} else {
		stats.PendingBytes = 0
	}
}

func updateOldest(oldest *int64, eventAt int64) {
	if eventAt <= 0 {
		return
	}
	if *oldest == 0 || eventAt < *oldest {
		*oldest = eventAt
	}
}

func encodeUint64(value uint64) []byte {
	encoded := make([]byte, 8)
	binary.BigEndian.PutUint64(encoded, value)
	return encoded
}

func decodeUint64(encoded []byte) uint64 {
	if len(encoded) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(encoded)
}
