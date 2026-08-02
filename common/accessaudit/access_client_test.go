package accessaudit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func accessEventForTest(id string, now time.Time) Event {
	return Event{
		EventTime: now, NodeID: 9, NodeTag: "node",
		UID: 100, UUID: id, SourceIP: "192.0.2.10",
		TargetHost: "example.com", TargetPort: 443,
		Network: "tcp", InboundTag: "trojan", OutboundTag: "direct",
	}
}

func TestAccessClientEnqueueReturnsAfterAdmission(t *testing.T) {
	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	block := make(chan struct{})
	started := make(chan struct{})
	spool := &fakeBatchSpool[Event]{block: block, started: started}
	batcher := newPersistBatcher(persistBatcherConfig[Event]{
		Spool: spool, QueueSize: 1, BatchSize: 1,
		BatchWindow: time.Millisecond, SubmitTimeout: time.Hour,
	})
	client := &Client{
		config:    Config{Enabled: true, Now: func() time.Time { return now }},
		persister: batcher,
		wakeCh:    make(chan struct{}, 1),
	}
	returned := make(chan bool, 1)
	go func() { returned <- client.Enqueue(accessEventForTest("nonblocking", now)) }()
	select {
	case ok := <-returned:
		if !ok {
			t.Fatal("event was not admitted")
		}
	case <-time.After(100 * time.Millisecond):
		close(block)
		<-returned
		t.Fatal("enqueue waited for local transaction")
	}
	close(block)
	if err := batcher.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestAccessClientRejectsFullPersistenceQueueAndRecordsGap(t *testing.T) {
	now := time.Date(2026, 8, 2, 8, 10, 0, 0, time.UTC)
	block := make(chan struct{})
	started := make(chan struct{})
	writerSpool := &fakeBatchSpool[Event]{block: block, started: started}
	batcher := newPersistBatcher(persistBatcherConfig[Event]{
		Spool: writerSpool, QueueSize: 1, BatchSize: 1,
		BatchWindow: time.Millisecond, SubmitTimeout: time.Hour,
	})
	gapSpool, err := NewBoltAccessSpool(SpoolConfig{
		Path: filepath.Join(t.TempDir(), "gap.db"), MaxBytes: 1 << 20,
		MaxAge: time.Hour, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			close(block)
			released = true
		}
	}
	defer func() {
		release()
		if err := batcher.Close(context.Background()); err != nil {
			t.Fatalf("close batcher: %v", err)
		}
		if err := gapSpool.Close(); err != nil {
			t.Fatalf("close gap spool: %v", err)
		}
	}()
	client := &Client{
		config:    Config{Enabled: true, Now: func() time.Time { return now }},
		spool:     gapSpool,
		persister: batcher,
		wakeCh:    make(chan struct{}, 1),
	}
	if err := batcher.TrySubmit(accessEventForTest("writer", now)); err != nil {
		t.Fatalf("start writer: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	if err := batcher.TrySubmit(accessEventForTest("queued", now)); err != nil {
		t.Fatalf("fill queue: %v", err)
	}

	returned := make(chan bool, 1)
	go func() { returned <- client.Enqueue(accessEventForTest("rejected", now)) }()
	select {
	case ok := <-returned:
		if ok {
			t.Fatal("full queue event was reported as accepted")
		}
	case <-time.After(100 * time.Millisecond):
		release()
		<-returned
		t.Fatal("full queue admission waited instead of failing fast")
	}
	status := client.Status()
	if status.PersistenceFailures != 1 || status.PersistenceFailureFrom != now.Unix() {
		t.Fatalf("missing queue-full gap: %#v", status)
	}
}

func TestAccessClientRetainsFailureAndReplaysAfterRestart(t *testing.T) {
	now := time.Date(2026, 7, 23, 3, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "access.db")
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	config := Config{
		Enabled: true, Endpoint: server.URL, Token: "secret",
		BatchSize: 10, MaxQueueSize: 100, FlushInterval: time.Hour,
		Timeout: time.Second, Now: func() time.Time { return now },
		SpoolPath: path, MaxSpoolBytes: 1 << 20, MaxSpoolAge: 24 * time.Hour,
		HTTPClient: server.Client(),
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if !client.Enqueue(accessEventForTest("event-a", now)) {
		t.Fatal("local persistence failed")
	}
	waitForPendingEvents(t, client.spool.Stats, 1)
	if err := client.flushOnce(); err == nil {
		t.Fatal("expected remote failure")
	}
	if got := client.Status().PendingEvents; got != 1 {
		t.Fatalf("pending=%d", got)
	}
	client.Close()

	fail.Store(false)
	client, err = NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.flushOnce(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := client.Status().PendingEvents; got != 0 {
		t.Fatalf("pending after replay=%d", got)
	}
}

func TestAccessClientBisects400AndRejectsOnlyBadSingleton(t *testing.T) {
	now := time.Date(2026, 7, 23, 3, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("event-bad")) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	client, err := NewClient(Config{
		Enabled: true, Endpoint: server.URL, Token: "secret",
		BatchSize: 10, MaxQueueSize: 100, FlushInterval: time.Hour,
		Timeout: time.Second, Now: func() time.Time { return now },
		SpoolPath:     filepath.Join(t.TempDir(), "access.db"),
		MaxSpoolBytes: 1 << 20, MaxSpoolAge: time.Hour, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if !client.Enqueue(accessEventForTest("event-bad", now)) ||
		!client.Enqueue(accessEventForTest("event-good", now)) {
		t.Fatal("persist access events")
	}
	waitForPendingEvents(t, client.spool.Stats, 2)
	if err := client.flushOnce(); err != nil {
		t.Fatalf("bisect: %v", err)
	}
	status := client.Status()
	if status.PendingEvents != 0 || status.RejectedEvents != 1 {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestConfigureKeepsProxyAvailableWhenAccessSpoolCannotOpen(t *testing.T) {
	err := Configure(Config{
		Enabled: true, Endpoint: "https://logs.invalid/access", Token: "secret",
		SpoolPath:     filepath.Join(t.TempDir(), "missing", "access.db"),
		MaxSpoolBytes: 1 << 20, MaxSpoolAge: time.Hour,
		MkdirAll: func(string, os.FileMode) error { return errors.New("disk read-only") },
	})
	if err != nil {
		t.Fatalf("local spool failure must not abort proxy startup: %v", err)
	}
	defer Shutdown()
	status := CurrentRuntimeStatus()
	if status.LastErrorCode != "spool_open" || status.PersistenceFailures != 1 {
		t.Fatalf("missing startup gap status: %#v", status)
	}
}

func TestConfigureKeepsAccessAuditWhenLegacyFlowMigrationFails(t *testing.T) {
	now := time.Date(2026, 7, 23, 5, 0, 0, 0, time.UTC)
	flowPath := filepath.Join(t.TempDir(), "flow.db")
	db, err := bolt.Open(flowPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		pending, err := tx.CreateBucketIfNotExists(spoolPendingBucket)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(spoolMetaBucket); err != nil {
			return err
		}
		return pending.Put(uint64Key(1), []byte("{broken"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	err = Configure(Config{
		Enabled: true, Endpoint: server.URL, Token: "secret",
		BatchSize: 10, MaxQueueSize: 100, FlushInterval: time.Hour,
		Timeout: time.Second, Now: func() time.Time { return now },
		HTTPClient:    server.Client(),
		SpoolPath:     filepath.Join(t.TempDir(), "access.db"),
		MaxSpoolBytes: 1 << 20, MaxSpoolAge: time.Hour,
		FlowTraffic: FlowConfig{
			Enabled: true, CheckpointInterval: time.Minute,
			SpoolPath: flowPath, MaxSpoolBytes: 1 << 20, MaxSpoolAge: time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("flow migration failure must not abort access audit: %v", err)
	}
	defer Shutdown()
	if !Enqueue(accessEventForTest("access-survives", now)) {
		t.Fatal("ordinary access audit must remain available")
	}
	if status := CurrentFlowRuntimeStatus(); status.LastErrorCode != "migration" {
		t.Fatalf("unexpected flow startup status: %#v", status)
	}
	db, err = bolt.Open(flowPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bolt.Tx) error {
		got := tx.Bucket(spoolPendingBucket).Get(uint64Key(1))
		if string(got) != "{broken" {
			t.Fatalf("legacy data changed: %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
