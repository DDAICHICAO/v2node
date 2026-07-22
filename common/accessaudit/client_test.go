package accessaudit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBoltFlowSpoolPersistsFIFOAndCounters(t *testing.T) {
	now := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "flow.db")
	config := SpoolConfig{
		Path:     path,
		MaxBytes: 1 << 20,
		MaxAge:   24 * time.Hour,
		Now:      func() time.Time { return now },
	}

	spool, err := NewBoltFlowSpool(config)
	if err != nil {
		t.Fatalf("open spool: %v", err)
	}
	for sequence := uint32(1); sequence <= 3; sequence++ {
		if err := spool.Enqueue(flowEventForTest(sequence, now)); err != nil {
			t.Fatalf("enqueue %d: %v", sequence, err)
		}
	}

	items, err := spool.Peek(2)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if len(items) != 2 || items[0].Event.Sequence != 1 || items[1].Event.Sequence != 2 {
		t.Fatalf("unexpected FIFO items: %#v", items)
	}
	if err := spool.Ack([]uint64{items[0].Key, items[1].Key}); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := spool.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	spool, err = NewBoltFlowSpool(config)
	if err != nil {
		t.Fatalf("reopen spool: %v", err)
	}
	defer spool.Close()
	items, err = spool.Peek(10)
	if err != nil {
		t.Fatalf("peek after reopen: %v", err)
	}
	if len(items) != 1 || items[0].Event.Sequence != 3 || items[0].Event.EventID != "9:session-a:00000003" {
		t.Fatalf("unexpected persisted item: %#v", items)
	}

	if err := spool.Reject([]uint64{items[0].Key}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	stats, err := spool.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.PendingEvents != 0 || stats.RejectedEvents != 1 || stats.RejectedBytes == 0 {
		t.Fatalf("unexpected stats after reject: %#v", stats)
	}
	if stats.RejectedEventFrom != now.Unix() || stats.RejectedEventTo != now.Unix() {
		t.Fatalf("unexpected rejected time range: %#v", stats)
	}
}

func TestBoltFlowSpoolDropsOldestAtCapacityAndExpiresOldItems(t *testing.T) {
	now := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "flow.db")
	spool, err := NewBoltFlowSpool(SpoolConfig{
		Path:     path,
		MaxBytes: 1,
		MaxAge:   time.Hour,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("open spool: %v", err)
	}

	if err := spool.Enqueue(flowEventForTest(1, now)); err != nil {
		t.Fatalf("enqueue over capacity: %v", err)
	}
	stats, err := spool.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.PendingEvents != 0 || stats.DroppedEvents != 1 || stats.DroppedBytes == 0 {
		t.Fatalf("unexpected capacity stats: %#v", stats)
	}
	if err := spool.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	spool, err = NewBoltFlowSpool(SpoolConfig{
		Path:     path,
		MaxBytes: 1 << 20,
		MaxAge:   time.Hour,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("reopen spool: %v", err)
	}
	defer spool.Close()
	if err := spool.Enqueue(flowEventForTest(2, now)); err != nil {
		t.Fatalf("enqueue expiring item: %v", err)
	}
	now = now.Add(2 * time.Hour)
	items, err := spool.Peek(10)
	if err != nil {
		t.Fatalf("peek after expiration: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected expired spool to be empty: %#v", items)
	}
	stats, err = spool.Stats()
	if err != nil {
		t.Fatalf("stats after expiration: %v", err)
	}
	if stats.DroppedEvents != 2 || stats.DroppedEventFrom == 0 || stats.DroppedEventTo != time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("unexpected persistent dropped stats: %#v", stats)
	}
}

func flowEventForTest(sequence uint32, eventTime time.Time) FlowEvent {
	event := FlowEvent{
		SessionID:         "session-a",
		Sequence:          sequence,
		SampleType:        FlowSampleCheckpoint,
		EventTime:         eventTime,
		IntervalStartedAt: eventTime.Add(-time.Minute),
		NodeID:            9,
		UID:               145817,
		TargetHost:        "example.com",
		TargetPort:        443,
		Network:           "tcp",
		UploadBytes:       uint64(sequence) * 10,
		DownloadBytes:     uint64(sequence) * 20,
	}
	if err := event.Normalize(eventTime); err != nil {
		panic(err)
	}
	return event
}

func TestFlowEventNormalizeBuildsStableIdentityAndCompletion(t *testing.T) {
	startedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	eventAt := startedAt.Add(5 * time.Minute)

	for _, tt := range []struct {
		name       string
		sampleType string
		completed  bool
	}{
		{name: "checkpoint", sampleType: FlowSampleCheckpoint, completed: false},
		{name: "final", sampleType: FlowSampleFinal, completed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			event := FlowEvent{
				SessionID:         "session-a",
				Sequence:          7,
				SampleType:        tt.sampleType,
				EventTime:         eventAt,
				IntervalStartedAt: startedAt,
				NodeID:            42,
				UID:               145817,
				TargetHost:        "example.com",
				TargetPort:        443,
				Network:           "tcp",
				UploadBytes:       12,
				DownloadBytes:     34,
			}

			if err := event.Normalize(eventAt); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if event.EventID != "42:session-a:00000007" {
				t.Fatalf("unexpected event id %q", event.EventID)
			}
			if event.Completed != tt.completed {
				t.Fatalf("completed=%v want %v", event.Completed, tt.completed)
			}

			firstID := event.EventID
			if err := event.Normalize(eventAt.Add(time.Hour)); err != nil {
				t.Fatalf("normalize retry: %v", err)
			}
			if event.EventID != firstID {
				t.Fatalf("event id changed across retry: %q -> %q", firstID, event.EventID)
			}
		})
	}
}

func TestFlowEventNormalizeRejectsInvalidInput(t *testing.T) {
	valid := func() FlowEvent {
		startedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
		return FlowEvent{
			SessionID:         "session-a",
			Sequence:          1,
			SampleType:        FlowSampleCheckpoint,
			EventTime:         startedAt.Add(time.Minute),
			IntervalStartedAt: startedAt,
			NodeID:            1,
			UID:               2,
			TargetHost:        "example.com",
			TargetPort:        443,
			Network:           "tcp",
		}
	}

	tests := []struct {
		name   string
		mutate func(*FlowEvent)
	}{
		{name: "missing node", mutate: func(event *FlowEvent) { event.NodeID = 0 }},
		{name: "missing session", mutate: func(event *FlowEvent) { event.SessionID = "" }},
		{name: "missing sequence", mutate: func(event *FlowEvent) { event.Sequence = 0 }},
		{name: "unknown sample", mutate: func(event *FlowEvent) { event.SampleType = "unknown" }},
		{name: "missing event time", mutate: func(event *FlowEvent) { event.EventTime = time.Time{} }},
		{name: "missing interval start", mutate: func(event *FlowEvent) { event.IntervalStartedAt = time.Time{} }},
		{name: "event before interval", mutate: func(event *FlowEvent) { event.EventTime = event.IntervalStartedAt.Add(-time.Second) }},
		{name: "byte overflow", mutate: func(event *FlowEvent) { event.UploadBytes = ^uint64(0); event.DownloadBytes = 1 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := valid()
			tt.mutate(&event)
			if err := event.Normalize(time.Now()); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestClientFlushesSignedBatch(t *testing.T) {
	received := make(chan *http.Request, 1)
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)
		received <- r.Clone(r.Context())
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client, err := NewClient(Config{
		Enabled:       true,
		Endpoint:      server.URL,
		Token:         "secret",
		BatchSize:     1,
		MaxQueueSize:  4,
		FlushInterval: time.Hour,
		Timeout:       time.Second,
		Now:           func() time.Time { return time.Unix(1779786000, 0) },
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	client.Start()
	defer client.Close()

	ok := client.Enqueue(Event{
		EventTime:   time.Date(2026, 5, 26, 17, 0, 0, 123000000, time.FixedZone("CST", 8*3600)),
		NodeID:      1,
		NodeTag:     "test-node",
		UID:         145817,
		UUID:        "device-a",
		SourceIP:    "1.2.3.4",
		TargetHost:  "example.com",
		TargetPort:  443,
		Network:     "tcp",
		InboundTag:  "test-inbound",
		OutboundTag: "test-outbound",
	})
	if !ok {
		t.Fatal("expected event to enqueue")
	}

	var req *http.Request
	var body string
	select {
	case req = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request")
	}
	select {
	case body = <-bodies:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for body")
	}

	if req.Method != http.MethodPost {
		t.Fatalf("expected POST, got %s", req.Method)
	}
	if req.Header.Get("X-SNTP-Timestamp") != "1779786000" {
		t.Fatalf("unexpected timestamp %q", req.Header.Get("X-SNTP-Timestamp"))
	}
	wantSignature := signForTest([]byte(body), "secret", "1779786000")
	if req.Header.Get("X-SNTP-Signature") != wantSignature {
		t.Fatalf("unexpected signature %q want %q", req.Header.Get("X-SNTP-Signature"), wantSignature)
	}
	for _, want := range []string{
		`"event_time":"2026-05-26T17:00:00.123+08:00"`,
		`"node_id":1`,
		`"uid":145817`,
		`"target_host":"example.com"`,
		`"outbound_tag":"test-outbound"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
}

func TestClientDropsWhenQueueFull(t *testing.T) {
	client, err := NewClient(Config{
		Enabled:       true,
		Endpoint:      "https://logs.sntp.uk/api/v1/access-events",
		Token:         "secret",
		BatchSize:     10,
		MaxQueueSize:  1,
		FlushInterval: time.Hour,
		Timeout:       time.Second,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	if !client.Enqueue(Event{NodeID: 1, UID: 1, UUID: "a", SourceIP: "1.2.3.4", TargetHost: "example.com", TargetPort: 443, Network: "tcp"}) {
		t.Fatal("expected first event to enqueue")
	}
	if client.Enqueue(Event{NodeID: 1, UID: 1, UUID: "b", SourceIP: "1.2.3.5", TargetHost: "example.com", TargetPort: 443, Network: "tcp"}) {
		t.Fatal("expected second event to drop when queue is full")
	}
	if got := client.Dropped(); got != 1 {
		t.Fatalf("expected one dropped event, got %d", got)
	}
}

func signForTest(body []byte, token string, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(body)
	mac.Write([]byte(timestamp))
	return hex.EncodeToString(mac.Sum(nil))
}
