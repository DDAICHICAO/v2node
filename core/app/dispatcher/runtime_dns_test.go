package dispatcher

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/session"
	featuredns "github.com/xtls/xray-core/features/dns"
)

type dnsOwnLinkVerifier interface {
	IsOwnLink(context.Context) bool
}

func runtimeDNSVerifier(t *testing.T, client *RuntimeDNSClient) dnsOwnLinkVerifier {
	t.Helper()
	verifier, ok := interface{}(client).(dnsOwnLinkVerifier)
	if !ok {
		t.Fatal("runtime DNS wrapper hides IsOwnLink; DNS outbound will intercept its own upstream queries")
	}
	return verifier
}

func dnsInboundContext(tag string) context.Context {
	return session.ContextWithInbound(context.Background(), &session.Inbound{Tag: tag})
}

func TestRuntimeDNSClientRecognizesOwnQueriesAcrossReload(t *testing.T) {
	oldDNS := &ownedRuntimeDNS{tag: "dns-old"}
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{ID: 1, DNS: oldDNS})
	defer manager.Close()
	client := NewRuntimeDNSClient()
	client.Bind(manager)
	verifier := runtimeDNSVerifier(t, client)
	oldLease, err := manager.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer oldLease.Release()
	if !verifier.IsOwnLink(dnsInboundContext("dns-old")) {
		t.Fatal("active backend's upstream DNS query was not recognized")
	}
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2, DNS: &ownedRuntimeDNS{tag: "dns-new"}}); err != nil {
		t.Fatal(err)
	}
	// Xray's DNS dispatcher detaches the original context, retaining only the
	// inbound tag. An old query must still bypass interception after publication.
	for _, tag := range []string{"dns-old", "dns-new"} {
		if !verifier.IsOwnLink(dnsInboundContext(tag)) {
			t.Fatalf("live DNS query %q was not recognized after reload", tag)
		}
	}
	if verifier.IsOwnLink(dnsInboundContext("client-inbound")) || verifier.IsOwnLink(context.Background()) {
		t.Fatal("ordinary client traffic bypassed DNS interception")
	}
	oldLease.Release()
	if verifier.IsOwnLink(dnsInboundContext("dns-old")) {
		t.Fatal("released generation still authorizes upstream DNS bypass")
	}
	if stats := manager.Stats(); stats.ActiveLeases != 0 || stats.Retired != 0 {
		t.Fatalf("ownership checks leaked generation leases: %+v", stats)
	}
}

func TestRuntimeDNSClientOwnQueryCheckRequiresLiveBackend(t *testing.T) {
	var nilClient *RuntimeDNSClient
	if runtimeDNSVerifier(t, nilClient).IsOwnLink(context.Background()) {
		t.Fatal("nil client recognized a query")
	}
	client := NewRuntimeDNSClient()
	verifier := runtimeDNSVerifier(t, client)
	if verifier.IsOwnLink(dnsInboundContext("dns")) {
		t.Fatal("unbound client recognized a query")
	}
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{ID: 1, DNS: &fixedRuntimeDNS{ip: "192.0.2.1"}})
	defer manager.Close()
	client.Bind(manager)
	if verifier.IsOwnLink(dnsInboundContext("dns")) {
		t.Fatal("backend without ownership interface recognized a query")
	}
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2, DNS: &ownedRuntimeDNS{tag: "dns"}}); err != nil {
		t.Fatal(err)
	}
	if verifier.IsOwnLink(nil) {
		t.Fatal("nil context recognized a query")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if verifier.IsOwnLink(dnsInboundContext("dns")) {
		t.Fatal("closed client recognized a query")
	}
	other := NewRuntimeDNSClient()
	other.Bind(manager)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if runtimeDNSVerifier(t, other).IsOwnLink(dnsInboundContext("dns")) {
		t.Fatal("closed runtime recognized a query")
	}
}

type ownedRuntimeDNS struct {
	fixedRuntimeDNS
	tag string
}

func (d *ownedRuntimeDNS) IsOwnLink(ctx context.Context) bool {
	inbound := session.InboundFromContext(ctx)
	return inbound != nil && inbound.Tag == d.tag
}

func TestRuntimeDNSClientPinsBackendDuringOwnQueryCheck(t *testing.T) {
	backend := &blockingOwnedRuntimeDNS{
		ownedRuntimeDNS: ownedRuntimeDNS{tag: "dns-old"},
		started:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(backend.release) }) }
	defer release()
	var cleaned atomic.Int32
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{
		ID: 1, DNS: backend, Cleanup: func() error { cleaned.Add(1); return nil },
	})
	defer manager.Close()
	client := NewRuntimeDNSClient()
	client.Bind(manager)
	verifier := runtimeDNSVerifier(t, client)
	result := make(chan bool, 1)
	go func() { result <- verifier.IsOwnLink(dnsInboundContext("dns-old")) }()
	select {
	case <-backend.started:
	case <-time.After(2 * time.Second):
		t.Fatal("ownership check did not reach backend")
	}
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2, DNS: &ownedRuntimeDNS{tag: "dns-new"}}); err != nil {
		t.Fatal(err)
	}
	if cleaned.Load() != 0 {
		t.Fatal("backend cleaned while ownership check was running")
	}
	release()
	select {
	case own := <-result:
		if !own {
			t.Fatal("ownership changed during publication")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ownership check did not finish")
	}
	if cleaned.Load() != 1 || manager.Stats().ActiveLeases != 0 {
		t.Fatalf("ownership check did not release backend: cleaned=%d stats=%+v", cleaned.Load(), manager.Stats())
	}
}

type blockingOwnedRuntimeDNS struct {
	ownedRuntimeDNS
	started chan struct{}
	release chan struct{}
}

func (d *blockingOwnedRuntimeDNS) IsOwnLink(ctx context.Context) bool {
	close(d.started)
	<-d.release
	return d.ownedRuntimeDNS.IsOwnLink(ctx)
}

func TestRuntimeDNSClientPinsOneBackendPerLookup(t *testing.T) {
	oldDNS := newBlockingDNS("192.0.2.1")
	var cleaned atomic.Int32
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{
		ID:      1,
		DNS:     oldDNS,
		Cleanup: func() error { cleaned.Add(1); return nil },
	})
	client := NewRuntimeDNSClient()
	client.Bind(manager)
	result := make(chan string, 1)
	go func() {
		ips, _, err := client.LookupIP("example.com", featuredns.IPOption{IPv4Enable: true})
		if err != nil {
			result <- "error: " + err.Error()
			return
		}
		result <- ips[0].String()
	}()
	<-oldDNS.started
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2, DNS: &fixedRuntimeDNS{ip: "198.51.100.2"}}); err != nil {
		t.Fatal(err)
	}
	if cleaned.Load() != 0 {
		t.Fatal("old DNS cleaned during lookup")
	}
	close(oldDNS.release)
	if got := <-result; got != "192.0.2.1" {
		t.Fatalf("old lookup result=%s", got)
	}
	if cleaned.Load() != 1 {
		t.Fatalf("cleanup count=%d", cleaned.Load())
	}
	ips, _, err := client.LookupIP("example.com", featuredns.IPOption{IPv4Enable: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := ips[0].String(); got != "198.51.100.2" {
		t.Fatalf("new lookup result=%s", got)
	}
}

func TestRuntimeDNSClientRequiresLiveBinding(t *testing.T) {
	client := NewRuntimeDNSClient()
	if _, _, err := client.LookupIP("example.com", featuredns.IPOption{IPv4Enable: true}); err == nil {
		t.Fatal("unbound DNS client accepted lookup")
	}
	client.Bind(NewRouteRuntimeManager(RouteRuntimeGeneration{ID: 1, DNS: &fixedRuntimeDNS{ip: "192.0.2.1"}}))
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.LookupIP("example.com", featuredns.IPOption{IPv4Enable: true}); err == nil {
		t.Fatal("closed DNS client accepted lookup")
	}
	if RuntimeDNSFeatureConfig() == nil {
		t.Fatal("runtime DNS feature config is nil")
	}
}

type fixedRuntimeDNS struct {
	ip string
}

func (*fixedRuntimeDNS) Type() interface{} { return featuredns.ClientType() }
func (*fixedRuntimeDNS) Start() error      { return nil }
func (*fixedRuntimeDNS) Close() error      { return nil }
func (d *fixedRuntimeDNS) LookupIP(string, featuredns.IPOption) ([]net.IP, uint32, error) {
	return []net.IP{net.ParseIP(d.ip)}, 60, nil
}

type blockingRuntimeDNS struct {
	fixedRuntimeDNS
	started chan struct{}
	release chan struct{}
}

func newBlockingDNS(ip string) *blockingRuntimeDNS {
	return &blockingRuntimeDNS{
		fixedRuntimeDNS: fixedRuntimeDNS{ip: ip},
		started:         make(chan struct{}),
		release:         make(chan struct{}),
	}
}

func (d *blockingRuntimeDNS) LookupIP(domain string, option featuredns.IPOption) ([]net.IP, uint32, error) {
	close(d.started)
	<-d.release
	return d.fixedRuntimeDNS.LookupIP(domain, option)
}
