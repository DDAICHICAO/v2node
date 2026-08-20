package core

import (
	"context"
	"strings"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
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
