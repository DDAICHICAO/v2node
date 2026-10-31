package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

const validNodeConfigBody = `{"protocol":"vless","listen_ip":"0.0.0.0","server_port":443,"base_config":{"push_interval":60,"pull_interval":60}}`

func TestNodeConfigVersionCommitsOnlyAfterSuccessfulParse(t *testing.T) {
	var body atomic.Value
	body.Store(validNodeConfigBody)
	var etag atomic.Value
	etag.Store(`"v1"`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", etag.Load().(string))
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer server.Close()

	client := newNodeConfigTestClient(server.URL)
	info, err := client.GetNodeInfo(context.Background())
	if err != nil || info == nil || client.NodeInfoVersion() == "" {
		t.Fatalf("info=%v version=%q err=%v", info, client.NodeInfoVersion(), err)
	}
	version := client.NodeInfoVersion()
	if client.nodeEtag != `"v1"` {
		t.Fatalf("etag=%q, want v1", client.nodeEtag)
	}

	body.Store(`{"protocol":`)
	etag.Store(`"broken"`)
	if _, err := client.GetNodeInfo(context.Background()); err == nil {
		t.Fatal("expected invalid JSON error")
	}
	if client.NodeInfoVersion() != version {
		t.Fatal("invalid response advanced node config version")
	}
	if client.nodeEtag != `"v1"` {
		t.Fatalf("invalid response advanced etag to %q", client.nodeEtag)
	}

	body.Store(validNodeConfigBody)
	etag.Store(`"v2"`)
	info, err = client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info != nil {
		t.Fatalf("same body returned a new node: %+v", info)
	}
	if client.NodeInfoVersion() != version {
		t.Fatal("same body changed the content version")
	}
	if client.nodeEtag != `"v2"` {
		t.Fatalf("same body did not remember new etag: %q", client.nodeEtag)
	}
}

func TestNodeConfigFetchSerializes(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maxActive.Load()
			if current <= observed || maxActive.CompareAndSwap(observed, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"stable"`)
		_, _ = w.Write([]byte(validNodeConfigBody))
	}))
	defer server.Close()

	client := newNodeConfigTestClient(server.URL)
	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := client.GetNodeInfo(context.Background())
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("concurrent panel requests=%d, want 1", got)
	}
}

func newNodeConfigTestClient(apiHost string) *Client {
	return &Client{
		client:   resty.New().SetBaseURL(apiHost),
		APIHost:  apiHost,
		NodeId:   1,
		UserList: &UserListBody{},
		AliveMap: &AliveMap{},
	}
}

func TestValidateIngressModeAcceptsSupportedSNIFrontProtocols(t *testing.T) {
	for _, protocol := range []string{"trojan", "anytls", "vless"} {
		common := validSNIFrontCommon(protocol)
		if err := ValidateIngressMode(common); err != nil {
			t.Fatalf("protocol=%s: unexpected validation error: %v", protocol, err)
		}
	}
}

func TestValidateIngressModeRejectsUnsafeSNIFrontCombinations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CommonNode)
	}{
		{name: "unsupported protocol", mutate: func(c *CommonNode) { c.Protocol = "vmess" }},
		{name: "tls disabled", mutate: func(c *CommonNode) { c.Tls = None }},
		{name: "reality", mutate: func(c *CommonNode) { c.Tls = Reality }},
		{name: "non tcp", mutate: func(c *CommonNode) { c.Network = "ws" }},
		{name: "missing sni", mutate: func(c *CommonNode) { c.TlsSettings.ServerName = "" }},
		{name: "ip sni", mutate: func(c *CommonNode) { c.TlsSettings.ServerName = "192.0.2.10" }},
		{name: "wildcard sni", mutate: func(c *CommonNode) { c.TlsSettings.ServerName = "*.example.com" }},
		{name: "invalid server port", mutate: func(c *CommonNode) { c.ServerPort = 0 }},
		{name: "certificate disabled", mutate: func(c *CommonNode) { c.TlsSettings.CertMode = "none" }},
		{name: "flow enabled", mutate: func(c *CommonNode) { c.Flow = "xtls-rprx-vision" }},
		{name: "proxy protocol type", mutate: func(c *CommonNode) { c.NetworkSettings = json.RawMessage(`{"acceptProxyProtocol":"true"}`) }},
		{name: "ech enabled", mutate: func(c *CommonNode) { c.TlsSettings.ECH = "custom" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			common := validSNIFrontCommon("vless")
			tc.mutate(common)
			if err := ValidateIngressMode(common); err == nil {
				t.Fatal("expected invalid SNI front configuration to be rejected")
			}
		})
	}
}

func TestValidateIngressModeKeepsDirectModeAndTitanSeparate(t *testing.T) {
	if err := ValidateIngressMode(&CommonNode{
		Protocol: "vless",
		Tls:      None,
		Network:  "ws",
	}); err != nil {
		t.Fatalf("direct mode should keep existing combinations valid: %v", err)
	}
	if err := ValidateIngressMode(&CommonNode{
		Protocol: "sntp-eclipse",
		Tls:      None,
		Network:  "tcp",
		TlsSettings: TlsSettings{
			FrontMode: "direct",
		},
	}); err != nil {
		t.Fatalf("Titan direct mode should remain valid: %v", err)
	}
	common := validSNIFrontCommon("sntp-eclipse")
	if err := ValidateIngressMode(common); err == nil {
		t.Fatal("Titan must not enter SNI front mode")
	}
}

func validSNIFrontCommon(protocol string) *CommonNode {
	return &CommonNode{
		Protocol:        protocol,
		Tls:             Tls,
		Network:         "tcp",
		ServerPort:      10011,
		Flow:            "",
		NetworkSettings: json.RawMessage(`{"acceptProxyProtocol":true}`),
		TlsSettings: TlsSettings{
			FrontMode:  "sni",
			ServerName: "node.example.com",
			CertMode:   "file",
		},
	}
}
