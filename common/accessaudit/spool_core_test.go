package accessaudit

import (
	"encoding/binary"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestBoltFlowSpoolMigratesLegacyStatsOnceAndKeepsFIFO(t *testing.T) {
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "flow.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		pending, err := tx.CreateBucketIfNotExists([]byte("pending"))
		if err != nil {
			return err
		}
		if _, err = tx.CreateBucketIfNotExists([]byte("meta")); err != nil {
			return err
		}
		for sequence := uint32(1); sequence <= 2; sequence++ {
			record := storedFlowEvent{Event: flowEventForTest(sequence, now), EnqueuedAt: now}
			encoded, err := json.Marshal(record)
			if err != nil {
				return err
			}
			record.Size = uint64(len(encoded))
			encoded, err = json.Marshal(record)
			if err != nil {
				return err
			}
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, uint64(sequence))
			if err := pending.Put(key, encoded); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var migrationScans atomic.Uint64
	config := SpoolConfig{
		Path: path, MaxBytes: 1 << 20, MaxAge: time.Hour,
		Now:                     func() time.Time { return now },
		MigrationRecordObserver: func() { migrationScans.Add(1) },
	}
	spool, err := NewBoltFlowSpool(config)
	if err != nil {
		t.Fatalf("open legacy spool: %v", err)
	}
	stats, err := spool.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingEvents != 2 || stats.PendingBytes == 0 || migrationScans.Load() != 2 {
		t.Fatalf("unexpected migrated state: stats=%#v scans=%d", stats, migrationScans.Load())
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}

	migrationScans.Store(0)
	spool, err = NewBoltFlowSpool(config)
	if err != nil {
		t.Fatalf("reopen migrated spool: %v", err)
	}
	defer spool.Close()
	if err := spool.EnqueueBatch([]FlowEvent{
		flowEventForTest(3, now),
		flowEventForTest(4, now),
	}); err != nil {
		t.Fatalf("enqueue batch: %v", err)
	}
	stats, err = spool.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if migrationScans.Load() != 0 {
		t.Fatalf("steady-state open scanned %d records", migrationScans.Load())
	}
	if stats.PendingEvents != 4 || stats.PendingBytes == 0 {
		t.Fatalf("unexpected incremental stats: %#v", stats)
	}
	items, err := spool.Peek(4)
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range items {
		if item.Event.Sequence != uint32(i+1) {
			t.Fatalf("FIFO changed at %d: %#v", i, items)
		}
	}
}
