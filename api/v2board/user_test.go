package panel

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestDeviceLimitEventReportUsesPrivatePayloadAndClassifiesValidation(t *testing.T) {
	var payload []byte
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/server/UniProxy/deviceLimitEvents" {
			http.NotFound(w, r)
			return
		}
		payload = append([]byte(nil), mustReadAll(t, r)...)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"data":true}`))
	}))
	defer server.Close()

	c := &Client{client: resty.New().SetBaseURL(server.URL)}
	event := DeviceLimitEvent{
		EventID: strings.Repeat("a", 64), UserID: 7, UUID: "device-a",
		Mode: "uuid", DeviceLimit: 1, AliveCount: 1,
		PendingDeviceCount: 2, CachedDeviceOverlap: 1,
		EffectiveDeviceCount: 2, MaxObservedCount: 3,
		HitCount: 4, FirstSeenAt: 1000, LastSeenAt: 1060,
	}
	if err := c.ReportDeviceLimitEvents(context.Background(), []DeviceLimitEvent{event}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(`"event_id"`)) ||
		!bytes.Contains(payload, []byte(`"events"`)) {
		t.Fatalf("payload=%s", payload)
	}
	if bytes.Contains(payload, []byte(`"ip"`)) || bytes.Contains(payload, []byte("192.0.2.")) {
		t.Fatalf("payload leaks source IP: %s", payload)
	}

	status = http.StatusUnprocessableEntity
	err := c.ReportDeviceLimitEvents(context.Background(), []DeviceLimitEvent{event})
	if err == nil || !IsValidationError(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestDeviceLimitEventPayloadHasNoSourceIPField(t *testing.T) {
	payload, err := json.Marshal(DeviceLimitEvent{EventID: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(`"ip"`)) {
		t.Fatalf("payload leaks source IP field: %s", payload)
	}
}

func mustReadAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	defer r.Body.Close()
	var body bytes.Buffer
	if _, err := body.ReadFrom(r.Body); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestUserSyncSeqKeepsHighestSequence(t *testing.T) {
	c := &Client{}

	c.SetUserSyncSeq(10)
	c.SetUserSyncSeq(9)
	if got := c.UserSyncSeq(); got != 10 {
		t.Fatalf("expected user sync seq to stay 10 after lower update, got %d", got)
	}

	c.updateUserSyncSeqFromHeader("11")
	if got := c.UserSyncSeq(); got != 11 {
		t.Fatalf("expected user sync seq to advance to 11, got %d", got)
	}

	c.updateUserSyncSeqFromHeader("8")
	if got := c.UserSyncSeq(); got != 11 {
		t.Fatalf("expected stale header to be ignored, got %d", got)
	}
}

func TestGetFullUserListSkipsIfNoneMatch(t *testing.T) {
	var gotIfNoneMatch string
	var gotForceHeader string
	var gotForceQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		gotForceHeader = r.Header.Get("X-Force-Full-User-List")
		gotForceQuery = r.URL.Query().Get("force_full")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"next"`)
		w.Header().Set("X-User-Sync-Seq", "12")
		_, _ = w.Write([]byte(`{"users":[]}`))
	}))
	defer server.Close()

	c := &Client{
		client:   resty.New().SetBaseURL(server.URL),
		userEtag: `"old"`,
	}

	users, err := c.GetFullUserList(context.Background())
	if err != nil {
		t.Fatalf("GetFullUserList returned error: %v", err)
	}
	if gotIfNoneMatch != "" {
		t.Fatalf("expected forced full user list to skip If-None-Match, got %q", gotIfNoneMatch)
	}
	if gotForceHeader != "1" {
		t.Fatalf("expected forced full user list header, got %q", gotForceHeader)
	}
	if gotForceQuery != "1" {
		t.Fatalf("expected forced full user list query flag, got %q", gotForceQuery)
	}
	if users == nil {
		t.Fatal("expected 200 empty users response to return a non-nil slice")
	}
	if got := c.UserSyncSeq(); got != 12 {
		t.Fatalf("expected sync seq 12, got %d", got)
	}
}

func TestFanoutContractsDecodeAndReport(t *testing.T) {
	var reported bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/server/config":
			_, _ = w.Write([]byte(`{"protocol":"vless","base_config":{"push_interval":60,"pull_interval":60,"uuid_ip_fanout_guard":{"enabled":true,"mode":"reject","window_seconds":600,"max_unique_ips":10,"event_cooldown_seconds":600,"whitelist_cidrs":["198.51.100.0/24"]}}}`))
		case "/api/v1/server/UniProxy/user":
			_, _ = w.Write([]byte(`{"users":[{"id":7,"uuid":"device-a","fanout_exempt":true}]}`))
		case "/api/v1/server/UniProxy/deviceAliveList":
			_, _ = w.Write([]byte(`{"alive_devices":{"7":1},"uuid_ip_fanout":{"revision":12,"mode":"reject","states":[{"user_id":7,"uuid":"device-a","allowed_ip_hashes":["abc"],"decision_expires_at":2000,"window_seconds":600,"threshold":10}]}}`))
		case "/api/v1/server/UniProxy/uuidIpFanoutEvents":
			reported = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := &Client{client: resty.New().SetBaseURL(server.URL), APIHost: server.URL, NodeId: 1}
	node, err := c.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if node.Common.BaseConfig.UUIDIPFanoutGuard == nil || node.Common.BaseConfig.UUIDIPFanoutGuard.Mode != "reject" {
		t.Fatalf("fanout config=%+v", node.Common.BaseConfig.UUIDIPFanoutGuard)
	}
	users, err := c.GetUserList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || !users[0].FanoutExempt {
		t.Fatalf("users=%+v", users)
	}
	state, err := c.GetUserDeviceAliveState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.AliveDevices[7] != 1 || state.UUIDIPFanout.Revision != 12 || len(state.UUIDIPFanout.States) != 1 {
		t.Fatalf("device alive state=%+v", state)
	}
	if err := c.ReportUUIDIPFanoutEvents(context.Background(), []UUIDIPFanoutEvent{{
		UserID: 7, UUID: "device-a", IP: "192.0.2.11", Scope: "local", Action: "reject",
		UniqueIPCount: 11, Threshold: 10, WindowSeconds: 600,
	}}); err != nil {
		t.Fatal(err)
	}
	if !reported {
		t.Fatal("fanout events were not reported")
	}
}

func TestAliveStateRequestsReturnPanelErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "panel unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	c := &Client{client: resty.New().SetBaseURL(server.URL)}
	if _, err := c.GetUserAlive(context.Background()); err == nil {
		t.Fatal("GetUserAlive hid panel error")
	}
	if _, err := c.GetUserDeviceAlive(context.Background()); err == nil {
		t.Fatal("GetUserDeviceAlive hid panel error")
	}
}

func TestUserReportRequestsReturnPanelErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "panel unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	c := &Client{client: resty.New().SetBaseURL(server.URL)}
	onlineUsers := map[int][]string{1: {"127.0.0.1"}}
	onlineDevices := map[int][]OnlineDeviceReportItem{1: {{UUID: "device", IP: "127.0.0.1"}}}
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "traffic", run: func() error {
			return c.ReportUserTraffic(context.Background(), []UserTraffic{{UID: 1, Upload: 1}})
		}},
		{name: "device traffic", run: func() error {
			return c.ReportUserDeviceTraffic(context.Background(), []UserDeviceTraffic{{UID: 1, UUID: "device", Upload: 1}})
		}},
		{name: "online users", run: func() error {
			return c.ReportNodeOnlineUsers(context.Background(), &onlineUsers)
		}},
		{name: "online devices", run: func() error {
			return c.ReportNodeOnlineDevices(context.Background(), &onlineDevices)
		}},
		{name: "UUID IP fanout events", run: func() error {
			return c.ReportUUIDIPFanoutEvents(context.Background(), []UUIDIPFanoutEvent{{
				UserID: 1, UUID: "device", IP: "127.0.0.1", Scope: "local", Action: "audit",
			}})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil {
				t.Fatal("report request hid panel error")
			}
		})
	}
}

func TestGetNodeInfoReturnsRepeatedHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "panel unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	c := &Client{client: resty.New().SetBaseURL(server.URL), APIHost: server.URL, NodeId: 1}
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := c.GetNodeInfo(context.Background()); err == nil {
			t.Fatalf("attempt %d hid panel error", attempt)
		}
	}
}
