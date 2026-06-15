package publicip

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeAcceptsOnlyPublicIPs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "public ipv4", in: " 8.8.8.8\n", want: "8.8.8.8"},
		{name: "public ipv6", in: "2606:4700:4700::1111", want: "2606:4700:4700::1111"},
		{name: "private ipv4", in: "172.31.15.115", want: ""},
		{name: "loopback", in: "127.0.0.1", want: ""},
		{name: "reserved", in: "192.0.2.1", want: ""},
		{name: "garbage", in: "not an ip", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Normalize(tt.in); got != tt.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDetectRefreshesExpiredCachedIP(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("1.1.1.1"))
	}))
	defer server.Close()
	withDetectState(t, []string{server.URL}, "8.8.8.8", time.Now().Add(-time.Second))

	if got := Detect(context.Background()); got != "1.1.1.1" {
		t.Fatalf("Detect() = %q, want refreshed IP", got)
	}
	if calls != 1 {
		t.Fatalf("public IP endpoint calls = %d, want 1", calls)
	}
}

func TestDetectUsesFreshCachedIP(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("1.1.1.1"))
	}))
	defer server.Close()
	withDetectState(t, []string{server.URL}, "8.8.8.8", time.Now().Add(time.Minute))

	if got := Detect(context.Background()); got != "8.8.8.8" {
		t.Fatalf("Detect() = %q, want cached IP", got)
	}
	if calls != 0 {
		t.Fatalf("public IP endpoint calls = %d, want 0", calls)
	}
}

func withDetectState(t *testing.T, endpoints []string, cachedIP string, nextAt time.Time) {
	t.Helper()

	detectMu.Lock()
	oldEndpoints := defaultEndpoints
	oldDetectedIP := detectedIP
	oldNextDetectAt := nextDetectAt
	defaultEndpoints = endpoints
	detectedIP = cachedIP
	nextDetectAt = nextAt
	detectMu.Unlock()

	t.Cleanup(func() {
		detectMu.Lock()
		defaultEndpoints = oldEndpoints
		detectedIP = oldDetectedIP
		nextDetectAt = oldNextDetectAt
		detectMu.Unlock()
	})
}
