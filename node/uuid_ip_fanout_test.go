package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
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
		Strategy:             "reservation",
		Reservation: &panel.UUIDIPFanoutReservationConfig{
			Enabled: true, TimeoutMS: 800, FailureMode: "open",
		},
	})
	if !got.Enabled || got.Mode != "reject" || got.Window != 5*time.Minute ||
		got.MaxUniqueIPs != 5 || got.EventCooldown != 2*time.Minute ||
		len(got.WhitelistCIDRs) != 1 || got.Strategy != "reservation" ||
		!got.ReservationEnabled || got.ReservationTimeout != 800*time.Millisecond {
		t.Fatalf("limiter config=%+v", got)
	}
}

func TestUUIDIPFanoutReservationControllerCallsOnceAndCachesAllow(t *testing.T) {
	var calls atomic.Int32
	controller, l, closeServer := newUUIDIPFanoutReservationTestController(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"decision":"allow","scope":"global","unique_ip_count":1,"threshold":10,"window_seconds":600}}`))
	})
	defer closeServer()

	taguuid := format.UserTag(controller.tag, "device-a")
	if _, rejected, info := l.CheckLimit(taguuid, "192.0.2.1", true); rejected {
		t.Fatalf("first connection rejected: %+v", info)
	}
	if _, rejected, info := l.CheckLimit(taguuid, "192.0.2.1", true); rejected {
		t.Fatalf("second connection rejected: %+v", info)
	}
	if calls.Load() != 1 {
		t.Fatalf("reservation calls=%d", calls.Load())
	}
}

func TestUUIDIPFanoutReservationTemporaryAndProtocolBehavior(t *testing.T) {
	t.Run("temporary fails open", func(t *testing.T) {
		controller, l, closeServer := newUUIDIPFanoutReservationTestController(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		})
		defer closeServer()

		started := time.Now()
		_, rejected, info := l.CheckLimit(format.UserTag(controller.tag, "device-a"), "192.0.2.2", true)
		if rejected {
			t.Fatalf("temporary error rejected: %+v", info)
		}
		if elapsed := time.Since(started); elapsed > 900*time.Millisecond {
			t.Fatalf("temporary fail-open elapsed=%s", elapsed)
		}
	})

	t.Run("unprocessable rejects and marks full sync", func(t *testing.T) {
		var calls atomic.Int32
		controller, l, closeServer := newUUIDIPFanoutReservationTestController(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			http.Error(w, "ownership mismatch", http.StatusUnprocessableEntity)
		})
		defer closeServer()
		controller.apiClient.SetUserSyncSeq(99)

		_, rejected, info := l.CheckLimit(format.UserTag(controller.tag, "device-a"), "192.0.2.3", true)
		if !rejected || info.FanoutScope != "protocol" {
			t.Fatalf("unprocessable result rejected=%v info=%+v", rejected, info)
		}
		if calls.Load() != 1 {
			t.Fatalf("reservation calls=%d", calls.Load())
		}
		if seq := controller.apiClient.UserSyncSeq(); seq != 0 {
			t.Fatalf("user sync seq=%d", seq)
		}
	})
}

func TestUUIDIPFanoutReservationFallbackAndSnapshotCompatibility(t *testing.T) {
	t.Run("404 uses snapshot", func(t *testing.T) {
		var calls atomic.Int32
		controller, l, closeServer := newUUIDIPFanoutReservationTestController(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			http.Error(w, "not found", http.StatusNotFound)
		})
		defer closeServer()
		installRejectingSnapshot(l, time.Now())

		_, rejected, info := l.CheckLimit(format.UserTag(controller.tag, "device-a"), "192.0.2.9", true)
		if !rejected || info.FanoutScope != "global" {
			t.Fatalf("fallback rejected=%v info=%+v", rejected, info)
		}
		if calls.Load() != 1 {
			t.Fatalf("reservation calls=%d", calls.Load())
		}
	})

	t.Run("snapshot strategy never reserves", func(t *testing.T) {
		var calls atomic.Int32
		controller, l, closeServer := newUUIDIPFanoutReservationTestController(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			http.Error(w, "must not be called", http.StatusInternalServerError)
		})
		defer closeServer()
		l.UpdateUUIDIPFanoutConfig(uuidIPFanoutLimiterConfig(&panel.UUIDIPFanoutConfig{
			Enabled: true, Mode: "reject", WindowSeconds: 600, MaxUniqueIPs: 10,
			EventCooldownSeconds: 600, Strategy: "snapshot",
		}))
		installRejectingSnapshot(l, time.Now())

		_, rejected, info := l.CheckLimit(format.UserTag(controller.tag, "device-a"), "192.0.2.9", true)
		if !rejected || info.FanoutScope != "global" {
			t.Fatalf("snapshot rejected=%v info=%+v", rejected, info)
		}
		if calls.Load() != 0 {
			t.Fatalf("snapshot reservation calls=%d", calls.Load())
		}
	})

	t.Run("config refresh switches to snapshot", func(t *testing.T) {
		var calls atomic.Int32
		controller, l, closeServer := newUUIDIPFanoutReservationTestController(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"decision":"allow"}}`))
		})
		defer closeServer()
		taguuid := format.UserTag(controller.tag, "device-a")
		if _, rejected, info := l.CheckLimit(taguuid, "192.0.2.1", true); rejected {
			t.Fatalf("reservation rejected: %+v", info)
		}

		l.UpdateUUIDIPFanoutConfig(uuidIPFanoutLimiterConfig(&panel.UUIDIPFanoutConfig{
			Enabled: true, Mode: "reject", WindowSeconds: 600, MaxUniqueIPs: 10,
			EventCooldownSeconds: 600, Strategy: "snapshot",
		}))
		if _, rejected, info := l.CheckLimit(taguuid, "192.0.2.2", true); rejected {
			t.Fatalf("snapshot local allow rejected: %+v", info)
		}
		if calls.Load() != 1 {
			t.Fatalf("reservation calls after refresh=%d", calls.Load())
		}
	})
}

func TestFanoutReservationRequestIDIsStableAndBucketed(t *testing.T) {
	candidate := limiter.UUIDIPFanoutInspection{
		UserID: 7, UUID: "device-a", IP: "192.0.2.1", WindowSeconds: 600,
	}
	now := time.Unix(1_700_000_000, 0)
	first := fanoutReservationRequestID("instance-a", candidate, now)
	second := fanoutReservationRequestID("instance-a", candidate, now.Add(time.Minute))
	nextBucket := fanoutReservationRequestID("instance-a", candidate, now.Add(10*time.Minute))
	if first != second {
		t.Fatalf("same bucket ids differ: %q %q", first, second)
	}
	if first == nextBucket {
		t.Fatalf("next bucket id did not change: %q", first)
	}
	if len(first) != sha256.Size*2 || first != strings.ToLower(first) {
		t.Fatalf("request id=%q", first)
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
	cfg := conf.NodeConfig{APIHost: server.URL, NodeID: 1, Key: "test", MachineIP: "1.1.1.1", RetryCount: &retryCount}
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

func newUUIDIPFanoutReservationTestController(
	t *testing.T,
	reserve http.HandlerFunc,
) (*Controller, *limiter.Limiter, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/server/uuid-ip-fanout/reserve" {
			http.NotFound(w, r)
			return
		}
		reserve(w, r)
	}))
	retryCount := 0
	cfg := conf.NodeConfig{
		APIHost: server.URL, NodeID: 1, Key: "test", MachineIP: "1.1.1.1",
		RetryCount: &retryCount,
	}
	client, err := panel.New(&cfg)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}

	const tag = "fanout-reservation-controller"
	limiter.Init()
	l := limiter.AddLimiter(
		"vless",
		tag,
		[]panel.UserInfo{{Id: 7, Uuid: "device-a"}},
		nil,
		nil,
		false,
	)
	l.UpdateUUIDIPFanoutConfig(uuidIPFanoutLimiterConfig(&panel.UUIDIPFanoutConfig{
		Enabled: true, Mode: "reject", WindowSeconds: 600, MaxUniqueIPs: 10,
		EventCooldownSeconds: 600, Strategy: "reservation",
		Reservation: &panel.UUIDIPFanoutReservationConfig{
			Enabled: true, TimeoutMS: 800, FailureMode: "open",
		},
	}))
	controller := &Controller{apiClient: client, limiter: l, tag: tag}
	controller.installUUIDIPFanoutReservation(l)
	return controller, l, server.Close
}

func installRejectingSnapshot(l *limiter.Limiter, now time.Time) {
	allowed := sha256.Sum256([]byte("192.0.2.1"))
	l.UpdateUUIDIPFanoutGlobal(limiter.UUIDIPFanoutGlobalState{
		Revision: 1,
		Mode:     "reject",
		States: []limiter.UUIDIPFanoutGlobalDecision{{
			UserID:            7,
			UUID:              "device-a",
			AllowedIPHashes:   []string{hex.EncodeToString(allowed[:])},
			DecisionExpiresAt: now.Add(time.Minute).Unix(),
			Threshold:         10,
			WindowSeconds:     600,
		}},
	}, now)
}
