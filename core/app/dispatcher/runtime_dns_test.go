package dispatcher

import (
	"net"
	"sync/atomic"
	"testing"

	featuredns "github.com/xtls/xray-core/features/dns"
)

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
