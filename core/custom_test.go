package core

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/limiter"
	"github.com/xtls/xray-core/app/proxyman"
	xnet "github.com/xtls/xray-core/common/net"
	coreConf "github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
	xws "github.com/xtls/xray-core/transport/internet/websocket"
)

func TestCloudflareRemoteAddr(t *testing.T) {
	store := newCloudflareRangeStore(builtinCloudflareRanges)
	for _, tc := range []struct {
		name, peer, header, ipv6, want string
		duplicate                      bool
	}{
		{name: "CF IPv4", peer: "173.245.48.1", header: "8.8.8.8", want: "8.8.8.8"},
		{name: "CF IPv6", peer: "2606:4700::1", header: "2001:4860:4860::8888", want: "2001:4860:4860::8888"},
		{name: "mapped CF", peer: "::ffff:173.245.48.1", header: "::ffff:8.8.8.8", want: "8.8.8.8"},
		{name: "forged direct", peer: "9.9.9.9", header: "8.8.8.8"},
		{name: "outside CF range", peer: "173.245.47.255", header: "8.8.8.8"},
		{name: "loopback forged", peer: "127.0.0.1", header: "8.8.8.8"},
		{name: "missing CF header", peer: "173.245.48.1"},
		{name: "not an IP", peer: "173.245.48.1", header: "host.invalid"},
		{name: "multiple IPs", peer: "173.245.48.1", header: "8.8.8.8, 9.9.9.9"},
		{name: "duplicate header", peer: "173.245.48.1", header: "8.8.8.8", duplicate: true},
		{name: "private client", peer: "173.245.48.1", header: "10.0.0.1"},
		{name: "loopback client", peer: "173.245.48.1", header: "::1"},
		{name: "mapped private", peer: "173.245.48.1", header: "::ffff:192.168.1.1"},
		{name: "unspecified client", peer: "173.245.48.1", header: "0.0.0.0"},
		{name: "multicast client", peer: "173.245.48.1", header: "ff02::1"},
		{name: "documentation client", peer: "173.245.48.1", header: "192.0.2.1"},
		{name: "scoped client", peer: "173.245.48.1", header: "2001:4860::1%eth0"},
		{name: "IP with port", peer: "173.245.48.1", header: "8.8.8.8:443"},
		{name: "pseudo IPv4", peer: "173.245.48.1", header: "240.1.2.3", ipv6: "2001:4860:4860::8888", want: "2001:4860:4860::8888"},
		{name: "pseudo without IPv6", peer: "173.245.48.1", header: "240.1.2.3"},
		{name: "pseudo with IPv4", peer: "173.245.48.1", header: "240.1.2.3", ipv6: "8.8.8.8"},
		{name: "CF IPv6 header alone", peer: "173.245.48.1", ipv6: "2001:4860:4860::8888"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := &net.TCPAddr{IP: net.ParseIP(tc.peer), Port: 12345}
			headers := http.Header{}
			headers.Set("X-Forwarded-For", "1.1.1.1")
			if tc.header != "" {
				headers.Set("CF-Connecting-IP", tc.header)
			}
			if tc.duplicate {
				headers.Add("CF-Connecting-IP", tc.header)
			}
			if tc.ipv6 != "" {
				headers.Set("CF-Connecting-IPv6", tc.ipv6)
			}
			got := store.remoteAddr(peer, headers)
			if tc.want == "" {
				if got != peer {
					t.Fatalf("untrusted header changed peer to %v", got)
				}
			} else if got.String() != net.JoinHostPort(tc.want, "0") {
				t.Fatalf("got %v, want %s", got, tc.want)
			}
		})
	}
	for _, cidr := range builtinCloudflareRanges {
		p := netip.MustParsePrefix(cidr)
		if !store.contains(p.Addr()) || !store.contains(p.Addr().Next()) {
			t.Fatalf("built-in range missing: %s", cidr)
		}
	}
}

type cloudflareTestRoundTrip func(*http.Request) (*http.Response, error)

func (f cloudflareTestRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCloudflareRangeRefresh(t *testing.T) {
	valid := `{"success":true,"result":{"ipv4_cidrs":["173.245.48.0/20"],"ipv6_cidrs":["2606:4700::/32"]}}`
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"valid", valid, 200, true},
		{"status", valid, 503, false},
		{"redirect", valid, 302, false},
		{"truncated", `{"success":`, 200, false},
		{"failure", strings.Replace(valid, `true`, `false`, 1), 200, false},
		{"missing family", strings.Replace(valid, `["2606:4700::/32"]`, `[]`, 1), 200, false},
		{"wrong family", strings.Replace(valid, `173.245.48.0/20`, `2606:4700::/32`, 1), 200, false},
		{"default", strings.Replace(valid, `173.245.48.0/20`, `0.0.0.0/0`, 1), 200, false},
		{"private", strings.Replace(valid, `173.245.48.0/20`, `10.0.0.0/8`, 1), 200, false},
		{"overlap reserved", strings.Replace(valid, `173.245.48.0/20`, `192.0.0.0/8`, 1), 200, false},
		{"host bits", strings.Replace(valid, `173.245.48.0/20`, `173.245.48.1/20`, 1), 200, false},
		{"too large", strings.Repeat(" ", 65537), 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newCloudflareRangeStore(builtinCloudflareRanges)
			before := store.prefixes.Load()
			client := &http.Client{Transport: cloudflareTestRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != cloudflareIPURL {
					t.Errorf("unexpected URL: %s", r.URL)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			err := store.refresh(context.Background(), client)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if !tc.valid && store.prefixes.Load() != before {
				t.Fatal("failed refresh replaced known-good snapshot")
			}
			if tc.valid && (len(*store.prefixes.Load()) != 2 || store.contains(netip.MustParseAddr("104.16.0.1"))) {
				t.Fatal("successful refresh did not replace snapshot")
			}
		})
	}
	// The embedded official fallback itself must satisfy updater validation.
	var v4, v6 []string
	for _, cidr := range builtinCloudflareRanges {
		if netip.MustParsePrefix(cidr).Addr().Is4() {
			v4 = append(v4, cidr)
		} else {
			v6 = append(v6, cidr)
		}
	}
	body, _ := json.Marshal(map[string]any{"success": true, "result": map[string]any{"ipv4_cidrs": v4, "ipv6_cidrs": v6}})
	if _, err := parseCloudflareRanges(strings.NewReader(string(body))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := newCloudflareRangeStore(builtinCloudflareRanges)
	before := store.prefixes.Load()
	if err := store.refresh(ctx, &http.Client{}); err == nil || store.prefixes.Load() != before {
		t.Fatal("canceled refresh changed the snapshot")
	}
}

func TestCloudflareWebSocketStream(t *testing.T) {
	for _, trust := range []bool{false, true} {
		t.Run(fmt.Sprintf("trusted=%v", trust), func(t *testing.T) {
			store := newCloudflareRangeStore(builtinCloudflareRanges)
			if trust {
				store = newCloudflareRangeStore([]string{"127.0.0.0/8"})
			} // test fixture only
			accepted := make(chan stat.Connection, 1)
			handler := &cloudflareWSHandler{config: &xws.Config{Path: "/ws", Host: "origin.test", HeartbeatPeriod: 1}, peers: store, addConn: func(c stat.Connection) { accepted <- c }}
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			dialer := websocket.Dialer{TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig, HandshakeTimeout: 3 * time.Second}
			url := "wss" + strings.TrimPrefix(server.URL, "https")
			for _, path := range []string{"/wrong", "/ws"} {
				headers := http.Header{"Host": []string{"wrong.test"}}
				_, response, err := dialer.Dial(url+path, headers)
				if err == nil || response == nil || response.StatusCode != 404 {
					t.Fatal("host/path validation was bypassed")
				}
				response.Body.Close()
			}
			headers := http.Header{}
			headers.Set("Host", "origin.test")
			headers.Set("CF-Connecting-IP", "8.8.8.8")
			headers.Set("X-Forwarded-For", "1.1.1.1")
			headers.Set("Sec-WebSocket-Protocol", base64.RawURLEncoding.EncodeToString([]byte("early")))
			client, response, err := dialer.Dial(url+"/ws", headers)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if response.Header.Get("Sec-WebSocket-Protocol") != headers.Get("Sec-WebSocket-Protocol") {
				t.Fatal("early-data protocol not echoed")
			}
			var conn stat.Connection
			select {
			case conn = <-accepted:
			case <-time.After(3 * time.Second):
				t.Fatal("missing stream")
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
			host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
			want := "127.0.0.1"
			if trust {
				want = "8.8.8.8"
			}
			if host != want {
				t.Fatalf("remote=%s want=%s", host, want)
			}
			data := make([]byte, 5)
			if _, err := io.ReadFull(conn, data); err != nil || string(data) != "early" {
				t.Fatalf("early data=%q err=%v", data, err)
			}
			if err := client.WriteMessage(websocket.BinaryMessage, []byte("hello")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(conn, data); err != nil || string(data) != "hello" {
				t.Fatalf("stream data=%q err=%v", data, err)
			}
			if _, err := conn.Write([]byte("reply")); err != nil {
				t.Fatal(err)
			}
			_, data, err = client.ReadMessage()
			if err != nil || string(data) != "reply" {
				t.Fatalf("reply=%q err=%v", data, err)
			}
		})
	}
}

func TestCloudflareRegisteredListener(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	reservation.Close()
	accepted := make(chan stat.Connection, 1)
	listener, err := internet.ListenTCP(context.Background(), xnet.LocalHostIP, xnet.Port(port), &internet.MemoryStreamConfig{
		ProtocolName: cloudflareWebSocket, ProtocolSettings: &xws.Config{Path: "/ws"},
	}, func(c stat.Connection) { accepted <- c })
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ws", port), http.Header{"Cf-Connecting-Ip": []string{"8.8.8.8"}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var conn stat.Connection
	select {
	case conn = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("registered transport did not accept")
	}
	defer conn.Close()
	if host, _, _ := net.SplitHostPort(conn.RemoteAddr().String()); host != "127.0.0.1" {
		t.Fatal("public forged header trusted")
	}
	listener.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := client.WriteMessage(websocket.BinaryMessage, []byte("survives")); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 8)
	if _, err := io.ReadFull(conn, data); err != nil || string(data) != "survives" {
		t.Fatalf("closing listener killed existing stream: %q %v", data, err)
	}
}

func TestCloudflareWebSocketInboundSelection(t *testing.T) {
	for _, protocol := range []string{"vless", "vmess", "trojan", "anytls"} {
		for _, proxy := range []bool{false, true} {
			n := routeCompileNode(42)
			n.Type = protocol
			n.Common.Protocol = protocol
			n.Common.ListenIP = "127.0.0.1"
			n.Common.ServerPort = 443
			n.Common.Network = "ws"
			if proxy {
				n.Common.NetworkSettings = json.RawMessage(`{"path":"/ws","acceptProxyProtocol":true}`)
			} else {
				n.Common.NetworkSettings = json.RawMessage(`{"path":"/ws"}`)
			}
			in, err := buildInbound(n, n.Tag)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := in.ReceiverSettings.GetInstance()
			if err != nil {
				t.Fatal(err)
			}
			want := "sntp-cf-websocket"
			if proxy {
				want = "websocket"
			}
			if got := raw.(*proxyman.ReceiverConfig).StreamSettings.ProtocolName; got != want {
				t.Fatalf("proxy=%v: transport=%q, want %q", proxy, got, want)
			}
			if _, err := internet.ToMemoryStreamConfig(raw.(*proxyman.ReceiverConfig).StreamSettings); err != nil {
				t.Fatal(err)
			}
		}
	}
	n := routeCompileNode(42)
	n.Common.ListenIP = "127.0.0.1"
	n.Common.ServerPort = 443
	n.Common.Network = "tcp"
	n.Common.NetworkSettings = json.RawMessage(`{}`)
	in, err := buildInbound(n, n.Tag)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := in.ReceiverSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	if raw.(*proxyman.ReceiverConfig).StreamSettings.ProtocolName != "tcp" {
		t.Fatal("non-WS inbound changed")
	}
	for _, protocol := range []string{"trojan", "anytls"} {
		n.Type = protocol
		n.Common.Protocol = protocol
		n.Common.Network = "ws"
		n.Common.NetworkSettings = nil
		if _, err := buildInbound(n, n.Tag); err != nil {
			t.Fatalf("default WS config %s: %v", protocol, err)
		}
	}
}

func TestCloudflareVLessOnlineReport(t *testing.T) {
	// Only this test temporarily trusts loopback to simulate a CF origin socket.
	before := cloudflarePeers.prefixes.Load()
	defer cloudflarePeers.prefixes.Store(before)
	fixture := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	cloudflarePeers.prefixes.Store(&fixture)
	limiter.Init()
	for i, trusted := range []bool{true, false} {
		t.Run(fmt.Sprintf("trusted=%v", trusted), func(t *testing.T) {
			if trusted {
				cloudflarePeers.prefixes.Store(&fixture)
			} else {
				cloudflarePeers.prefixes.Store(before)
			}
			reservation, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			n := routeCompileNode(9042 + i)
			n.Common.ListenIP = "127.0.0.1"
			n.Common.ServerPort = reservation.Addr().(*net.TCPAddr).Port
			n.Common.Network = "ws"
			n.Common.NetworkSettings = json.RawMessage(`{"path":"/ws"}`)
			reservation.Close()
			users := []panel.UserInfo{{Id: 123, Uuid: "11111111-2222-4333-8444-555555555555"}}
			limit := limiter.AddLimiter("v2ray", n.Tag, users, nil, nil, true)
			defer limiter.DeleteLimiter(n.Tag)
			v := New(conf.New())
			if err := v.Start(nil); err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			if _, err := v.AddNodeWithUsers(n.Tag, n, users); err != nil {
				t.Fatal(err)
			}
			client, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ws", n.Common.ServerPort), http.Header{"Cf-Connecting-Ip": []string{"8.8.8.8"}})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			stream := xws.NewConnection(client, client.RemoteAddr(), nil, 0)
			_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
			id, _ := hex.DecodeString(strings.ReplaceAll(users[0].Uuid, "-", ""))
			// No external target: this test ends after real authentication/online accounting.
			port := 1
			request := append([]byte{0}, id...)
			request = append(request, 0, 1, byte(port>>8), byte(port), 1, 127, 0, 0, 1)
			request = append(request, []byte("hello")...)
			if _, err := stream.Write(request); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, ok := limit.UserOnlineIP.Load(format.UserTag(n.Tag, users[0].Uuid)); ok {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("authenticated connection never reached online accounting")
				}
				time.Sleep(10 * time.Millisecond)
			}
			online, devices, err := limit.GetOnlineDeviceState()
			want := "127.0.0.1"
			if trusted {
				want = "8.8.8.8"
			}
			if err != nil || len(*online) != 1 || len(*devices) != 1 || (*online)[0].IP != want || (*devices)[0].IP != want || (*devices)[0].UID != 123 {
				t.Fatalf("online=%+v devices=%+v error=%v want=%s", online, devices, err, want)
			}
		})
	}
}

func TestApplyDNSRouteConfigKeepsSimpleDNSRoute(t *testing.T) {
	value := "8.8.8.8"
	config := &coreConf.DNSConfig{}

	applyDNSRouteConfig(config, panel.Route{
		Match:       []string{"geosite:openai"},
		ActionValue: &value,
	})

	if len(config.Servers) != 1 {
		t.Fatalf("expected 1 DNS server, got %d", len(config.Servers))
	}
	if config.Servers[0].Address == nil {
		t.Fatal("expected simple DNS route to set a server address")
	}
	if len(config.Servers[0].Domains) != 1 || config.Servers[0].Domains[0] != "geosite:openai" {
		t.Fatalf("expected route match to become server domains, got %#v", config.Servers[0].Domains)
	}
	if !config.Servers[0].SkipFallback {
		t.Fatal("expected matched DNS route to skip fallback")
	}
}

func TestApplyDNSRouteConfigMergesXrayDNSConfig(t *testing.T) {
	value := `{
		"servers": [
			"1.1.1.1",
			{
				"address": "https+local://hkg.core.access.zznet.fun/dns-query",
				"domains": ["geosite:openai"],
				"skipFallback": true,
				"finalQuery": true,
				"queryStrategy": "UseIPv4"
			}
		],
		"queryStrategy": "UseIPv4",
		"disableFallbackIfMatch": true,
		"tag": "dns_inbound"
	}`
	config := &coreConf.DNSConfig{}

	applyDNSRouteConfig(config, panel.Route{ActionValue: &value})

	if len(config.Servers) != 2 {
		t.Fatalf("expected 2 DNS servers, got %d", len(config.Servers))
	}
	if config.QueryStrategy != "UseIPv4" {
		t.Fatalf("expected query strategy UseIPv4, got %q", config.QueryStrategy)
	}
	if !config.DisableFallbackIfMatch {
		t.Fatal("expected disableFallbackIfMatch to be enabled")
	}
	if config.Tag != "dns_inbound" {
		t.Fatalf("expected tag dns_inbound, got %q", config.Tag)
	}
	unlockServer := config.Servers[1]
	if unlockServer.Address == nil {
		t.Fatal("expected unlock server address")
	}
	if len(unlockServer.Domains) != 1 || unlockServer.Domains[0] != "geosite:openai" {
		t.Fatalf("expected unlock server domains, got %#v", unlockServer.Domains)
	}
	if !unlockServer.SkipFallback || !unlockServer.FinalQuery {
		t.Fatalf("expected skipFallback and finalQuery to be true, got skipFallback=%v finalQuery=%v", unlockServer.SkipFallback, unlockServer.FinalQuery)
	}
}

func TestApplyDNSRouteConfigAppliesMatchToSingleJSONServer(t *testing.T) {
	value := `{"address":"https://dns.google/dns-query","queryStrategy":"UseIPv4"}`
	config := &coreConf.DNSConfig{}

	applyDNSRouteConfig(config, panel.Route{
		Match:       []string{"domain:hulu.jp"},
		ActionValue: &value,
	})

	if len(config.Servers) != 1 {
		t.Fatalf("expected 1 DNS server, got %d", len(config.Servers))
	}
	if len(config.Servers[0].Domains) != 1 || config.Servers[0].Domains[0] != "domain:hulu.jp" {
		t.Fatalf("expected route match to be applied to the JSON server, got %#v", config.Servers[0].Domains)
	}
	if !config.Servers[0].SkipFallback {
		t.Fatal("expected matched JSON DNS server to skip fallback")
	}
	if config.Servers[0].QueryStrategy != "UseIPv4" {
		t.Fatalf("expected server query strategy UseIPv4, got %q", config.Servers[0].QueryStrategy)
	}
}
