package core

import (
	"fmt"
	"strings"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/app/proxyman"
	xraycore "github.com/xtls/xray-core/core"
)

func TestCompileRouteRuntimeRejectsConflictingOutboundTag(t *testing.T) {
	a := `{"tag":"proxy-a","protocol":"freedom","settings":{}}`
	b := `{"tag":"proxy-a","protocol":"blackhole","settings":{}}`
	infos := []*panel.NodeInfo{
		routeCompileNode(1, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &a}),
		routeCompileNode(2, panel.Route{Id: 11, Action: "route", Match: []string{"domain:b.test"}, ActionValue: &b}),
	}
	_, err := CompileRouteRuntime(infos)
	if err == nil || !strings.Contains(err.Error(), "node_id=2") || !strings.Contains(err.Error(), "action=route") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompileRouteRuntimeReusesEquivalentOutboundAndRoutesToPhysicalTag(t *testing.T) {
	a := `{"tag":"proxy-a","protocol":"freedom","settings":{}}`
	b := `{ "protocol": "freedom", "settings": {}, "tag": "proxy-a" }`
	compiled, err := CompileRouteRuntime([]*panel.NodeInfo{
		routeCompileNode(1, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &a}),
		routeCompileNode(2, panel.Route{Id: 11, Action: "route", Match: []string{"domain:b.test"}, ActionValue: &b}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.CustomOutbounds) != 1 {
		t.Fatalf("custom outbounds=%d, want 1", len(compiled.CustomOutbounds))
	}
	physical := compiled.LogicalToPhysical["proxy-a"]
	if physical == "" || physical == "proxy-a" || !strings.HasPrefix(physical, "proxy-a@") {
		t.Fatalf("physical tag=%q", physical)
	}
	if compiled.CustomOutbounds[0].Tag != physical {
		t.Fatalf("outbound tag=%q, want %q", compiled.CustomOutbounds[0].Tag, physical)
	}
	var matched int
	for _, rule := range compiled.RouterConfig.GetRule() {
		if rule.GetTag() == physical {
			matched++
		}
	}
	if matched != 2 {
		t.Fatalf("router rules targeting %q=%d, want 2", physical, matched)
	}
	if compiled.ConfigHash == "" {
		t.Fatal("config hash is empty")
	}

	again, err := CompileRouteRuntime([]*panel.NodeInfo{
		routeCompileNode(1, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &a}),
		routeCompileNode(2, panel.Route{Id: 11, Action: "route", Match: []string{"domain:b.test"}, ActionValue: &b}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ConfigHash != compiled.ConfigHash || again.LogicalToPhysical["proxy-a"] != physical {
		t.Fatalf("compile is not deterministic: first=%q second=%q", compiled.ConfigHash, again.ConfigHash)
	}
}

func TestCompileRouteRuntimePreservesRuleOrder(t *testing.T) {
	value := `{"tag":"proxy-a","protocol":"freedom","settings":{}}`
	compiled, err := CompileRouteRuntime([]*panel.NodeInfo{
		routeCompileNode(1,
			panel.Route{Id: 10, Action: "route", Match: []string{"domain:first.test"}, ActionValue: &value},
			panel.Route{Id: 11, Action: "block", Match: []string{"domain:second.test"}},
			panel.Route{Id: 12, Action: "route", Match: []string{"domain:third.test"}, ActionValue: &value},
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	physical := compiled.LogicalToPhysical["proxy-a"]
	rules := compiled.RouterConfig.GetRule()
	if len(rules) != 4 {
		t.Fatalf("router rules=%d, want DNS plus 3 user rules", len(rules))
	}
	got := []string{rules[1].GetTag(), rules[2].GetTag(), rules[3].GetTag()}
	want := []string{physical, "block", physical}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rule order=%v, want %v", got, want)
		}
	}
}

func TestCompileRouteRuntimeRewritesOutboundDependencies(t *testing.T) {
	child := `{"tag":"proxy-a","protocol":"freedom","settings":{}}`
	proxyParent := `{"tag":"proxy-b","protocol":"freedom","settings":{},"proxySettings":{"tag":"proxy-a"}}`
	dialerParent := `{"tag":"proxy-c","protocol":"freedom","settings":{},"streamSettings":{"sockopt":{"dialerProxy":"proxy-a"}}}`
	compiled, err := CompileRouteRuntime([]*panel.NodeInfo{
		routeCompileNode(1,
			panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &child},
			panel.Route{Id: 11, Action: "route", Match: []string{"domain:b.test"}, ActionValue: &proxyParent},
			panel.Route{Id: 12, Action: "route", Match: []string{"domain:c.test"}, ActionValue: &dialerParent},
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	childPhysical := compiled.LogicalToPhysical["proxy-a"]
	proxyParentPhysical := compiled.LogicalToPhysical["proxy-b"]
	proxyParentOutbound := findCompiledOutbound(compiled.CustomOutbounds, proxyParentPhysical)
	if proxyParentOutbound == nil || proxyParentOutbound.SenderSettings == nil {
		t.Fatalf("proxy parent outbound not found: %q", proxyParentPhysical)
	}
	instance, err := proxyParentOutbound.SenderSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	sender, ok := instance.(*proxyman.SenderConfig)
	if !ok {
		t.Fatalf("sender settings type=%T", instance)
	}
	if sender.GetProxySettings().GetTag() != childPhysical {
		t.Fatalf("proxySettings.tag=%q, want %q", sender.GetProxySettings().GetTag(), childPhysical)
	}

	dialerParentPhysical := compiled.LogicalToPhysical["proxy-c"]
	dialerParentOutbound := findCompiledOutbound(compiled.CustomOutbounds, dialerParentPhysical)
	if dialerParentOutbound == nil || dialerParentOutbound.SenderSettings == nil {
		t.Fatalf("dialer parent outbound not found: %q", dialerParentPhysical)
	}
	instance, err = dialerParentOutbound.SenderSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	sender, ok = instance.(*proxyman.SenderConfig)
	if !ok {
		t.Fatalf("sender settings type=%T", instance)
	}
	if sender.GetStreamSettings().GetSocketSettings().GetDialerProxy() != childPhysical {
		t.Fatalf("dialerProxy=%q, want %q", sender.GetStreamSettings().GetSocketSettings().GetDialerProxy(), childPhysical)
	}
}

func TestCompileRouteRuntimeRejectsMissingOutboundDependency(t *testing.T) {
	value := `{"tag":"proxy-b","protocol":"freedom","settings":{},"proxySettings":{"tag":"missing"}}`
	_, err := CompileRouteRuntime([]*panel.NodeInfo{
		routeCompileNode(7, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &value}),
	})
	if err == nil || !strings.Contains(err.Error(), "node_id=7") || !strings.Contains(err.Error(), "missing outbound dependency") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetCustomConfigSkipsInvalidLegacyOutboundDependency(t *testing.T) {
	value := `{"tag":"proxy-b","protocol":"freedom","settings":{},"proxySettings":{"tag":"missing"}}`
	dnsConfig, outbounds, routerConfig, err := GetCustomConfig([]*panel.NodeInfo{
		routeCompileNode(7, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &value}),
	})
	if err != nil {
		t.Fatalf("compatible startup rejected legacy route: %v", err)
	}
	if dnsConfig == nil || routerConfig == nil {
		t.Fatal("compatible startup did not return base DNS/router config")
	}
	if len(outbounds) != 3 {
		t.Fatalf("outbounds=%d, want only 3 builtins", len(outbounds))
	}
}

func TestCompileRouteRuntimeRejectsInvalidValueWithoutLeakingIt(t *testing.T) {
	secret := "token-top-secret"
	_, err := CompileRouteRuntime([]*panel.NodeInfo{
		routeCompileNode(3, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &secret}),
	})
	if err == nil || !strings.Contains(err.Error(), "node_id=3") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked action value: %v", err)
	}
}

func TestCompileRouteRuntimeRejectsReservedOutboundTag(t *testing.T) {
	value := `{"tag":"Default","protocol":"freedom","settings":{}}`
	_, err := CompileRouteRuntime([]*panel.NodeInfo{
		routeCompileNode(4, panel.Route{Id: 10, Action: "default_out", ActionValue: &value}),
	})
	if err == nil || !strings.Contains(err.Error(), "reserved outbound tag") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func routeCompileNode(id int, routes ...panel.Route) *panel.NodeInfo {
	return &panel.NodeInfo{
		Id:   id,
		Type: "vless",
		Tag:  fmt.Sprintf("node-%d", id),
		Common: &panel.CommonNode{
			Protocol: "vless",
			Routes:   routes,
		},
	}
}

func findCompiledOutbound(outbounds []*xraycore.OutboundHandlerConfig, tag string) *xraycore.OutboundHandlerConfig {
	for _, outbound := range outbounds {
		if outbound != nil && outbound.Tag == tag {
			return outbound
		}
	}
	return nil
}
