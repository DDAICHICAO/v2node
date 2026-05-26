package accessaudit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
