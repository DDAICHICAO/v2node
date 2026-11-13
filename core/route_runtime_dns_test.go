package core

import (
	"context"
	"fmt"
	"github.com/wyx2685/v2node/limiter"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	xraycore "github.com/xtls/xray-core/core"
	routingdns "github.com/xtls/xray-core/features/routing/dns"
	routingsession "github.com/xtls/xray-core/features/routing/session"
	"github.com/xtls/xray-core/proxy/shadowsocks"
	"github.com/xtls/xray-core/transport"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// This exercises the real DNS outbound, not a mock of the resolver selector.
func TestRuntimeDNSIsolatedByInboundAndGeneration(t *testing.T) {
	jp := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 1})
	us := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 2})
	changed := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 3})
	infos := func(usPort int) []*panel.NodeInfo {
		return []*panel.NodeInfo{isolatedDNSNode(1, jp), isolatedDNSNode(2, usPort), routeCompileNode(3)}
	}
	v := New(conf.New())
	if err := v.Start(infos(us)); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	old, err := v.routeRuntime.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	check := func(base context.Context, tag, want string) {
		t.Helper()
		got := queryIsolatedDNS(t, v, session.ContextWithInbound(base, &session.Inbound{Tag: tag}), "video.isolation.invalid")
		if got != want {
			t.Fatalf("inbound %s DNS=%s, want %s", tag, got, want)
		}
	}
	for i := 0; i < 2; i++ {
		check(context.Background(), "node-1", "127.0.0.1")
		check(context.Background(), "node-2", "127.0.0.2")
		check(context.Background(), StreamUnlockProbeTag("node-2"), "127.0.0.2")
	}
	if _, err := v.ApplyRouteRuntime(infos(changed)); err != nil {
		t.Fatal(err)
	}
	check(context.Background(), "node-1", "127.0.0.1")
	check(context.Background(), "node-2", "127.0.0.3")
	check(old.Context(context.Background()), "node-2", "127.0.0.2")
}

func isolatedDNSNode(id, port int) *panel.NodeInfo {
	value := fmt.Sprintf(`{"servers":[{"address":"127.0.0.1","port":%d,"queryStrategy":"UseIPv4","timeoutMs":500}],"disableFallbackIfMatch":true}`, port)
	return routeCompileNode(id, panel.Route{Id: id, Action: "dns", Match: []string{"domain:isolation.invalid"}, ActionValue: &value})
}

func startIsolatedDNSUpstream(t *testing.T, ip [4]byte) int {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		packet := make([]byte, 4096)
		for {
			n, peer, err := conn.ReadFrom(packet)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if q.Unpack(packet[:n]) != nil || len(q.Questions) != 1 {
				continue
			}
			response := dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true, RecursionDesired: true, RecursionAvailable: true}, Questions: q.Questions}
			if q.Questions[0].Type == dnsmessage.TypeA {
				response.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: ip}}}
			}
			wire, err := response.Pack()
			if err == nil {
				_, _ = conn.WriteTo(wire, peer)
			}
		}
	}()
	t.Cleanup(func() { conn.Close(); <-done })
	return conn.LocalAddr().(*net.UDPAddr).Port
}

func queryIsolatedDNS(t *testing.T, v *V2Core, ctx context.Context, domain string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := xraycore.Dial(ctx, v.Server, xnet.UDPDestination(xnet.ParseAddress("192.0.2.53"), 53))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 123, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(domain + "."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	wire, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, 4096)
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() { n, err := conn.Read(packet); done <- result{n, err} }()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		conn.Close()
		<-done
		t.Fatal("isolated DNS query timed out")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	var response dnsmessage.Message
	if err := response.Unpack(packet[:r.n]); err != nil {
		t.Fatal(err)
	}
	for _, answer := range response.Answers {
		if a, ok := answer.Body.(*dnsmessage.AResource); ok {
			return net.IP(a.A[:]).String()
		}
	}
	t.Fatalf("no A answer: %+v", response.Header)
	return ""
}

// A normal Freedom connection must use the owning node's DNS too.
func TestRuntimeDNSIsolatedFreedomTCP(t *testing.T) {
	jp := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 1})
	us := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 2})
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			ip := c.LocalAddr().(*net.TCPAddr).IP.To4()
			_, _ = c.Write([]byte{ip[3]})
			c.Close()
		}
	}()
	defer func() { listener.Close(); <-done }()
	v := New(conf.New())
	if err := v.Start([]*panel.NodeInfo{isolatedDNSNode(1, jp), isolatedDNSNode(2, us)}); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	for _, id := range []int{1, 2, 1, 2} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		ctx = session.ContextWithInbound(ctx, &session.Inbound{Tag: fmt.Sprintf("node-%d", id)})
		conn, err := xraycore.Dial(ctx, v.Server, xnet.TCPDestination(xnet.DomainAddress("video.isolation.invalid"), xnet.Port(listener.Addr().(*net.TCPAddr).Port)))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		var b [1]byte
		_, err = io.ReadFull(conn, b[:])
		conn.Close()
		cancel()
		if err != nil || int(b[0]) != id {
			t.Fatalf("node %d connected to DNS target %d: %v", id, b[0], err)
		}
	}
}

// Endpoint-addressed UDP packets resolve after the socket has been opened.
// Keep an old session alive and resolve a fresh domain after publication.
func TestRuntimeDNSIsolatedFreedomUDPEndpoints(t *testing.T) {
	jp := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 1})
	us := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 2})
	updated := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 3})
	port := 0
	for _, id := range []int{1, 2, 3} {
		c, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.%d:%d", id, port))
		if err != nil {
			t.Fatal(err)
		}
		if port == 0 {
			port = c.LocalAddr().(*net.UDPAddr).Port
		}
		done := make(chan struct{})
		go func(c net.PacketConn, id int) {
			defer close(done)
			b := make([]byte, 4096)
			for {
				_, peer, err := c.ReadFrom(b)
				if err != nil {
					return
				}
				_, _ = c.WriteTo([]byte{byte(id)}, peer)
			}
		}(c, id)
		t.Cleanup(func() { c.Close(); <-done })
	}
	makeInfos := func(usPort int) []*panel.NodeInfo {
		return []*panel.NodeInfo{isolatedDNSNode(1, jp), isolatedDNSNode(2, usPort)}
	}
	v := New(conf.New())
	if err := v.Start(makeInfos(us)); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	open := func(id int) *transport.Link {
		t.Helper()
		ctx := session.ContextWithInbound(context.Background(), &session.Inbound{Tag: fmt.Sprintf("node-%d", id)})
		link, err := v.dispatcher.Dispatch(ctx, xnet.UDPDestination(xnet.ParseAddress("127.0.0.1"), xnet.Port(port)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { common.Close(link.Writer); common.Interrupt(link.Reader) })
		return link
	}
	query := func(link *transport.Link, domain string, want byte) {
		t.Helper()
		b := buf.New()
		_, _ = b.Write([]byte("ping"))
		dest := xnet.UDPDestination(xnet.DomainAddress(domain), xnet.Port(port))
		b.UDP = &dest
		if err := link.Writer.WriteMultiBuffer(buf.MultiBuffer{b}); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		var response buf.MultiBuffer
		var err error
		go func() { response, err = link.Reader.ReadMultiBuffer(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			common.Interrupt(link.Reader)
			<-done
			t.Fatal("UDP isolation timeout")
		}
		defer buf.ReleaseMulti(response)
		if err != nil || len(response) != 1 || response[0].Len() != 1 || response[0].Byte(0) != want {
			t.Fatalf("UDP endpoint want=%d response=%v err=%v", want, response, err)
		}
	}
	oldUS := open(2)
	query(oldUS, "video.isolation.invalid", 2)
	query(open(1), "video.isolation.invalid", 1)
	if _, err := v.ApplyRouteRuntime(makeInfos(updated)); err != nil {
		t.Fatal(err)
	}
	query(oldUS, "fresh.isolation.invalid", 2)
	query(open(2), "fresh.isolation.invalid", 3)
}

func TestRuntimeDNSIsolatedEncryptedShadowsocks(t *testing.T) {
	limiter.Init()
	jp := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 1})
	us := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 2})
	updated := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 3})
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				marker := c.LocalAddr().(*net.TCPAddr).IP.To4()[3]
				var b [4]byte
				for {
					if _, err := io.ReadFull(c, b[:]); err != nil {
						return
					}
					if _, err := c.Write(append([]byte{marker}, b[:]...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	defer func() { listener.Close(); <-done }()
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("custom=%t", custom), func(t *testing.T) {
			infos := []*panel.NodeInfo{isolatedDNSNode(1, jp), isolatedDNSNode(2, us)}
			users := []panel.UserInfo{{Id: 42, Uuid: "dns-isolation-test-password"}}
			for _, info := range infos {
				info.Type = "shadowsocks"
				info.Common.Protocol = "shadowsocks"
				info.Common.Cipher = "aes-128-gcm"
				info.Common.ListenIP = "127.0.0.1"
				reservation, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				info.Common.ServerPort = reservation.Addr().(*net.TCPAddr).Port
				reservation.Close()

				// Permit only the loopback fixture; production private-IP blocking stays unchanged.
				value := `{"tag":"shared-dns-direct","protocol":"freedom","settings":{"targetStrategy":"ForceIPv4","finalRules":[{"action":"allow","ip":["127.0.0.0/8"]}]}}`
				if custom {
					value = `{"tag":"shared-dns-direct","protocol":"freedom","settings":{"finalRules":[{"action":"allow","ip":["127.0.0.0/8"]}]},"streamSettings":{"sockopt":{"domainStrategy":"ForceIPv4"}}}`
				}
				info.Common.Routes = append(info.Common.Routes, panel.Route{Action: "route", Match: []string{"domain:isolation.invalid"}, ActionValue: &value})
				limiter.AddLimiter("shadowsocks", info.Tag, users, nil, nil, true)
				defer limiter.DeleteLimiter(info.Tag)
			}
			v := New(conf.New())
			if err := v.Start(infos); err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			for _, info := range infos {
				if _, err := v.AddNodeWithUsers(info.Tag, info, users); err != nil {
					t.Fatal(err)
				}
			}
			account, err := (&shadowsocks.Account{Password: users[0].Uuid, CipherType: shadowsocks.CipherType_AES_128_GCM}).AsAccount()
			if err != nil {
				t.Fatal(err)
			}
			user := &protocol.MemoryUser{Account: account}
			type stream struct {
				conn   net.Conn
				writer buf.Writer
				reader io.Reader
			}
			open := func(id int) *stream {
				t.Helper()
				conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", infos[id-1].Common.ServerPort), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
				t.Cleanup(func() { conn.Close() })
				writer, err := shadowsocks.WriteTCPRequest(&protocol.RequestHeader{Command: protocol.RequestCommandTCP, Address: xnet.DomainAddress("video.isolation.invalid"), Port: xnet.Port(listener.Addr().(*net.TCPAddr).Port), User: user}, conn)
				if err != nil {
					t.Fatal(err)
				}
				var reader io.Reader
				lazy := isolatedDNSReader(func(p []byte) (int, error) {
					if reader == nil {
						r, err := shadowsocks.ReadTCPResponse(user, conn)
						if err != nil {
							return 0, err
						}
						reader = &buf.BufferedReader{Reader: r}
					}
					return reader.Read(p)
				})
				return &stream{conn: conn, writer: writer, reader: lazy}
			}
			check := func(s *stream, want byte) {
				t.Helper()
				b := buf.New()
				_, _ = b.Write([]byte("ping"))
				if err := s.writer.WriteMultiBuffer(buf.MultiBuffer{b}); err != nil {
					t.Fatal(err)
				}
				var response [5]byte
				if _, err := io.ReadFull(s.reader, response[:]); err != nil {
					t.Fatal(err)
				}
				if response[0] != want || string(response[1:]) != "ping" {
					t.Fatalf("encrypted stream got=%v want marker=%d", response, want)
				}
			}
			// Write the first body before constructing the response reader (it reads salt).
			// The reader is lazy because the SS server returns its salt after the body.
			a := open(1)
			check(a, 1)
			b := open(2)
			check(b, 2)
			replacement := isolatedDNSNode(2, updated).Common.Routes[0]
			infos[1].Common.Routes[0] = replacement
			if _, err := v.ApplyRouteRuntime(infos); err != nil {
				t.Fatal(err)
			}
			check(b, 2)
			c := open(2)
			check(c, 3)
			check(a, 1)
		})
	}
}

type isolatedDNSReader func([]byte) (int, error)

func (r isolatedDNSReader) Read(p []byte) (int, error) { return r(p) }

func TestRuntimeDNSRoutingAndUnconfiguredNode(t *testing.T) {
	upstream := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 2})
	infos := []*panel.NodeInfo{isolatedDNSNode(1, upstream), routeCompileNode(2)}
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
	group := lease.DNS().(*routeNodeDNS)
	if group.ForInbound("node-1") == group.ForInbound("node-2") || group.ForInbound("unknown") != group.fallback {
		t.Fatal("unconfigured or unknown inbound inherited custom DNS")
	}
	if group.ForInbound("node-1") != group.ForInbound(StreamUnlockProbeTag("node-1")) {
		t.Fatal("probe does not inherit its node")
	}
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "node-1"})
	ctx = lease.Context(ctx)
	ctx = session.ContextWithOutbounds(ctx, []*session.Outbound{{Target: xnet.TCPDestination(xnet.DomainAddress("video.isolation.invalid"), 443)}})
	// The router wraps the context a second time for IPIfNonMatch/IPOnDemand.
	routingCtx := routingdns.ContextWithDNSClient(routingsession.AsRoutingContext(ctx), v.routeDNS)
	routingCtx = routingdns.ContextWithDNSClient(routingCtx, v.routeDNS)
	ips := routingCtx.GetTargetIPs()
	if len(ips) != 1 || ips[0].String() != "127.0.0.2" {
		t.Fatalf("routing resolution lost node context: %v", ips)
	}

	// A loopback/detour may retag the session, but DNS belongs to its ingress.
	retagged := session.ContextWithInbound(ctx, &session.Inbound{Tag: "node-2"})
	if got := queryIsolatedDNS(t, v, retagged, "nested.isolation.invalid"); got != "127.0.0.2" {
		t.Fatal("nested dispatch changed its owning node DNS")
	}
	// Removing a DNS route must remove its cached answers from new connections.
	if _, err := v.ApplyRouteRuntime([]*panel.NodeInfo{routeCompileNode(1), routeCompileNode(2)}); err != nil {
		t.Fatal(err)
	}
	current, err := v.routeRuntime.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer current.Release()
	next := current.DNS().(*routeNodeDNS)
	if next.ForInbound("node-1") == group.ForInbound("node-1") {
		t.Fatal("removed DNS route retained its cache")
	}
	if got := queryIsolatedDNS(t, v, ctx, "video.isolation.invalid"); got != "127.0.0.2" {
		t.Fatal("old generation lost its resolver")
	}
}

func TestRuntimeDNSConcurrentIsolationDuringReload(t *testing.T) {
	jp := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 1})
	us := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 2})
	updated := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 3})
	v := New(conf.New())
	if err := v.Start([]*panel.NodeInfo{isolatedDNSNode(1, jp), isolatedDNSNode(2, us)}); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	var workers sync.WaitGroup
	failures := make(chan error, 100)
	start := make(chan struct{})
	for _, id := range []int{1, 2} {
		workers.Add(1)
		go func(id int) {
			defer workers.Done()
			<-start
			for i := 0; i < 50; i++ {
				lease, err := v.routeRuntime.Acquire(context.Background())
				if err != nil {
					failures <- err
					return
				}
				want := fmt.Sprintf("127.0.0.%d", id)
				if id == 2 && lease.ID() > 0 {
					want = "127.0.0.3"
				}
				ctx := lease.Context(session.ContextWithInbound(context.Background(), &session.Inbound{Tag: fmt.Sprintf("node-%d", id)}))
				ips, _, err := featuredns.LookupIP(ctx, v.routeDNS, "concurrent.isolation.invalid", featuredns.IPOption{IPv4Enable: true})
				lease.Release()
				if err != nil || len(ips) != 1 || ips[0].String() != want {
					failures <- fmt.Errorf("node=%d want=%s got=%v error=%v", id, want, ips, err)
					return
				}
			}
		}(id)
	}
	close(start)
	if _, err := v.ApplyRouteRuntime([]*panel.NodeInfo{isolatedDNSNode(1, jp), isolatedDNSNode(2, updated)}); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	// A rejected candidate must leave the published node resolvers usable.
	invalid := isolatedDNSNode(2, us)
	value := `{"servers":[{"address":"127.0.0.1"}],"script":"function {"}`
	invalid.Common.Routes[0].ActionValue = &value
	if _, err := v.ApplyRouteRuntime([]*panel.NodeInfo{isolatedDNSNode(1, jp), invalid}); err == nil {
		t.Fatal("invalid DNS candidate was published")
	}
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "node-2"})
	if got := queryIsolatedDNS(t, v, ctx, "rollback.isolation.invalid"); got != "127.0.0.3" {
		t.Fatalf("failed update replaced live DNS: %s", got)
	}
}

func TestRuntimeDNSSingleNodeDoesNotLendDNSToUnknownInbound(t *testing.T) {
	upstream := startIsolatedDNSUpstream(t, [4]byte{127, 0, 0, 2})
	v := New(conf.New())
	if err := v.Start([]*panel.NodeInfo{isolatedDNSNode(1, upstream)}); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	lease, err := v.routeRuntime.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	group := lease.DNS().(*routeNodeDNS)
	if group.ForInbound("removed-node") != group.fallback || group.ForInbound("removed-node") == group.ForInbound("node-1") {
		t.Fatal("unknown inbound borrowed the sole node's DNS")
	}
	ips, _, err := v.routeDNS.LookupIP("legacy.isolation.invalid", featuredns.IPOption{IPv4Enable: true})
	if err != nil || len(ips) != 1 || ips[0].String() != "127.0.0.2" {
		t.Fatalf("single-node context-free compatibility lost: %v %v", ips, err)
	}
}
