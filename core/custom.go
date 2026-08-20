package core

import (
	"encoding/json"
	"net"
	"strings"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/app/dns"
	"github.com/xtls/xray-core/app/router"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	coreConf "github.com/xtls/xray-core/infra/conf"
)

// hasPublicIPv6 checks if the machine has a public IPv6 address
func hasPublicIPv6() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		// Check if it's IPv6, not loopback, not link-local, not private/ULA
		if ip.To4() == nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsPrivate() {
			return true
		}
	}
	return false
}

func hasOutboundWithTag(list []*core.OutboundHandlerConfig, tag string) bool {
	for _, o := range list {
		if o != nil && o.Tag == tag {
			return true
		}
	}
	return false
}

func routeInboundTags(tag string) []string {
	return []string{tag, StreamUnlockProbeTag(tag)}
}

func looksLikeJSON(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")
}

func jsonObjectHasKey(value string, key string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return false
	}
	_, ok := raw[key]
	return ok
}

func dnsConfigHasValue(config *coreConf.DNSConfig) bool {
	if config == nil {
		return false
	}
	return len(config.Servers) > 0 ||
		config.Hosts != nil ||
		config.ClientIP != nil ||
		config.Tag != "" ||
		config.QueryStrategy != "" ||
		config.DisableCache ||
		config.ServeStale ||
		config.ServeExpiredTTL != 0 ||
		config.DisableFallback ||
		config.DisableFallbackIfMatch ||
		config.EnableParallelQuery ||
		config.UseSystemHosts
}

func applyRouteMatchToDNSServer(server *coreConf.NameServerConfig, match []string) {
	if server == nil || len(match) == 0 || len(server.Domains) != 0 {
		return
	}
	server.Domains = match
	server.SkipFallback = true
}

func mergeDNSConfig(dst *coreConf.DNSConfig, src *coreConf.DNSConfig, routeMatch []string) {
	if dst == nil || src == nil {
		return
	}
	if len(src.Servers) == 1 {
		applyRouteMatchToDNSServer(src.Servers[0], routeMatch)
	}
	if len(src.Servers) > 0 {
		dst.Servers = append(dst.Servers, src.Servers...)
	}
	if src.Hosts != nil {
		dst.Hosts = src.Hosts
	}
	if src.ClientIP != nil {
		dst.ClientIP = src.ClientIP
	}
	if src.Tag != "" {
		dst.Tag = src.Tag
	}
	if src.QueryStrategy != "" {
		dst.QueryStrategy = src.QueryStrategy
	}
	if src.DisableCache {
		dst.DisableCache = true
	}
	if src.ServeStale {
		dst.ServeStale = true
	}
	if src.ServeExpiredTTL != 0 {
		dst.ServeExpiredTTL = src.ServeExpiredTTL
	}
	if src.DisableFallback {
		dst.DisableFallback = true
	}
	if src.DisableFallbackIfMatch {
		dst.DisableFallbackIfMatch = true
	}
	if src.EnableParallelQuery {
		dst.EnableParallelQuery = true
	}
	if src.UseSystemHosts {
		dst.UseSystemHosts = true
	}
}

func appendDNSServerConfig(coreDnsConfig *coreConf.DNSConfig, value string, routeMatch []string) bool {
	var server coreConf.NameServerConfig
	if err := json.Unmarshal([]byte(value), &server); err != nil || server.Address == nil {
		return false
	}
	applyRouteMatchToDNSServer(&server, routeMatch)
	coreDnsConfig.Servers = append(coreDnsConfig.Servers, &server)
	return true
}

func applyDNSRouteConfig(coreDnsConfig *coreConf.DNSConfig, route panel.Route) {
	if coreDnsConfig == nil || route.ActionValue == nil {
		return
	}
	actionValue := strings.TrimSpace(*route.ActionValue)
	if actionValue == "" {
		return
	}

	if looksLikeJSON(actionValue) {
		if strings.HasPrefix(actionValue, "{") && jsonObjectHasKey(actionValue, "address") {
			appendDNSServerConfig(coreDnsConfig, actionValue, route.Match)
			return
		}

		dnsJSON := actionValue
		if strings.HasPrefix(dnsJSON, "[") {
			dnsJSON = `{"servers":` + dnsJSON + `}`
		}

		var dnsConfig coreConf.DNSConfig
		if err := json.Unmarshal([]byte(dnsJSON), &dnsConfig); err == nil && dnsConfigHasValue(&dnsConfig) {
			mergeDNSConfig(coreDnsConfig, &dnsConfig, route.Match)
			return
		}

		appendDNSServerConfig(coreDnsConfig, actionValue, route.Match)
		return
	}

	server := &coreConf.NameServerConfig{
		Address: &coreConf.Address{
			Address: xnet.ParseAddress(actionValue),
		},
	}
	applyRouteMatchToDNSServer(server, route.Match)
	coreDnsConfig.Servers = append(coreDnsConfig.Servers, server)
}

func GetCustomConfig(infos []*panel.NodeInfo) (*dns.Config, []*core.OutboundHandlerConfig, *router.Config, error) {
	compiled, err := compileRouteRuntime(infos, false)
	if err != nil {
		return nil, nil, nil, err
	}
	defaultOutbound, err := buildDefaultOutbound()
	if err != nil {
		return nil, nil, nil, err
	}
	blockOutbound, err := buildBlockOutbound()
	if err != nil {
		return nil, nil, nil, err
	}
	dnsOutbound, err := buildDnsOutbound()
	if err != nil {
		return nil, nil, nil, err
	}
	outbounds := []*core.OutboundHandlerConfig{defaultOutbound, blockOutbound, dnsOutbound}
	outbounds = append(outbounds, compiled.CustomOutbounds...)
	return compiled.DNSConfig, outbounds, compiled.RouterConfig, nil
}
