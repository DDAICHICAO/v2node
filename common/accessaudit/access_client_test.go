package accessaudit

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func accessEventForTest(id string, now time.Time) Event {
	return Event{
		EventTime: now, NodeID: 9, NodeTag: "node",
		UID: 100, UUID: id, SourceIP: "192.0.2.10",
		TargetHost: "example.com", TargetPort: 443,
		Network: "tcp", InboundTag: "trojan", OutboundTag: "direct",
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
