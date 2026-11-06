package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Official Cloudflare origin peer ranges, checked on 2026-11-06.
// These are independent of the client-facing preferred-IP DNS lists.
const cloudflareIPURL = "https://api.cloudflare.com/client/v4/ips"

var builtinCloudflareRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

type cloudflareRangeStore struct {
	prefixes atomic.Pointer[[]netip.Prefix]
}

func newCloudflareRangeStore(cidrs []string) *cloudflareRangeStore {
	s := new(cloudflareRangeStore)
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		prefixes = append(prefixes, netip.MustParsePrefix(cidr))
	}
	s.prefixes.Store(&prefixes)
	return s
}

var cloudflarePeers = newCloudflareRangeStore(builtinCloudflareRanges)

func validatedCloudflarePrefixes(cidrs []string) ([]netip.Prefix, error) {
	if len(cidrs) == 0 || len(cidrs) > 256 {
		return nil, errors.New("Cloudflare CIDRs must contain 1 to 256 prefixes")
	}
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	seen := make(map[netip.Prefix]bool, len(cidrs))
	for _, cidr := range cidrs {
		p, err := netip.ParsePrefix(cidr)
		if err != nil || p != p.Masked() || p.Addr().Is4In6() || p.Bits() < 8 ||
			(!p.Addr().Is4() && p.Bits() < 19) || !p.Addr().IsGlobalUnicast() {
			return nil, errors.New("invalid Cloudflare prefix")
		}
		for _, blocked := range *nonPublicRanges.prefixes.Load() {
			if p.Overlaps(blocked) {
				return nil, errors.New("unsafe Cloudflare prefix")
			}
		}
		if !seen[p] {
			seen[p] = true
			prefixes = append(prefixes, p)
		}
	}
	return prefixes, nil
}

func (s *cloudflareRangeStore) contains(ip netip.Addr) bool {
	list := s.prefixes.Load()
	if list != nil {
		for _, prefix := range *list {
			if prefix.Contains(ip.Unmap()) {
				return true
			}
		}
	}
	return false
}

var nonPublicRanges = newCloudflareRangeStore([]string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/3", "::/96", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48",
	"100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
	"fc00::/7", "fe80::/10", "ff00::/8",
})

func publicClientIP(value string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || ip.Zone() != "" {
		return netip.Addr{}, false
	}
	ip = ip.Unmap()
	return ip, ip.IsGlobalUnicast() && !ip.IsPrivate() && !nonPublicRanges.contains(ip)
}

// Resolve only from the socket peer, never X-Forwarded-For or another header.
func (s *cloudflareRangeStore) remoteAddr(peer net.Addr, headers http.Header) net.Addr {
	if peer == nil {
		return peer
	}
	host, _, err := net.SplitHostPort(peer.String())
	if err != nil {
		return peer
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !s.contains(ip) {
		return peer
	}
	values := headers.Values("CF-Connecting-IP")
	if len(values) != 1 {
		return peer
	}
	client, ok := publicClientIP(values[0])
	// Cloudflare Pseudo IPv4 overwrite mode retains the real IPv6 here.
	if raw, err := netip.ParseAddr(strings.TrimSpace(values[0])); err == nil && netip.MustParsePrefix("240.0.0.0/4").Contains(raw.Unmap()) {
		v6 := headers.Values("CF-Connecting-IPv6")
		if len(v6) != 1 {
			return peer
		}
		client, ok = publicClientIP(v6[0])
		ok = ok && client.Is6()
	}
	if !ok {
		return peer
	}
	return &net.TCPAddr{IP: net.IP(client.AsSlice()), Port: 0}
}

func parseCloudflareRanges(r io.Reader) ([]netip.Prefix, error) {
	const maxBody = 64 * 1024
	body, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, errors.New("Cloudflare IP response too large")
	}
	var response struct {
		Success bool `json:"success"`
		Result  struct {
			IPv4 []string `json:"ipv4_cidrs"`
			IPv6 []string `json:"ipv6_cidrs"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if !response.Success || len(response.Result.IPv4) == 0 || len(response.Result.IPv6) == 0 || len(response.Result.IPv4)+len(response.Result.IPv6) > 256 {
		return nil, errors.New("invalid Cloudflare IP response")
	}
	prefixes := make([]netip.Prefix, 0, len(response.Result.IPv4)+len(response.Result.IPv6))
	for family, cidrs := range [][]string{response.Result.IPv4, response.Result.IPv6} {
		for _, cidr := range cidrs {
			p, err := netip.ParsePrefix(cidr)
			if err != nil || p != p.Masked() || p.Addr().Is4() != (family == 0) || p.Addr().Is4In6() || p.Bits() < 8 || (family == 1 && p.Bits() < 19) || !p.Addr().IsGlobalUnicast() {
				return nil, errors.New("invalid Cloudflare prefix")
			}
			for _, blocked := range *nonPublicRanges.prefixes.Load() {
				if p.Overlaps(blocked) {
					return nil, errors.New("unsafe Cloudflare prefix")
				}
			}
			prefixes = append(prefixes, p)
		}
	}
	return prefixes, nil
}

func (s *cloudflareRangeStore) refresh(ctx context.Context, client *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cloudflareIPURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "SNTP-v2node/Cloudflare-IP-ranges")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Cloudflare IP HTTP status %d", resp.StatusCode)
	}
	prefixes, err := parseCloudflareRanges(resp.Body)
	if err != nil {
		return err
	}
	s.prefixes.Store(&prefixes)
	return nil
}

// RunCloudflareIPUpdates refreshes independently of business startup/reloads.
// An unavailable API never blocks startup or replaces a known-good snapshot.
func RunCloudflareIPUpdates(ctx context.Context) {
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := cloudflarePeers.refresh(ctx, client); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("Cloudflare 回源网段更新失败，继续使用已有清单")
		} else {
			log.Info("Cloudflare 回源网段清单已更新")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
