package panel

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestWakeupCapabilityAndConfigContract(t *testing.T) {
	if !slices.Contains(deviceLimitCapabilities, "user_sync_wakeup_v1") {
		t.Fatal("user_sync_wakeup_v1 capability missing")
	}
	var base BaseConfig
	err := json.Unmarshal([]byte(`{
		"user_sync_wakeup":{
			"enabled":true,
			"path":"/api/v2/server/user-sync/wakeup",
			"heartbeat_seconds":25,
			"fallback_poll_ms":2000,
			"merge_ms":250
		}
	}`), &base)
	if err != nil {
		t.Fatal(err)
	}
	if base.UserSyncWakeup == nil || !base.UserSyncWakeup.Enabled ||
		base.UserSyncWakeup.FallbackPollMS != 2000 {
		t.Fatalf("unexpected wakeup config: %#v", base.UserSyncWakeup)
	}
}

func TestFanoutReservationHTTPClassification(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantKind FanoutReservationKind
		wantErr  error
	}{
		{name: "allow", status: http.StatusOK, body: `{"data":{"decision":"allow"}}`, wantKind: FanoutReservationAllow},
		{name: "reject", status: http.StatusOK, body: `{"data":{"decision":"reject"}}`, wantKind: FanoutReservationReject},
		{name: "bad_request", status: http.StatusBadRequest, body: `{"error":"bad request"}`, wantErr: ErrFanoutReservationProtocol},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, wantErr: ErrFanoutReservationProtocol},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":"forbidden"}`, wantErr: ErrFanoutReservationProtocol},
		{name: "not_found", status: http.StatusNotFound, body: `{"error":"not found"}`, wantErr: ErrFanoutReservationFallback},
		{name: "conflict", status: http.StatusConflict, body: `{"error":"disabled"}`, wantErr: ErrFanoutReservationFallback},
		{name: "unprocessable", status: http.StatusUnprocessableEntity, body: `{"error":"ownership mismatch"}`, wantErr: ErrFanoutReservationProtocol},
		{name: "rate_limited", status: http.StatusTooManyRequests, body: `{"error":"busy"}`, wantErr: ErrFanoutReservationTemporary},
		{name: "server_error", status: http.StatusServiceUnavailable, body: `{"error":"unavailable"}`, wantErr: ErrFanoutReservationTemporary},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/server/uuid-ip-fanout/reserve" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client := &Client{client: resty.New().SetBaseURL(server.URL)}
			got, err := client.ReserveUUIDIPFanout(context.Background(), FanoutReservationRequest{
				UserID: 7, UUID: "device-a", IP: "192.0.2.1",
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err=%v", err)
				}
				if status := FanoutReservationStatus(err); status != tc.status {
					t.Fatalf("status=%d", status)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.Kind != tc.wantKind {
				t.Fatalf("kind=%v", got)
			}
		})
	}
}

func TestFanoutReservationTransportFailureIsTemporary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	client := &Client{client: resty.New().SetBaseURL(url)}
	_, err := client.ReserveUUIDIPFanout(context.Background(), FanoutReservationRequest{
		UserID: 7, UUID: "device-a", IP: "192.0.2.1",
	})
	if !errors.Is(err, ErrFanoutReservationTemporary) {
		t.Fatalf("err=%v", err)
	}
}

func TestFanoutReservationCapabilityAndInstanceIdentity(t *testing.T) {
	var advertised bool
	for _, capability := range deviceLimitCapabilities {
		if capability == "uuid_ip_fanout_reservation_v1" {
			advertised = true
			break
		}
	}
	if !advertised {
		t.Fatal("fanout reservation capability was not advertised")
	}

	client := &Client{instanceID: "node-instance"}
	if got := client.InstanceID(); got != "node-instance" {
		t.Fatalf("instance id=%q", got)
	}
}

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

	c.MarkUserSyncFullRequired()
	if got := c.UserSyncSeq(); got != 0 {
		t.Fatalf("expected full sync marker to reset sequence, got %d", got)
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
			_, _ = w.Write([]byte(`{"protocol":"vless","base_config":{"push_interval":60,"pull_interval":60,"uuid_ip_fanout_guard":{"enabled":true,"mode":"reject","window_seconds":600,"max_unique_ips":10,"event_cooldown_seconds":600,"whitelist_cidrs":["198.51.100.0/24"],"strategy":"reservation","reservation":{"enabled":true,"timeout_ms":800,"failure_mode":"open"}}}}`))
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
	if node.Common.BaseConfig.UUIDIPFanoutGuard.Strategy != "reservation" ||
		node.Common.BaseConfig.UUIDIPFanoutGuard.Reservation == nil ||
		node.Common.BaseConfig.UUIDIPFanoutGuard.Reservation.TimeoutMS != 800 {
		t.Fatalf("reservation config=%+v", node.Common.BaseConfig.UUIDIPFanoutGuard)
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
