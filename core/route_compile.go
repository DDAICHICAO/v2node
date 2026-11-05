package core

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/app/dns"
	"github.com/xtls/xray-core/app/router"
	xnet "github.com/xtls/xray-core/common/net"
	xraycore "github.com/xtls/xray-core/core"
	coreConf "github.com/xtls/xray-core/infra/conf"
	"google.golang.org/protobuf/proto"
)

type CompiledRouteRuntime struct {
	DNSConfig         *dns.Config
	RouterConfig      *router.Config
	CustomOutbounds   []*xraycore.OutboundHandlerConfig
	LogicalToPhysical map[string]string
	ConfigHash        string
}

type routeOutboundDefinition struct {
	logical      string
	raw          string
	logicalProto *xraycore.OutboundHandlerConfig
	info         *panel.NodeInfo
	index        int
	route        panel.Route
	members      []*routeOutboundDefinition
}

type routeRuleCandidate struct {
	info     *panel.NodeInfo
	index    int
	route    panel.Route
	logical  string
	raw      json.RawMessage
	balancer bool
}

type outboundResolutionError struct {
	logical string
	err     error
}

func (e *outboundResolutionError) Error() string { return e.err.Error() }
func (e *outboundResolutionError) Unwrap() error { return e.err }

var reservedOutboundTags = map[string]struct{}{
	"Default": {},
	"block":   {},
	"dns_out": {},
}

func CompileRouteRuntime(infos []*panel.NodeInfo) (*CompiledRouteRuntime, error) {
	return compileRouteRuntime(infos, true)
}

func compileRouteRuntime(infos []*panel.NodeInfo, strict bool) (*CompiledRouteRuntime, error) {
	coreDNSConfig := defaultRouteDNSConfig()
	definitions := make(map[string]*routeOutboundDefinition)
	definitionOrder := make([]string, 0)
	routeCandidates := make([]routeRuleCandidate, 0)

	for _, info := range infos {
		if info == nil || info.Common == nil {
			if strict {
				return nil, errors.New("compile route: node info is incomplete")
			}
			continue
		}
		for index, route := range info.Common.Routes {
			switch route.Action {
			case "dns":
				if err := applyDNSRouteConfigChecked(coreDNSConfig, route); err != nil {
					if handled := handleRouteCompileFailure(strict, info, index, route, err); handled != nil {
						return nil, handled
					}
				}
			case "block", "block_ip", "block_port", "protocol":
				raw, err := buildStaticRouteRule(info, route)
				if err != nil {
					if handled := handleRouteCompileFailure(strict, info, index, route, err); handled != nil {
						return nil, handled
					}
					continue
				}
				routeCandidates = append(routeCandidates, routeRuleCandidate{info: info, index: index, route: route, raw: raw})
			case "route", "route_ip", "default_out":
				definition, err := parseRouteOutboundDefinition(info, index, route)
				if err != nil {
					if handled := handleRouteCompileFailure(strict, info, index, route, err); handled != nil {
						return nil, handled
					}
					continue
				}
				if err := registerRouteOutboundDefinition(definition, definitions, &definitionOrder); err != nil {
					if handled := handleRouteCompileFailure(strict, info, index, route, err); handled != nil {
						return nil, handled
					}
					continue
				}
				routeCandidates = append(routeCandidates, routeRuleCandidate{info: info, index: index, route: route, logical: definition.logical, balancer: len(definition.members) > 0})
			default:
				if handled := handleRouteCompileFailure(strict, info, index, route, errors.New("unknown route action")); handled != nil {
					return nil, handled
				}
			}
		}
	}

	var logicalToPhysical map[string]string
	for {
		var err error
		logicalToPhysical, err = resolvePhysicalOutboundTags(definitions, definitionOrder)
		if err == nil {
			break
		}
		if strict {
			return nil, err
		}
		var resolutionError *outboundResolutionError
		if !errors.As(err, &resolutionError) {
			return nil, err
		}
		definition := definitions[resolutionError.logical]
		if definition == nil {
			return nil, err
		}
		log.WithFields(log.Fields{
			"node_id":    definition.info.Id,
			"rule_index": definition.index,
			"action":     definition.route.Action,
		}).WithError(resolutionError.err).Warn("Skipping invalid route outbound during compatible startup")
		delete(definitions, resolutionError.logical)
		definitionOrder = removeLogicalTag(definitionOrder, resolutionError.logical)
		for _, member := range definition.members {
			delete(definitions, member.logical)
			definitionOrder = removeLogicalTag(definitionOrder, member.logical)
		}
		routeCandidates = removeOutboundRouteCandidates(routeCandidates, resolutionError.logical)
	}
	customOutbounds := make([]*xraycore.OutboundHandlerConfig, 0, len(definitionOrder))
	for _, logical := range definitionOrder {
		definition := definitions[logical]
		if len(definition.members) > 0 {
			continue
		}
		outbound, err := buildPhysicalOutbound(definition, logicalToPhysical)
		if err != nil {
			return nil, routeCompileError(definition.info, definition.index, definition.route, err)
		}
		customOutbounds = append(customOutbounds, outbound)
	}

	dnsConfig, err := coreDNSConfig.Build()
	if err != nil {
		return nil, errors.New("compile route: invalid DNS config")
	}
	balancers := buildRouteBalancers(definitions, definitionOrder, logicalToPhysical)
	routerConfig, err := buildCompiledRouterConfig(routeCandidates, logicalToPhysical, balancers, strict)
	if err != nil {
		return nil, err
	}
	configHash, err := compiledRouteRuntimeHash(dnsConfig, routerConfig, customOutbounds)
	if err != nil {
		return nil, err
	}
	return &CompiledRouteRuntime{
		DNSConfig:         dnsConfig,
		RouterConfig:      routerConfig,
		CustomOutbounds:   customOutbounds,
		LogicalToPhysical: logicalToPhysical,
		ConfigHash:        configHash,
	}, nil
}

func defaultRouteDNSConfig() *coreConf.DNSConfig {
	queryStrategy := "UseIPv4v6"
	if !hasPublicIPv6() {
		queryStrategy = "UseIPv4"
	}
	return &coreConf.DNSConfig{
		Servers: []*coreConf.NameServerConfig{{
			Address: &coreConf.Address{Address: xnet.ParseAddress("localhost")},
		}},
		QueryStrategy: queryStrategy,
	}
}

func defaultRouteRouterConfig() *coreConf.RouterConfig {
	domainStrategy := "AsIs"
	dnsRule, _ := json.Marshal(map[string]interface{}{
		"port":        "53",
		"network":     "udp",
		"outboundTag": "dns_out",
	})
	return &coreConf.RouterConfig{
		RuleList:       []json.RawMessage{dnsRule},
		DomainStrategy: &domainStrategy,
	}
}

func applyDNSRouteConfigChecked(config *coreConf.DNSConfig, route panel.Route) error {
	if config == nil || route.ActionValue == nil || strings.TrimSpace(*route.ActionValue) == "" {
		return errors.New("DNS action value is empty")
	}
	actionValue := strings.TrimSpace(*route.ActionValue)
	if !looksLikeJSON(actionValue) {
		applyDNSRouteConfig(config, route)
		return nil
	}
	if !json.Valid([]byte(actionValue)) {
		return errors.New("invalid DNS action JSON")
	}
	if strings.HasPrefix(actionValue, "{") && jsonObjectHasKey(actionValue, "address") {
		if !appendDNSServerConfig(config, actionValue, route.Match) {
			return errors.New("invalid DNS server config")
		}
		return nil
	}

	dnsJSON := actionValue
	if strings.HasPrefix(dnsJSON, "[") {
		dnsJSON = `{"servers":` + dnsJSON + `}`
	}
	var dnsConfig coreConf.DNSConfig
	if err := json.Unmarshal([]byte(dnsJSON), &dnsConfig); err == nil && dnsConfigHasValue(&dnsConfig) {
		mergeDNSConfig(config, &dnsConfig, route.Match)
		return nil
	}
	if appendDNSServerConfig(config, actionValue, route.Match) {
		return nil
	}
	return errors.New("invalid DNS action config")
}

func parseRouteOutboundDefinition(info *panel.NodeInfo, index int, route panel.Route) (*routeOutboundDefinition, error) {
	if route.ActionValue == nil || strings.TrimSpace(*route.ActionValue) == "" {
		return nil, errors.New("outbound action value is empty")
	}
	raw := strings.TrimSpace(*route.ActionValue)
	if group, err := parseShadowsocksRouteGroup(info, index, route, raw); group != nil || err != nil {
		return group, err
	}
	return parseSingleRouteOutboundDefinition(info, index, route, raw)
}

func parseSingleRouteOutboundDefinition(info *panel.NodeInfo, index int, route panel.Route, raw string) (*routeOutboundDefinition, error) {
	outbound, err := decodeOutboundConfig(raw)
	if err != nil {
		return nil, err
	}
	outbound.Tag = strings.TrimSpace(outbound.Tag)
	if outbound.Tag == "" {
		return nil, errors.New("outbound tag is empty")
	}
	if _, reserved := reservedOutboundTags[outbound.Tag]; reserved {
		return nil, errors.New("reserved outbound tag")
	}
	built, err := outbound.Build()
	if err != nil {
		return nil, errors.New("invalid outbound config")
	}
	return &routeOutboundDefinition{
		logical:      outbound.Tag,
		raw:          raw,
		logicalProto: built,
		info:         info,
		index:        index,
		route:        route,
	}, nil
}

func resolvePhysicalOutboundTags(definitions map[string]*routeOutboundDefinition, order []string) (map[string]string, error) {
	resolved := make(map[string]string, len(definitions))
	visiting := make(map[string]bool, len(definitions))
	var resolve func(string) (string, error)
	resolve = func(logical string) (string, error) {
		if physical := resolved[logical]; physical != "" {
			return physical, nil
		}
		definition := definitions[logical]
		if definition == nil {
			return "", errors.New("missing outbound dependency")
		}
		if visiting[logical] {
			return "", newOutboundResolutionError(definition, errors.New("outbound dependency cycle"))
		}
		visiting[logical] = true
		defer delete(visiting, logical)
		if len(definition.members) > 0 {
			members := make([]string, 0, len(definition.members))
			for _, member := range definition.members {
				physical, err := resolve(member.logical)
				if err != nil {
					return "", newOutboundResolutionError(definition, err)
				}
				members = append(members, physical)
			}
			sum := sha256.Sum256([]byte(logical + "\x00" + strings.Join(members, "\x00")))
			physical := "sntp-balancer@" + hex.EncodeToString(sum[:])
			resolved[logical] = physical
			return physical, nil
		}

		outbound, err := decodeOutboundConfig(definition.raw)
		if err != nil {
			return "", newOutboundResolutionError(definition, err)
		}
		for _, ref := range outboundReferenceFields(outbound) {
			dependency := strings.TrimSpace(*ref)
			if dependency == "" {
				continue
			}
			if _, builtin := reservedOutboundTags[dependency]; builtin {
				continue
			}
			if definitions[dependency] == nil {
				return "", newOutboundResolutionError(definition, errors.New("missing outbound dependency"))
			}
			if len(definitions[dependency].members) > 0 {
				return "", newOutboundResolutionError(definition, errors.New("outbound dependency cannot reference a load-balancing group"))
			}
			physical, err := resolve(dependency)
			if err != nil {
				return "", err
			}
			*ref = physical
		}
		outbound.Tag = logical
		built, err := outbound.Build()
		if err != nil {
			return "", newOutboundResolutionError(definition, errors.New("invalid outbound config"))
		}
		physical, err := physicalOutboundTag(logical, built)
		if err != nil {
			return "", newOutboundResolutionError(definition, errors.New("cannot version outbound config"))
		}
		resolved[logical] = physical
		return physical, nil
	}

	for _, logical := range order {
		if _, err := resolve(logical); err != nil {
			return nil, err
		}
	}
	return resolved, nil
}

func newOutboundResolutionError(definition *routeOutboundDefinition, cause error) error {
	return &outboundResolutionError{
		logical: definition.logical,
		err:     routeCompileError(definition.info, definition.index, definition.route, cause),
	}
}

func removeLogicalTag(tags []string, remove string) []string {
	filtered := tags[:0]
	for _, tag := range tags {
		if tag != remove {
			filtered = append(filtered, tag)
		}
	}
	return filtered
}

func removeOutboundRouteCandidates(candidates []routeRuleCandidate, logical string) []routeRuleCandidate {
	filtered := candidates[:0]
	for _, candidate := range candidates {
		if candidate.logical != logical {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func buildPhysicalOutbound(definition *routeOutboundDefinition, mapping map[string]string) (*xraycore.OutboundHandlerConfig, error) {
	outbound, err := decodeOutboundConfig(definition.raw)
	if err != nil {
		return nil, err
	}
	for _, ref := range outboundReferenceFields(outbound) {
		logical := strings.TrimSpace(*ref)
		if logical == "" {
			continue
		}
		if physical := mapping[logical]; physical != "" {
			*ref = physical
			continue
		}
		if _, builtin := reservedOutboundTags[logical]; !builtin {
			return nil, errors.New("missing outbound dependency")
		}
	}
	outbound.Tag = mapping[definition.logical]
	built, err := outbound.Build()
	if err != nil {
		return nil, errors.New("invalid outbound config")
	}
	return built, nil
}

func decodeOutboundConfig(raw string) (*coreConf.OutboundDetourConfig, error) {
	var outbound coreConf.OutboundDetourConfig
	if err := json.Unmarshal([]byte(raw), &outbound); err != nil {
		return nil, errors.New("invalid outbound JSON")
	}
	if outbound.ProxySettings != nil {
		var legacy struct {
			Tag                 string `json:"tag"`
			TransportLayerProxy bool   `json:"transportLayer"`
		}
		if err := json.Unmarshal(*outbound.ProxySettings, &legacy); err != nil || strings.TrimSpace(legacy.Tag) == "" {
			return nil, errors.New("invalid legacy outbound proxy settings")
		}
		if stream := outbound.StreamSetting; stream != nil && !legacy.TransportLayerProxy {
			network := "tcp"
			if stream.Network != nil {
				network = strings.ToLower(string(*stream.Network))
			}
			if stream.Method != nil {
				network = strings.ToLower(string(*stream.Method))
			}
			security := strings.ToLower(stream.Security)
			// Legacy application-layer chaining ignored these transport options.
			// Activating them during migration would silently change the wire.
			if (network != "tcp" && network != "raw") || (security != "" && security != "none" && security != "tls") ||
				stream.TCPSettings != nil || stream.RAWSettings != nil || stream.FinalMask != nil ||
				stream.Address != nil || stream.Port != 0 || stream.SocketSettings != nil {
				return nil, errors.New("legacy outbound proxy transport settings require explicit dialerProxy migration")
			}
		}
		if outbound.StreamSetting == nil {
			outbound.StreamSetting = &coreConf.StreamConfig{}
		}
		if outbound.StreamSetting.SocketSettings == nil {
			outbound.StreamSetting.SocketSettings = &coreConf.SocketConfig{}
		}
		if outbound.StreamSetting.SocketSettings.DialerProxy != "" {
			return nil, errors.New("conflicting outbound proxy settings")
		}
		// Xray removed proxySettings; normalize before resolving dependencies so
		// legacy panel routes retain the same validation and physical tag rewrite.
		outbound.StreamSetting.SocketSettings.DialerProxy = strings.TrimSpace(legacy.Tag)
		outbound.ProxySettings = nil
	}
	return &outbound, nil
}

func outboundReferenceFields(outbound *coreConf.OutboundDetourConfig) []*string {
	if outbound == nil {
		return nil
	}
	refs := make([]*string, 0, 2)
	if outbound.StreamSetting != nil && outbound.StreamSetting.SocketSettings != nil {
		refs = append(refs, &outbound.StreamSetting.SocketSettings.DialerProxy)
	}
	return refs
}

func physicalOutboundTag(logical string, config *xraycore.OutboundHandlerConfig) (string, error) {
	clone := proto.Clone(config).(*xraycore.OutboundHandlerConfig)
	clone.Tag = logical
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%s@%s", logical, hex.EncodeToString(sum[:6])), nil
}

func buildStaticRouteRule(info *panel.NodeInfo, route panel.Route) (json.RawMessage, error) {
	rule := map[string]interface{}{
		"inboundTag":  routeInboundTags(info.Tag),
		"outboundTag": "block",
	}
	switch route.Action {
	case "block":
		rule["domain"] = route.Match
	case "block_ip":
		rule["ip"] = route.Match
	case "block_port":
		rule["port"] = strings.Join(route.Match, ",")
	case "protocol":
		rule["protocol"] = route.Match
	default:
		return nil, errors.New("unknown route action")
	}
	return json.Marshal(rule)
}

func buildOutboundRouteRule(info *panel.NodeInfo, route panel.Route, physicalTag string, balancer bool) (json.RawMessage, error) {
	if physicalTag == "" {
		return nil, errors.New("outbound physical tag is empty")
	}
	rule := map[string]interface{}{
		"inboundTag": routeInboundTags(info.Tag),
	}
	if balancer {
		rule["balancerTag"] = physicalTag
	} else {
		rule["outboundTag"] = physicalTag
	}
	switch route.Action {
	case "route":
		rule["domain"] = route.Match
	case "route_ip":
		rule["ip"] = route.Match
	case "default_out":
		rule["network"] = "tcp,udp"
	default:
		return nil, errors.New("unknown route action")
	}
	return json.Marshal(rule)
}

func buildCompiledRouterConfig(candidates []routeRuleCandidate, mapping map[string]string, balancers []*coreConf.BalancingRule, strict bool) (*router.Config, error) {
	newConfig := func() *coreConf.RouterConfig {
		config := defaultRouteRouterConfig()
		config.Balancers = balancers
		return config
	}
	materialized := make([]routeRuleCandidate, 0, len(candidates))
	config := newConfig()
	for _, candidate := range candidates {
		raw, err := materializeRouteRule(candidate, mapping)
		if err != nil {
			if handled := handleRouteCompileFailure(strict, candidate.info, candidate.index, candidate.route, err); handled != nil {
				return nil, handled
			}
			continue
		}
		candidate.raw = raw
		materialized = append(materialized, candidate)
		config.RuleList = append(config.RuleList, raw)
	}
	routerConfig, err := config.Build()
	if err == nil {
		return routerConfig, nil
	}

	if strict {
		for _, candidate := range materialized {
			probe := newConfig()
			probe.RuleList = append(probe.RuleList, candidate.raw)
			if _, probeErr := probe.Build(); probeErr != nil {
				return nil, routeCompileError(candidate.info, candidate.index, candidate.route, errors.New("invalid route match"))
			}
		}
		return nil, errors.New("compile route: invalid router config")
	}

	compatible := newConfig()
	for _, candidate := range materialized {
		probe := newConfig()
		probe.RuleList = append(probe.RuleList, candidate.raw)
		if _, probeErr := probe.Build(); probeErr != nil {
			_ = handleRouteCompileFailure(false, candidate.info, candidate.index, candidate.route, errors.New("invalid route match"))
			continue
		}
		compatible.RuleList = append(compatible.RuleList, candidate.raw)
	}
	routerConfig, err = compatible.Build()
	if err != nil {
		return nil, errors.New("compile route: invalid compatible router config")
	}
	return routerConfig, nil
}

func materializeRouteRule(candidate routeRuleCandidate, mapping map[string]string) (json.RawMessage, error) {
	if candidate.logical == "" {
		return candidate.raw, nil
	}
	return buildOutboundRouteRule(candidate.info, candidate.route, mapping[candidate.logical], candidate.balancer)
}

func routeCompileError(info *panel.NodeInfo, index int, route panel.Route, err error) error {
	nodeID := 0
	if info != nil {
		nodeID = info.Id
	}
	return fmt.Errorf("compile route: node_id=%d rule_index=%d action=%s: %w", nodeID, index, route.Action, err)
}

func handleRouteCompileFailure(strict bool, info *panel.NodeInfo, index int, route panel.Route, err error) error {
	wrapped := routeCompileError(info, index, route, err)
	if strict {
		return wrapped
	}
	log.WithFields(log.Fields{
		"node_id":    info.Id,
		"rule_index": index,
		"action":     route.Action,
	}).WithError(err).Warn("Skipping invalid route config during compatible startup")
	return nil
}

func compiledRouteRuntimeHash(dnsConfig *dns.Config, routerConfig *router.Config, outbounds []*xraycore.OutboundHandlerConfig) (string, error) {
	hasher := sha256.New()
	if err := writeDeterministicProto(hasher, dnsConfig); err != nil {
		return "", err
	}
	if err := writeDeterministicProto(hasher, routerConfig); err != nil {
		return "", err
	}
	sorted := append([]*xraycore.OutboundHandlerConfig(nil), outbounds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Tag < sorted[j].Tag })
	for _, outbound := range sorted {
		if err := writeDeterministicProto(hasher, outbound); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func writeDeterministicProto(hasher hash.Hash, message proto.Message) error {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return err
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(raw)))
	_, _ = hasher.Write(length[:])
	_, _ = hasher.Write(raw)
	return nil
}
