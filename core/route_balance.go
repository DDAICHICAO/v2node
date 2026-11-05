package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	panel "github.com/wyx2685/v2node/api/v2board"
	coreConf "github.com/xtls/xray-core/infra/conf"
	"google.golang.org/protobuf/proto"
)

// Multi-server Shadowsocks is SNTP shorthand for separate single-server
// outbounds and a router balancer. Single-server JSON is passed through intact.
func parseShadowsocksRouteGroup(info *panel.NodeInfo, index int, route panel.Route, raw string) (*routeOutboundDefinition, error) {
	var header struct {
		Protocol string          `json:"protocol"`
		Tag      string          `json:"tag"`
		Settings json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal([]byte(raw), &header); err != nil {
		return nil, errors.New("invalid outbound JSON")
	}
	if !strings.EqualFold(header.Protocol, "shadowsocks") {
		return nil, nil
	}
	var pool struct {
		Servers []json.RawMessage `json:"servers"`
	}
	if len(header.Settings) == 0 || json.Unmarshal(header.Settings, &pool) != nil {
		return nil, nil // Leave existing single-outbound validation unchanged.
	}
	if len(pool.Servers) <= 1 {
		return nil, nil
	}
	logical := strings.TrimSpace(header.Tag)
	if logical == "" {
		return nil, errors.New("outbound tag is empty")
	}
	if _, reserved := reservedOutboundTags[logical]; reserved {
		return nil, errors.New("reserved outbound tag")
	}
	var root map[string]json.RawMessage
	var settings map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &root) != nil || json.Unmarshal(root["settings"], &settings) != nil {
		return nil, errors.New("invalid Shadowsocks group settings")
	}
	// New-style address/port settings take precedence in the core. Mixing them
	// with a pool would silently send all members to that one address.
	if address := settings["address"]; len(address) > 0 && string(address) != "null" {
		return nil, errors.New("Shadowsocks group cannot combine address and multiple servers")
	}
	group := &routeOutboundDefinition{logical: logical, raw: raw, info: info, index: index, route: route}
	sum := sha256.Sum256([]byte(logical))
	for i, server := range pool.Servers {
		var target map[string]json.RawMessage
		if json.Unmarshal(server, &target) != nil || target == nil {
			return nil, fmt.Errorf("invalid Shadowsocks group member %d: expected server object", i+1)
		}
		memberTag := fmt.Sprintf("sntp-member-%s-%d", hex.EncodeToString(sum[:]), i)
		// Preserve stream settings, chaining, mux, and other outbound options.
		settings["servers"], _ = json.Marshal([]json.RawMessage{server})
		root["settings"], _ = json.Marshal(settings)
		root["tag"], _ = json.Marshal(memberTag)
		memberJSON, err := json.Marshal(root)
		if err != nil {
			return nil, errors.New("cannot encode Shadowsocks group member")
		}
		member, err := parseSingleRouteOutboundDefinition(info, index, route, string(memberJSON))
		if err != nil {
			// Never include the supplied JSON or server password in errors.
			return nil, fmt.Errorf("invalid Shadowsocks group member %d: %w", i+1, err)
		}
		group.members = append(group.members, member)
	}
	return group, nil
}

func equivalentRouteOutbound(a, b *routeOutboundDefinition) bool {
	if len(a.members) != len(b.members) {
		return false
	}
	if len(a.members) == 0 {
		return proto.Equal(a.logicalProto, b.logicalProto)
	}
	for i := range a.members {
		if !equivalentRouteOutbound(a.members[i], b.members[i]) {
			return false
		}
	}
	return true
}

func registerRouteOutboundDefinition(definition *routeOutboundDefinition, definitions map[string]*routeOutboundDefinition, order *[]string) error {
	all := append([]*routeOutboundDefinition{definition}, definition.members...)
	// Validate the whole pool before publishing any member to the compiler.
	for _, item := range all {
		if existing := definitions[item.logical]; existing != nil && !equivalentRouteOutbound(existing, item) {
			return errors.New("conflicting outbound tag")
		}
	}
	for _, item := range all {
		if definitions[item.logical] == nil {
			definitions[item.logical] = item
			*order = append(*order, item.logical)
		}
	}
	return nil
}

func buildRouteBalancers(definitions map[string]*routeOutboundDefinition, order []string, mapping map[string]string) []*coreConf.BalancingRule {
	var result []*coreConf.BalancingRule
	for _, logical := range order {
		definition := definitions[logical]
		if len(definition.members) == 0 {
			continue
		}
		selectors := make(coreConf.StringList, 0, len(definition.members))
		for _, member := range definition.members {
			selectors = append(selectors, mapping[member.logical])
		}
		result = append(result, &coreConf.BalancingRule{
			Tag: mapping[logical], Selectors: selectors,
			Strategy: coreConf.StrategyConfig{Type: "roundRobin"},
		})
	}
	return result
}
