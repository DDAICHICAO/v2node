package publicip

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const maxIPResponseBytes = 128
const failedDetectRetryInterval = time.Minute
const successfulDetectRefreshInterval = time.Minute

var defaultEndpoints = []string{
	"https://api.ipify.org",
	"https://api64.ipify.org",
	"https://icanhazip.com",
}

var detectMu sync.Mutex
var detectedIP string
var nextDetectAt time.Time

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("::ffff:0:0/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// Normalize returns a canonical public IP string, or an empty string for
// private, reserved, malformed, or otherwise unsuitable addresses.
func Normalize(value string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if !addr.IsValid() || !addr.IsGlobalUnicast() {
		return ""
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return ""
		}
	}
	return addr.String()
}

func Detect(ctx context.Context) string {
	now := time.Now()

	detectMu.Lock()
	cached := detectedIP
	nextAt := nextDetectAt
	detectMu.Unlock()
	if cached != "" && !nextAt.IsZero() && now.Before(nextAt) {
		return cached
	}
	if cached == "" && !nextAt.IsZero() && now.Before(nextAt) {
		return ""
	}

	ip := DetectFromEndpoints(ctx, defaultEndpoints)
	detectMu.Lock()
	defer detectMu.Unlock()
	if ip == "" {
		if detectedIP == "" {
			nextDetectAt = time.Now().Add(failedDetectRetryInterval)
			return ""
		}
		nextDetectAt = time.Now().Add(failedDetectRetryInterval)
		return detectedIP
	}

	detectedIP = ip
	nextDetectAt = time.Now().Add(successfulDetectRefreshInterval)
	return ip
}

func DetectFromEndpoints(ctx context.Context, endpoints []string) string {
	client := &http.Client{Timeout: 3 * time.Second}
	for _, endpoint := range endpoints {
		ip := detectFromEndpoint(ctx, client, strings.TrimSpace(endpoint))
		if ip != "" {
			return ip
		}
	}
	return ""
}

func detectFromEndpoint(ctx context.Context, client *http.Client, endpoint string) string {
	if endpoint == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "v2node")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIPResponseBytes))
	if err != nil {
		return ""
	}
	return Normalize(string(body))
}
