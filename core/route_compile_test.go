package core

import (
	"fmt"
	"strings"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/app/dns"
	"github.com/xtls/xray-core/app/proxyman"
	xraycore "github.com/xtls/xray-core/core"
	"google.golang.org/protobuf/proto"
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

func TestCompileRouteRuntimeLogicalTagReferences(t *testing.T) {
	for _, pool := range []bool{false, true} {
		for _, forward := range []bool{false, true} {
			t.Run(fmt.Sprintf("pool=%t/forward=%t", pool, forward), func(t *testing.T) {
				value := `{"tag":"shared-exit","protocol":"shadowsocks","settings":{"servers":[{"address":"127.0.0.1","port":19001,"method":"aes-128-gcm","password":"test-only"}]}}`
				if pool {
					value = strings.Replace(value, `}]}`, `},{"address":"127.0.0.1","port":19002,"method":"aes-128-gcm","password":"test-only-two"}]}`, 1)
				}
				ref := "shared-exit"
				definition := panel.Route{Id: 1, Action: "route", Match: []string{"domain:one.invalid"}, ActionValue: &value}
				reference := panel.Route{Id: 2, Action: "route", Match: []string{"domain:two.invalid"}, ActionValue: &ref}
				routes := []panel.Route{definition, reference}
				if forward {
					routes[0], routes[1] = routes[1], routes[0]
				}
				compiled, err := CompileRouteRuntime([]*panel.NodeInfo{routeCompileNode(1, routes...)})
				if err != nil {
					t.Fatal(err)
				}
				for _, rule := range compiled.RouterConfig.Rule[1:] {
					got := rule.GetTag()
					if pool {
						got = rule.GetBalancingTag()
					}
					if got != compiled.LogicalToPhysical[ref] {
						t.Fatal("reference did not target the same compiled outbound or pool")
					}
				}
			})
		}
	}
	for _, tag := range []string{"Default", "block", "dns_out"} {
		compiled, err := CompileRouteRuntime([]*panel.NodeInfo{routeCompileNode(1, panel.Route{Action: "default_out", ActionValue: &tag})})
		if err != nil || compiled.RouterConfig.Rule[1].GetTag() != tag || len(compiled.CustomOutbounds) != 0 {
			t.Fatalf("built-in reference failed: %v", err)
		}
	}
	missing := "missing-exit"
	if _, err := CompileRouteRuntime([]*panel.NodeInfo{routeCompileNode(1, panel.Route{Action: "default_out", ActionValue: &missing})}); err == nil {
		t.Fatal("a reference cannot replace its only definition")
	}
}

func TestCompileRouteRuntimeShadowsocksMultipleServers(t *testing.T) {
	value := `{"tag":"test-pool","protocol":"Shadowsocks","settings":{"servers":[{"address":"127.0.0.1","port":19001,"method":"aes-128-gcm","password":"test-only-a"},{"address":"127.0.0.1","port":19002,"method":"aes-128-gcm","password":"test-only-b"}]}}`
	infos := []*panel.NodeInfo{routeCompileNode(1, panel.Route{Id: 52, Action: "route", Match: []string{"domain:media.invalid"}, ActionValue: &value})}
	compiled, err := CompileRouteRuntime(infos)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.CustomOutbounds) != 2 || len(compiled.RouterConfig.BalancingRule) != 1 {
		t.Fatalf("outbounds=%d balancers=%d, want 2 and 1", len(compiled.CustomOutbounds), len(compiled.RouterConfig.BalancingRule))
	}
	b := compiled.RouterConfig.BalancingRule[0]
	if b.Strategy != "roundrobin" || len(b.OutboundSelector) != 2 {
		t.Fatalf("unexpected balancer: %v", b)
	}
	if got := compiled.RouterConfig.Rule[1].GetBalancingTag(); got != b.Tag {
		t.Fatalf("rule balancer=%q want %q", got, b.Tag)
	}
	for _, tag := range b.OutboundSelector {
		if findCompiledOutbound(compiled.CustomOutbounds, tag) == nil {
			t.Fatalf("balancer references missing physical outbound %q", tag)
		}
	}
	again, err := CompileRouteRuntime(infos)
	if err != nil || again.ConfigHash != compiled.ConfigHash {
		t.Fatalf("pool compilation is not deterministic: %v", err)
	}
	// A shared definition remains one pool, while each inbound keeps its rule.
	infos = append(infos, routeCompileNode(2, panel.Route{Id: 52, Action: "route", Match: []string{"domain:media.invalid"}, ActionValue: &value}))
	shared, err := CompileRouteRuntime(infos)
	if err != nil || len(shared.CustomOutbounds) != 2 || len(shared.RouterConfig.BalancingRule) != 1 || len(shared.RouterConfig.Rule) != 3 {
		t.Fatalf("shared pool was not deduplicated: %v", err)
	}
}

func TestCompileRouteRuntimeShadowsocksSingleServerUnchanged(t *testing.T) {
	for _, value := range []string{
		`{"tag":"legacy-ss","protocol":"Shadowsocks","settings":{"servers":[{"address":"127.0.0.1","port":19001,"method":"aes-128-gcm","password":"test-only"}]}}`,
		`{"tag":"legacy-ss","protocol":"shadowsocks","settings":{"address":"127.0.0.1","port":19001,"method":"aes-128-gcm","password":"test-only"}}`,
	} {
		out, err := decodeOutboundConfig(value)
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := out.Build()
		if err != nil {
			t.Fatal(err)
		}
		physical, err := physicalOutboundTag(out.Tag, legacy)
		if err != nil {
			t.Fatal(err)
		}
		legacy.Tag = physical
		compiled, err := CompileRouteRuntime([]*panel.NodeInfo{routeCompileNode(1, panel.Route{Id: 52, Action: "route", Match: []string{"domain:media.invalid"}, ActionValue: &value})})
		if err != nil {
			t.Fatal(err)
		}
		if len(compiled.CustomOutbounds) != 1 || len(compiled.RouterConfig.BalancingRule) != 0 || !proto.Equal(compiled.CustomOutbounds[0], legacy) || compiled.LogicalToPhysical[out.Tag] != physical || compiled.RouterConfig.Rule[1].GetTag() != physical {
			t.Fatal("single-server config, physical tag, or route changed")
		}
	}
}

func TestCompileRouteRuntimeShadowsocksGroupValidation(t *testing.T) {
	member := `{"address":"127.0.0.1","port":19001,"method":"aes-128-gcm","password":"test-only"}`
	for _, tc := range []struct{ name, settings string }{
		{"null-member", `{"servers":[` + member + `,null]}`},
		{"invalid-member", `{"servers":[` + member + `,{"password":"never-echo-this-secret"}]}`},
		{"mixed-address", `{"address":"127.0.0.1","servers":[` + member + `,` + member + `]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := `{"tag":"pool","protocol":"shadowsocks","settings":` + tc.settings + `}`
			_, err := CompileRouteRuntime([]*panel.NodeInfo{routeCompileNode(1, panel.Route{Id: 52, Action: "route", Match: []string{"domain:media.invalid"}, ActionValue: &value})})
			if err == nil || strings.Contains(err.Error(), "never-echo-this-secret") {
				t.Fatalf("expected safe group validation error, got %v", err)
			}
		})
	}
}

func TestCompileRouteRuntimeShadowsocksGroupDependenciesAndConflict(t *testing.T) {
	value := `{"tag":"pool","protocol":"shadowsocks","settings":{"servers":[{"address":"127.0.0.1","port":19001,"method":"aes-128-gcm","password":"test-a"},{"address":"127.0.0.1","port":19002,"method":"aes-128-gcm","password":"test-b"}]},"streamSettings":{"sockopt":{"dialerProxy":"upstream"}}}`
	upstream := `{"tag":"upstream","protocol":"freedom","settings":{}}`
	makeInfos := func(v string) []*panel.NodeInfo {
		return []*panel.NodeInfo{routeCompileNode(1,
			panel.Route{Id: 52, Action: "route", Match: []string{"domain:media.invalid"}, ActionValue: &v},
			panel.Route{Id: 53, Action: "route", Match: []string{"domain:other.invalid"}, ActionValue: &upstream},
		)}
	}
	compiled, err := CompileRouteRuntime(makeInfos(value))
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range compiled.RouterConfig.BalancingRule[0].OutboundSelector {
		out := findCompiledOutbound(compiled.CustomOutbounds, tag)
		cfg, err := out.SenderSettings.GetInstance()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.(*proxyman.SenderConfig).GetStreamSettings().GetSocketSettings().GetDialerProxy() != compiled.LogicalToPhysical["upstream"] {
			t.Fatal("pool member lost its upstream dependency")
		}
	}
	changed := strings.ReplaceAll(value, "19002", "19003")
	infos := append(makeInfos(value), routeCompileNode(2, panel.Route{Id: 54, Action: "route", Match: []string{"domain:media.invalid"}, ActionValue: &changed}))
	if _, err := CompileRouteRuntime(infos); err == nil || !strings.Contains(err.Error(), "conflicting outbound tag") {
		t.Fatalf("expected pool conflict: %v", err)
	}
	// Compatibility startup drops an invalid pool as a unit, without orphaned members.
	missing := strings.ReplaceAll(value, `"upstream"`, `"missing"`)
	compatible, err := compileRouteRuntime(makeInfos(missing), false)
	if err != nil || len(compatible.CustomOutbounds) != 1 || len(compatible.RouterConfig.BalancingRule) != 0 {
		t.Fatalf("invalid pool was only partially removed: %v", err)
	}
	parent := `{"tag":"dependent","protocol":"freedom","settings":{},"streamSettings":{"sockopt":{"dialerProxy":"pool"}}}`
	infos = append(makeInfos(value), routeCompileNode(2, panel.Route{Id: 55, Action: "route", Match: []string{"domain:media.invalid"}, ActionValue: &parent}))
	if _, err := CompileRouteRuntime(infos); err == nil || !strings.Contains(err.Error(), "cannot reference a load-balancing group") {
		t.Fatalf("expected explicit unsupported group dependency error: %v", err)
	}
}

func TestCompileRouteRuntimeShadowsocksGroupRouteActions(t *testing.T) {
	value := `{"tag":"pool","protocol":"shadowsocks","settings":{"servers":[{"address":"127.0.0.1","port":19001,"method":"aes-128-gcm","password":"test-a"},{"address":"127.0.0.1","port":19002,"method":"aes-128-gcm","password":"test-b"}]}}`
	for _, tc := range []struct {
		action string
		match  []string
	}{
		{"route", []string{"domain:media.invalid"}},
		{"route_ip", []string{"192.0.2.0/24"}},
		{"default_out", nil},
	} {
		t.Run(tc.action, func(t *testing.T) {
			compiled, err := CompileRouteRuntime([]*panel.NodeInfo{routeCompileNode(1, panel.Route{Id: 52, Action: tc.action, Match: tc.match, ActionValue: &value})})
			if err != nil {
				t.Fatal(err)
			}
			if compiled.RouterConfig.Rule[1].GetBalancingTag() != compiled.RouterConfig.BalancingRule[0].Tag {
				t.Fatal("route does not target pool")
			}
		})
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
	if sender.GetStreamSettings().GetSocketSettings().GetDialerProxy() != childPhysical {
		t.Fatalf("migrated proxySettings dialerProxy=%q, want %q", sender.GetStreamSettings().GetSocketSettings().GetDialerProxy(), childPhysical)
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

func TestCompileRouteRuntimeIsolatesNodeDNSOptionsAndHash(t *testing.T) {
	jp := `{"servers":[{"address":"192.0.2.1"}],"queryStrategy":"UseIPv4","disableCache":true,"disableFallbackIfMatch":true}`
	us := `{"servers":[{"address":"192.0.2.2"}],"queryStrategy":"UseIPv6","disableCache":false,"disableFallbackIfMatch":false}`
	a := routeCompileNode(1, panel.Route{Action: "dns", Match: []string{"domain:video.test"}, ActionValue: &jp})
	b := routeCompileNode(2, panel.Route{Action: "dns", Match: []string{"domain:video.test"}, ActionValue: &us})
	c := routeCompileNode(3)
	compile := func(infos ...*panel.NodeInfo) *CompiledRouteRuntime {
		t.Helper()
		v, err := CompileRouteRuntime(infos)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := compile(a, b, c)
	if len(first.NodeDNSConfigs) != 3 {
		t.Fatal("missing per-node DNS config")
	}
	for tag, want := range map[string]dns.QueryStrategy{"node-1": dns.QueryStrategy_USE_IP4, "node-2": dns.QueryStrategy_USE_IP6} {
		if first.NodeDNSConfigs[tag].QueryStrategy != want {
			t.Fatalf("%s query strategy leaked", tag)
		}
	}
	if !first.NodeDNSConfigs[a.Tag].DisableCache || first.NodeDNSConfigs[b.Tag].DisableCache {
		t.Fatal("cache option leaked between nodes")
	}
	if !first.NodeDNSConfigs[a.Tag].DisableFallbackIfMatch || first.NodeDNSConfigs[b.Tag].DisableFallbackIfMatch {
		t.Fatal("fallback option leaked between nodes")
	}
	if len(first.NodeDNSConfigs[c.Tag].NameServer) != 1 || len(first.DNSConfig.NameServer) != 1 {
		t.Fatal("unconfigured node inherited another node's resolver")
	}
	if reordered := compile(c, b, a); reordered.ConfigHash != first.ConfigHash {
		t.Fatal("node order changed DNS-only config hash")
	}
	a.Common.Routes[0].ActionValue = &us
	b.Common.Routes[0].ActionValue = &jp
	if switched := compile(a, b, c); switched.ConfigHash == first.ConfigHash {
		t.Fatal("swapping per-node DNS bindings did not change runtime hash")
	}
}

func TestGetCustomConfigPreservesSingleNodeDNS(t *testing.T) {
	value := `{"servers":[{"address":"192.0.2.1"}],"disableCache":true}`
	config, _, _, err := GetCustomConfig([]*panel.NodeInfo{routeCompileNode(1, panel.Route{Action: "dns", ActionValue: &value})})
	if err != nil || config == nil || !config.DisableCache || len(config.NameServer) != 2 {
		t.Fatalf("single-node DNS compatibility lost: config=%v error=%v", config, err)
	}
}
