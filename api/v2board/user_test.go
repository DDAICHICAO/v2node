package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-resty/resty/v2"
)

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
