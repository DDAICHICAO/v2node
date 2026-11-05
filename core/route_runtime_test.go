package core

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	xraycore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	routingsession "github.com/xtls/xray-core/features/routing/session"
	"github.com/xtls/xray-core/proxy/shadowsocks"
)

func TestApplyRouteRuntimeKeepsCoreAndInboundManagers(t *testing.T) {
	oldValue := `{"tag":"proxy-a","protocol":"freedom","settings":{}}`
	oldInfos := []*panel.NodeInfo{
		routeCompileNode(1, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &oldValue}),
	}
	v := New(conf.New())
	if err := v.Start(oldInfos); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	serverBefore := v.Server
	dispatcherBefore := v.dispatcher
	inboundBefore := v.ihm
	oldCompiled, err := CompileRouteRuntime(oldInfos)
	if err != nil {
		t.Fatal(err)
	}
	oldPhysical := oldCompiled.LogicalToPhysical["proxy-a"]
	oldLease, err := v.routeRuntime.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	newValue := `{"tag":"proxy-a","protocol":"freedom","settings":{"domainStrategy":"UseIPv4"}}`
	newInfos := []*panel.NodeInfo{
		routeCompileNode(1, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &newValue}),
	}
	generation, err := v.ApplyRouteRuntime(newInfos)
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 || v.routeRuntime.Stats().ActiveGeneration != 1 {
		t.Fatalf("generation=%d stats=%+v", generation, v.routeRuntime.Stats())
	}
	if v.Server != serverBefore || v.dispatcher != dispatcherBefore || v.ihm != inboundBefore {
		t.Fatal("route update replaced Core, dispatcher, or inbound manager")
	}
	if v.ohm.GetHandler(oldPhysical) == nil {
		t.Fatal("old outbound was removed while old lease remained")
	}
	oldLease.Release()
	waitForOutboundRemoval(t, v, oldPhysical)

	invalidA := `{"tag":"dup","protocol":"freedom","settings":{}}`
	invalidB := `{"tag":"dup","protocol":"blackhole","settings":{}}`
	invalid := []*panel.NodeInfo{
		routeCompileNode(1, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &invalidA}),
		routeCompileNode(2, panel.Route{Id: 11, Action: "route", Match: []string{"domain:b.test"}, ActionValue: &invalidB}),
	}
	if _, err := v.ApplyRouteRuntime(invalid); err == nil {
		t.Fatal("invalid candidate was published")
	}
	if got := v.routeRuntime.Stats().ActiveGeneration; got != 1 {
		t.Fatalf("invalid candidate advanced generation to %d", got)
	}
}

func TestRouteRuntimeShadowsocksRoundRobinAndHotReload(t *testing.T) {
	value := `{"tag":"pool","protocol":"shadowsocks","settings":{"servers":[{"address":"127.0.0.1","port":19001,"method":"aes-128-gcm","password":"test-only-a"},{"address":"127.0.0.1","port":19002,"method":"aes-128-gcm","password":"test-only-b"}]}}`
	makeInfos := func(raw string) []*panel.NodeInfo {
		return []*panel.NodeInfo{routeCompileNode(1, panel.Route{Id: 52, Action: "route", Match: []string{"domain:media.invalid"}, ActionValue: &raw})}
	}
	infos := makeInfos(value)
	v := New(conf.New())
	if err := v.Start(infos); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	oldCompiled, err := CompileRouteRuntime(infos)
	if err != nil {
		t.Fatal(err)
	}
	old, err := v.routeRuntime.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	pick := func(lease interface{ Router() routing.Router }, allowed []string) {
		t.Helper()
		counts := map[string]int{}
		for i := 0; i < 12; i++ {
			ctx := &routingsession.Context{Inbound: &session.Inbound{Tag: "node-1"}, Outbound: &session.Outbound{Target: xnet.TCPDestination(xnet.DomainAddress("video.media.invalid"), 443)}}
			route, err := lease.Router().PickRoute(ctx)
			if err != nil {
				t.Fatal(err)
			}
			counts[route.GetOutboundTag()]++
		}
		if len(counts) != len(allowed) {
			t.Fatalf("selected unexpected members: %v", counts)
		}
		for _, tag := range allowed {
			if counts[tag] != 12/len(allowed) {
				t.Fatalf("unequal round-robin selection: %v", counts)
			}
		}
	}
	pick(old, oldCompiled.RouterConfig.BalancingRule[0].OutboundSelector)
	changed := strings.ReplaceAll(value, "19002", "19003")
	if generation, err := v.ApplyRouteRuntime(makeInfos(changed)); err != nil || generation != 1 {
		t.Fatalf("apply pool update: generation=%d err=%v", generation, err)
	}
	newCompiled, err := CompileRouteRuntime(makeInfos(changed))
	if err != nil {
		t.Fatal(err)
	}
	current, err := v.routeRuntime.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer current.Release()
	pick(current, newCompiled.RouterConfig.BalancingRule[0].OutboundSelector)
	// Retained generations must never select a new-generation member.
	pick(old, oldCompiled.RouterConfig.BalancingRule[0].OutboundSelector)
	invalid := strings.ReplaceAll(changed, `"port":19003`, `"port":0`)
	if _, err := v.ApplyRouteRuntime(makeInfos(invalid)); err == nil {
		t.Fatal("invalid pool member was accepted")
	}
	if v.routeRuntime.Stats().ActiveGeneration != 1 {
		t.Fatal("invalid pool update replaced the active generation")
	}
	pick(current, newCompiled.RouterConfig.BalancingRule[0].OutboundSelector)
	single := `{"tag":"pool","protocol":"shadowsocks","settings":{"servers":[{"address":"127.0.0.1","port":19001,"method":"aes-128-gcm","password":"test-only-a"}]}}`
	if generation, err := v.ApplyRouteRuntime(makeInfos(single)); err != nil || generation != 2 {
		t.Fatalf("restore single-server config: generation=%d err=%v", generation, err)
	}
	singleCompiled, err := CompileRouteRuntime(makeInfos(single))
	if err != nil {
		t.Fatal(err)
	}
	last, err := v.routeRuntime.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer last.Release()
	pick(last, []string{singleCompiled.LogicalToPhysical["pool"]})
	pick(current, newCompiled.RouterConfig.BalancingRule[0].OutboundSelector)
	old.Release()
	current.Release()
	for _, out := range append(oldCompiled.CustomOutbounds, newCompiled.CustomOutbounds...) {
		waitForOutboundRemoval(t, v, out.Tag)
	}
}

func TestRouteRuntimeShadowsocksPoolEncryptedTraffic(t *testing.T) {
	a := startRoutePoolSSPeer(t, 'A')
	b := startRoutePoolSSPeer(t, 'B')
	c := startRoutePoolSSPeer(t, 'C')
	makeInfos := func(first int) []*panel.NodeInfo {
		raw := fmt.Sprintf(`{"tag":"pool","protocol":"Shadowsocks","settings":{"servers":[{"address":"127.0.0.1","port":%d,"method":"aes-128-gcm","password":"test-only"},{"address":"127.0.0.1","port":%d,"method":"aes-128-gcm","password":"test-only"}]}}`, first, b)
		return []*panel.NodeInfo{routeCompileNode(1, panel.Route{Id: 52, Action: "route", Match: []string{"domain:media.invalid"}, ActionValue: &raw})}
	}
	v := New(conf.New())
	if err := v.Start(makeInfos(a)); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	open := func() (net.Conn, byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		t.Cleanup(cancel)
		ctx = session.ContextWithInbound(ctx, &session.Inbound{Tag: "node-1"})
		conn, err := xraycore.Dial(ctx, v.Server, xnet.TCPDestination(xnet.DomainAddress("video.media.invalid"), 443))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("ping")); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		var response [5]byte
		if _, err := io.ReadFull(conn, response[:]); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if string(response[1:]) != "ping" {
			conn.Close()
			t.Fatalf("invalid encrypted response %q", response)
		}
		return conn, response[0]
	}
	check := func(first byte) {
		t.Helper()
		counts := map[byte]int{}
		for i := 0; i < 6; i++ {
			conn, marker := open()
			counts[marker]++
			conn.Close()
		}
		if len(counts) != 2 || counts[first] != 3 || counts['B'] != 3 {
			t.Fatalf("encrypted traffic was not distributed equally: %v", counts)
		}
	}
	old, _ := open()
	defer old.Close()
	check('A')
	if _, err := v.ApplyRouteRuntime(makeInfos(c)); err != nil {
		t.Fatal(err)
	}
	check('C')
	// An already established encrypted stream must survive replacing its peer.
	if _, err := old.Write([]byte("stay")); err != nil {
		t.Fatal(err)
	}
	var echoed [4]byte
	if _, err := io.ReadFull(old, echoed[:]); err != nil || string(echoed[:]) != "stay" {
		t.Fatalf("old encrypted connection failed after pool update: %q %v", echoed, err)
	}
}

func startRoutePoolSSPeer(t *testing.T, marker byte) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	account, err := (&shadowsocks.Account{Password: "test-only", CipherType: shadowsocks.CipherType_AES_128_GCM}).AsAccount()
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	validator := new(shadowsocks.Validator)
	if err := validator.Add(&protocol.MemoryUser{Account: account, Email: "test-peer"}); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	var pending sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			pending.Add(1)
			go func() {
				defer pending.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				request, reader, err := shadowsocks.ReadTCPSession(validator, conn)
				if err != nil {
					t.Errorf("local SS authentication failed: %v", err)
					return
				}
				if request.Address.Domain() != "video.media.invalid" || request.Port != 443 {
					t.Error("SS peer received wrong destination")
					return
				}
				writer, err := shadowsocks.WriteTCPResponse(request, conn)
				if err != nil {
					t.Error(err)
					return
				}
				if err := writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte{marker})}); err != nil {
					return
				}
				_ = buf.Copy(reader, writer)
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-acceptDone; pending.Wait() })
	return listener.Addr().(*net.TCPAddr).Port
}

func TestStartKeepsCompatibleGenerationDegradedUntilStrictApply(t *testing.T) {
	legacyValue := `{"tag":"proxy-b","protocol":"freedom","settings":{},"proxySettings":{"tag":"missing"}}`
	legacy := []*panel.NodeInfo{
		routeCompileNode(7, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &legacyValue}),
	}
	v := New(conf.New())
	if err := v.Start(legacy); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	degraded, reason := v.RouteRuntimeHealth()
	if !degraded || !strings.Contains(reason, "missing outbound dependency") {
		t.Fatalf("degraded=%v reason=%q", degraded, reason)
	}
	if got := v.routeRuntime.Stats().ActiveGeneration; got != 0 {
		t.Fatalf("compatible start generation=%d, want 0", got)
	}

	fixedValue := `{"tag":"proxy-b","protocol":"freedom","settings":{}}`
	fixed := []*panel.NodeInfo{
		routeCompileNode(7, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &fixedValue}),
	}
	if _, err := v.ApplyRouteRuntime(fixed); err != nil {
		t.Fatal(err)
	}
	if degraded, reason = v.RouteRuntimeHealth(); degraded || reason != "" {
		t.Fatalf("degraded=%v reason=%q after valid apply", degraded, reason)
	}
}

func waitForOutboundRemoval(t *testing.T, v *V2Core, tag string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v.ohm.GetHandler(tag) == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("outbound %q was not removed", tag)
}
