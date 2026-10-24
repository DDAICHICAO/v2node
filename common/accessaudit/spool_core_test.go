package accessaudit

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestBoltSpoolRecoversCommittedEventsWithoutClose(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	for _, kind := range []string{"access", "flow"} {
		if child := os.Getenv("SNTP_SPOOL_EXIT_TEST"); child != "" && child != kind {
			continue
		}
		t.Run(kind, func(t *testing.T) {
			if kind == "access" {
				checkSpoolExitRecovery(t, kind, now, func(config SpoolConfig) (*boltSpool[Event], error) {
					s, err := NewBoltAccessSpool(config)
					if err != nil {
						return nil, err
					}
					return s.core, nil
				}, []Event{accessEventForTest("one", now), accessEventForTest("two", now), accessEventForTest("three", now), accessEventForTest("four", now), accessEventForTest("five", now)})
			} else {
				checkSpoolExitRecovery(t, kind, now, func(config SpoolConfig) (*boltSpool[FlowEvent], error) {
					s, err := NewBoltFlowSpool(config)
					if err != nil {
						return nil, err
					}
					return s.core, nil
				}, []FlowEvent{flowEventForTest(1, now), flowEventForTest(2, now), flowEventForTest(3, now), flowEventForTest(4, now), flowEventForTest(5, now)})
			}
		})
	}
}

func checkSpoolExitRecovery[T any](t *testing.T, kind string, now time.Time, open func(SpoolConfig) (*boltSpool[T], error), events []T) {
	t.Helper()
	child := os.Getenv("SNTP_SPOOL_EXIT_TEST") != ""
	path := os.Getenv("SNTP_SPOOL_EXIT_PATH")
	if !child {
		path = filepath.Join(t.TempDir(), "spool.db")
	}
	config := SpoolConfig{Path: path, MaxBytes: 1 << 20, MaxAge: time.Hour, Now: func() time.Time { return now }}
	s, err := open(config)
	if err != nil {
		t.Fatal(err)
	}
	if child {
		if s.db.NoSync {
			t.Fatal("audit data sync disabled")
		}
		items, err := s.peek(3)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 3 {
			t.Fatalf("legacy pending count: %d", len(items))
		}
		if err := s.ack([]uint64{items[0].Key}); err != nil {
			t.Fatal(err)
		}
		if err := s.reject([]uint64{items[1].Key}); err != nil {
			t.Fatal(err)
		}
		if err := s.enqueueBatch(events[3:4]); err != nil {
			t.Fatal(err)
		}
		// Intentionally bypass Close and all deferred functions, as with process
		// termination after successful commits. This is not a power-loss test.
		os.Exit(0)
	}
	// Seed the on-disk format used by the previous release.
	s.db.NoFreelistSync = false
	if err := s.enqueueBatch(events[:3]); err != nil {
		s.close()
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBoltSpoolRecoversCommittedEventsWithoutClose$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SNTP_SPOOL_EXIT_TEST="+kind, "SNTP_SPOOL_EXIT_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
	s, err = open(config)
	if err != nil {
		t.Fatalf("reopen after process exit: %v", err)
	}
	items, err := s.peek(10)
	if err != nil {
		s.close()
		t.Fatal(err)
	}
	if len(items) != 2 || !reflect.DeepEqual(items[0].Event, events[2]) || !reflect.DeepEqual(items[1].Event, events[3]) {
		s.close()
		t.Fatalf("committed events/FIFO changed: %#v", items)
	}
	stats, err := s.stats()
	if err != nil {
		s.close()
		t.Fatal(err)
	}
	if stats.PendingEvents != 2 || stats.PendingBytes == 0 || stats.RejectedEvents != 1 || stats.RejectedBytes == 0 || stats.DroppedEvents != 0 || stats.PersistenceFailures != 0 {
		s.close()
		t.Fatalf("recovery stats: %#v", stats)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	// The deployed dependency's default options represent the old binary.
	legacy, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("rollback open: %v", err)
	}
	err = legacy.View(func(tx *bolt.Tx) error {
		for checkErr := range tx.Check() {
			t.Errorf("rollback DB integrity: %v", checkErr)
		}
		got, err := readSpoolStats(tx)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, stats) {
			t.Errorf("rollback stats changed: %#v", got)
		}
		return nil
	})
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.enqueueBatch(events[4:]); err != nil {
		t.Fatal(err)
	}
	reopened, err := s.peek(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened) != 3 || !reflect.DeepEqual(reopened[:2], items) || reopened[2].Key <= items[1].Key || !reflect.DeepEqual(reopened[2].Event, events[4]) {
		t.Fatalf("rollback/reupgrade lost FIFO or sequence: %#v", reopened)
	}
}

// A nearly empty spool can retain a large freelist after an upload backlog drains.
// Small enqueue/ack transactions must not rewrite that historical free space map.
func TestBoltSpoolSmallWritesAfterLegacyBacklogDrains(t *testing.T) {
	now := time.Now().UTC()
	t.Run("access", func(t *testing.T) {
		checkSmallSpoolWrites(t, func(config SpoolConfig) (*boltSpool[Event], error) {
			s, err := NewBoltAccessSpool(config)
			if err != nil {
				return nil, err
			}
			return s.core, nil
		}, []Event{accessEventForTest("first", now), accessEventForTest("second", now)})
	})
	t.Run("flow", func(t *testing.T) {
		checkSmallSpoolWrites(t, func(config SpoolConfig) (*boltSpool[FlowEvent], error) {
			s, err := NewBoltFlowSpool(config)
			if err != nil {
				return nil, err
			}
			return s.core, nil
		}, []FlowEvent{flowEventForTest(1, now), flowEventForTest(2, now)})
	})
}

func checkSmallSpoolWrites[T any](t *testing.T, open func(SpoolConfig) (*boltSpool[T], error), events []T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spool.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucket([]byte("drained-backlog"))
		if err != nil {
			return err
		}
		return bucket.Put([]byte("payload"), bytes.Repeat([]byte("x"), 32<<20))
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("drained-backlog")) })
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := open(SpoolConfig{Path: path, MaxBytes: 1 << 20, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.db.NoSync {
		t.Fatal("audit data must retain synchronous commits")
	}
	if s.db.Stats().FreePageN < 4000 {
		t.Fatal("fixture did not retain a large freelist")
	}
	before := s.db.Stats().TxStats.PageAlloc
	if err := s.enqueueBatch(events); err != nil {
		t.Fatal(err)
	}
	items, err := s.peek(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d pending items", len(items))
	}
	if err := s.ack([]uint64{items[0].Key}); err != nil {
		t.Fatal(err)
	}
	if err := s.reject([]uint64{items[1].Key}); err != nil {
		t.Fatal(err)
	}
	allocated := s.db.Stats().TxStats.PageAlloc - before
	// PageAlloc counts transaction pages handed to the disk writer, excluding
	// the fixed-size meta page. Bound it instead of timing a particular disk.
	budget := 3 * 8 * s.db.Info().PageSize
	t.Logf("enqueue/ack/reject transaction page bytes: %d (budget %d)", allocated, budget)
	if allocated > budget {
		t.Fatalf("small transactions rewrite historical freelist: %d > %d bytes", allocated, budget)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	reopened, err := open(SpoolConfig{Path: path, MaxBytes: 1 << 20, MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("reconstruct freelist after backlog drains: %v", err)
	}
	defer reopened.close()
	t.Logf("reopen drained fixture: %s", time.Since(started))
	stats, err := reopened.stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingEvents != 0 || stats.PendingBytes != 0 || stats.RejectedEvents != 1 {
		t.Fatalf("drained state changed on reopen: %#v", stats)
	}
	if err := reopened.enqueueBatch(events); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.View(func(tx *bolt.Tx) error {
		for checkErr := range tx.Check() {
			t.Errorf("rebuilt freelist integrity: %v", checkErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

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
