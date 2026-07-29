package node

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/limiter"
)

func TestDeviceLimitEventReportRequeuesOnFailureWithStableID(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var requestCount atomic.Int32
	var eventIDsMu sync.Mutex
	var eventIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/server/UniProxy/deviceLimitEvents" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Events []panel.DeviceLimitEvent `json:"events"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Events) != 1 {
			t.Fatalf("events=%+v", body.Events)
		}
		eventIDsMu.Lock()
		eventIDs = append(eventIDs, body.Events[0].EventID)
		eventIDsMu.Unlock()
		requestCount.Add(1)
		if fail.Load() {
			http.Error(w, "panel unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":true}`))
	}))
	defer server.Close()

	retryCount := 0
	cfg := conf.NodeConfig{
		APIHost: server.URL, NodeID: 1, Key: "test",
		MachineIP: "127.0.0.1", RetryCount: &retryCount,
	}
	client, err := panel.New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	const tag = "device-limit-event-report"
	limiter.Init()
	l := limiter.AddLimiter("vless", tag, []panel.UserInfo{
		{Id: 7, Uuid: "device-a", DeviceLimit: 1},
		{Id: 7, Uuid: "device-b", DeviceLimit: 1},
	}, nil, map[int]int{7: 1}, true)
	l.CheckLimit(format.UserTag(tag, "device-a"), "192.0.2.1", true)
	l.CheckLimit(format.UserTag(tag, "device-b"), "192.0.2.2", true)

	c := &Controller{apiClient: client, limiter: l, tag: tag}
	c.reportDeviceLimitEvents(context.Background())
	fail.Store(false)
	c.reportDeviceLimitEvents(context.Background())

	if requestCount.Load() != 2 || len(eventIDs) != 2 ||
		eventIDs[0] == "" || eventIDs[0] != eventIDs[1] {
		t.Fatalf("requests=%d eventIDs=%v", requestCount.Load(), eventIDs)
	}
	if events := l.DrainDeviceLimitEvents(500); len(events) != 0 {
		t.Fatalf("reported events remained queued: %+v", events)
	}
}

func TestDeviceLimitEventValidationFailureIsNotRequeued(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid event", http.StatusUnprocessableEntity)
	}))
	defer server.Close()

	retryCount := 0
	client, err := panel.New(&conf.NodeConfig{
		APIHost: server.URL, NodeID: 1, Key: "test",
		MachineIP: "127.0.0.1", RetryCount: &retryCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	limiter.Init()
	l := limiter.AddLimiter("vless", "validation-event", nil, nil, nil, true)
	l.DeviceLimitEvents.Enqueue(limiter.DeviceLimitEvent{
		UserID: 7, UUID: "device-a", Mode: "uuid",
		DeviceLimit: 1, EffectiveDeviceCount: 2,
	}, time.Unix(1000, 0))

	c := &Controller{apiClient: client, limiter: l, tag: "validation-event"}
	c.reportDeviceLimitEvents(context.Background())
	if events := l.DrainDeviceLimitEvents(500); len(events) != 0 {
		t.Fatalf("validation failure was requeued: %+v", events)
	}
}
