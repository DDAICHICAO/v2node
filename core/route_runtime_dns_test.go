package core

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core/app/dispatcher"
	featuredns "github.com/xtls/xray-core/features/dns"
	"golang.org/x/net/dns/dnsmessage"
)

func TestRuntimeDNSUpstreamBypassesDNSInterception(t *testing.T) {
	upstream, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		packet := make([]byte, 4096)
		for {
			n, peer, err := upstream.ReadFrom(packet)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if err := query.Unpack(packet[:n]); err != nil || len(query.Questions) != 1 {
				t.Error("upstream received an invalid DNS query")
				return
			}
			queries.Add(1)
			response := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: query.ID, Response: true, RecursionDesired: true, RecursionAvailable: true},
				Questions: query.Questions,
				Answers: []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: query.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 88}},
				}},
			}
			wire, err := response.Pack()
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := upstream.WriteTo(wire, peer); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	defer func() { upstream.Close(); <-done }()

	port := upstream.LocalAddr().(*net.UDPAddr).Port
	dnsValue := fmt.Sprintf(`{"servers":[{"address":"127.0.0.1","port":%d,"queryStrategy":"UseIPv4","timeoutMs":300}],"disableFallbackIfMatch":true}`, port)
	infos := []*panel.NodeInfo{routeCompileNode(1, panel.Route{
		Id: 47, Action: "dns", Match: []string{"domain:dns-loop.invalid"}, ActionValue: &dnsValue,
	})}
	v := New(conf.New())
	if err := v.Start(infos); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	lease, err := v.routeRuntime.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	compiled, err := CompileRouteRuntime(infos)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the production UDP DNS interception rule on an ephemeral port,
	// so this test never needs to bind or alter the machine's real port 53.
	ports := compiled.RouterConfig.Rule[0].PortList.Range[0]
	ports.From, ports.To = uint32(port), uint32(port)
	router, err := createRouteRouter(v.Server, compiled.RouterConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Start(); err != nil {
		router.Close()
		t.Fatal(err)
	}
	if err := v.routeRuntime.Publish(dispatcher.RouteRuntimeGeneration{
		ID: 1, DNS: lease.DNS(), Router: router,
		Handlers: listOutboundHandlers(v.ohm), Default: lease.DefaultHandler(), Cleanup: router.Close,
	}); err != nil {
		router.Close()
		t.Fatal(err)
	}
	ips, _, err := v.routeDNS.LookupIP("www.dns-loop.invalid", featuredns.IPOption{IPv4Enable: true})
	if err != nil {
		t.Fatalf("upstream DNS query was intercepted recursively: %v (upstream queries=%d)", err, queries.Load())
	}
	if len(ips) != 1 || ips[0].String() != "192.0.2.88" || queries.Load() != 1 {
		t.Fatalf("lookup did not use the configured upstream exactly once: ips=%v queries=%d", ips, queries.Load())
	}
}
