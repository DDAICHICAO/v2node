package node

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/limiter"
)

func TestUUIDIPFanoutLimiterConfigUsesPanelValues(t *testing.T) {
	got := uuidIPFanoutLimiterConfig(&panel.UUIDIPFanoutConfig{
		Enabled:              true,
		Mode:                 "reject",
		WindowSeconds:        300,
		MaxUniqueIPs:         5,
		EventCooldownSeconds: 120,
		WhitelistCIDRs:       []string{"198.51.100.0/24"},
	})
	if !got.Enabled || got.Mode != "reject" || got.Window != 5*time.Minute ||
		got.MaxUniqueIPs != 5 || got.EventCooldown != 2*time.Minute ||
		len(got.WhitelistCIDRs) != 1 {
		t.Fatalf("limiter config=%+v", got)
	}
}

func TestUUIDIPFanoutEventReportRequeuesOnFailure(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/server/UniProxy/uuidIpFanoutEvents" {
			http.NotFound(w, r)
			return
		}
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
	cfg := conf.NodeConfig{APIHost: server.URL, NodeID: 1, Key: "test", MachineIP: "127.0.0.1", RetryCount: &retryCount}
	client, err := panel.New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	const tag = "fanout-event-report"
	limiter.Init()
	l := limiter.AddLimiter("vless", tag, []panel.UserInfo{{Id: 7, Uuid: "device-a"}}, nil, nil, false)
	l.UpdateUUIDIPFanoutConfig(limiter.UUIDIPFanoutConfig{
		Enabled: true, Mode: "reject", Window: time.Minute, MaxUniqueIPs: 2,
	})
	l.CheckLimit(format.UserTag(tag, "device-a"), "192.0.2.1", true)
	l.CheckLimit(format.UserTag(tag, "device-a"), "192.0.2.2", true)
	l.CheckLimit(format.UserTag(tag, "device-a"), "192.0.2.3", true)

	c := &Controller{apiClient: client, limiter: l, tag: tag}
	c.reportUUIDIPFanoutEvents(context.Background())
	fail.Store(false)
	c.reportUUIDIPFanoutEvents(context.Background())
	if requestCount.Load() != 2 {
		t.Fatalf("request count=%d, want 2", requestCount.Load())
	}
	if events := l.DrainUUIDIPFanoutEvents(500); len(events) != 0 {
		t.Fatalf("reported events remained queued: %+v", events)
	}
}
