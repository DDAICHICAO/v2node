package core

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
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

func GetCustomConfig(infos []*panel.NodeInfo) (*dns.Config, []*core.OutboundHandlerConfig, *router.Config, error) {
	//dns
	queryStrategy := "UseIPv4v6"
	if !hasPublicIPv6() {
		queryStrategy = "UseIPv4"
	}
	coreDnsConfig := &coreConf.DNSConfig{
		Servers: []*coreConf.NameServerConfig{
			{
				Address: &coreConf.Address{
					Address: xnet.ParseAddress("localhost"),
				},
			},
		},
		QueryStrategy: queryStrategy,
	}
	//outbound
	defaultoutbound, _ := buildDefaultOutbound()
	coreOutboundConfig := append([]*core.OutboundHandlerConfig{}, defaultoutbound)
	block, _ := buildBlockOutbound()
	coreOutboundConfig = append(coreOutboundConfig, block)
	dns, _ := buildDnsOutbound()
	coreOutboundConfig = append(coreOutboundConfig, dns)

	//route
	domainStrategy := "AsIs"
	dnsRule, _ := json.Marshal(map[string]interface{}{
		"port":        "53",
		"network":     "udp",
		"outboundTag": "dns_out",
	})
	coreRouterConfig := &coreConf.RouterConfig{
		RuleList:       []json.RawMessage{dnsRule},
		DomainStrategy: &domainStrategy,
	}

	for _, info := range infos {
		if len(info.Common.Routes) == 0 {
			continue
		}
		for _, route := range info.Common.Routes {
			domainMatch := normalizeRouteDomainMatches(route.Match)
			switch route.Action {
			case "dns":
				if route.ActionValue == nil {
					continue
				}
				server := &coreConf.NameServerConfig{
					Address: &coreConf.Address{
						Address: xnet.ParseAddress(*route.ActionValue),
					},
				}
				if len(domainMatch) != 0 {
					server.Domains = domainMatch
					server.SkipFallback = true
				}
				coreDnsConfig.Servers = append(coreDnsConfig.Servers, server)
			case "block":
				if len(domainMatch) == 0 {
					continue
				}
				rule := map[string]interface{}{
					"ruleTag":     routeRuleTag(route),
					"inboundTag":  routeInboundTags(info.Tag),
					"domain":      domainMatch,
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "block_ip":
				rule := map[string]interface{}{
					"ruleTag":     routeRuleTag(route),
					"inboundTag":  routeInboundTags(info.Tag),
					"ip":          route.Match,
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "block_port":
				rule := map[string]interface{}{
					"ruleTag":     routeRuleTag(route),
					"inboundTag":  routeInboundTags(info.Tag),
					"port":        strings.Join(route.Match, ","),
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "protocol":
				rule := map[string]interface{}{
					"ruleTag":     routeRuleTag(route),
					"inboundTag":  routeInboundTags(info.Tag),
					"protocol":    route.Match,
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "route":
				if route.ActionValue == nil {
					continue
				}
				if len(domainMatch) == 0 {
					continue
				}
				outbound := &coreConf.OutboundDetourConfig{}
				err := json.Unmarshal([]byte(*route.ActionValue), outbound)
				if err != nil {
					continue
				}
				rule := map[string]interface{}{
					"ruleTag":     routeRuleTag(route),
					"inboundTag":  routeInboundTags(info.Tag),
					"domain":      domainMatch,
					"outboundTag": outbound.Tag,
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
				if hasOutboundWithTag(coreOutboundConfig, outbound.Tag) {
					continue
				}
				custom_outbound, err := outbound.Build()
				if err != nil {
					continue
				}
				coreOutboundConfig = append(coreOutboundConfig, custom_outbound)
			case "route_ip":
				if route.ActionValue == nil {
					continue
				}
				outbound := &coreConf.OutboundDetourConfig{}
				err := json.Unmarshal([]byte(*route.ActionValue), outbound)
				if err != nil {
					continue
				}
				rule := map[string]interface{}{
					"ruleTag":     routeRuleTag(route),
					"inboundTag":  routeInboundTags(info.Tag),
					"ip":          route.Match,
					"outboundTag": outbound.Tag,
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
				if hasOutboundWithTag(coreOutboundConfig, outbound.Tag) {
					continue
				}
				custom_outbound, err := outbound.Build()
				if err != nil {
					continue
				}
				coreOutboundConfig = append(coreOutboundConfig, custom_outbound)
			case "default_out":
				if route.ActionValue == nil {
					continue
				}
				outbound := &coreConf.OutboundDetourConfig{}
				err := json.Unmarshal([]byte(*route.ActionValue), outbound)
				if err != nil {
					continue
				}
				rule := map[string]interface{}{
					"ruleTag":     routeRuleTag(route),
					"inboundTag":  routeInboundTags(info.Tag),
					"network":     "tcp,udp",
					"outboundTag": outbound.Tag,
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
				if hasOutboundWithTag(coreOutboundConfig, outbound.Tag) {
					continue
				}
				custom_outbound, err := outbound.Build()
				if err != nil {
					continue
				}
				coreOutboundConfig = append(coreOutboundConfig, custom_outbound)
			default:
				continue
			}
		}
	}
	DnsConfig, err := coreDnsConfig.Build()
	if err != nil {
		return nil, nil, nil, err
	}
	RouterConfig, err := coreRouterConfig.Build()
	if err != nil {
		return nil, nil, nil, err
	}
	return DnsConfig, coreOutboundConfig, RouterConfig, nil
}

func routeRuleTag(route panel.Route) string {
	if route.Id > 0 {
		return fmt.Sprintf("%s:%d", route.Action, route.Id)
	}
	return route.Action
}

func normalizeRouteDomainMatches(matches []string) []string {
	normalized := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		item := normalizeRouteDomainMatch(match)
		if item == "" {
			continue
		}
		key := strings.ToLower(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, item)
	}
	return normalized
}

func normalizeRouteDomainMatch(value string) string {
	item := strings.TrimSpace(value)
	if item == "" {
		return ""
	}
	lower := strings.ToLower(item)
	for _, prefix := range []string{"geosite:", "regexp:", "domain:", "full:", "keyword:"} {
		if strings.HasPrefix(lower, prefix) {
			return item
		}
	}
	if strings.HasPrefix(lower, "regex:") {
		return "regexp:" + strings.TrimSpace(item[len("regex:"):])
	}

	if strings.HasPrefix(item, "//") {
		if u, err := url.Parse("https:" + item); err == nil && u.Hostname() != "" {
			item = u.Hostname()
		}
	} else if strings.Contains(item, "://") {
		if u, err := url.Parse(item); err == nil && u.Hostname() != "" {
			item = u.Hostname()
		}
	} else if cut := strings.IndexAny(item, "/?#"); cut >= 0 {
		item = item[:cut]
	}

	item = stripRouteHostPort(item)
	item = strings.TrimSuffix(strings.TrimSpace(item), ".")
	if item == "" {
		return ""
	}
	if strings.HasPrefix(item, "*.") {
		item = "domain:" + strings.TrimPrefix(item, "*.")
	} else if strings.HasPrefix(item, ".") {
		item = "domain:" + strings.TrimPrefix(item, ".")
	}
	return strings.ToLower(item)
}

func stripRouteHostPort(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(h, "[]")
	}
	if strings.Count(host, ":") == 1 {
		parts := strings.SplitN(host, ":", 2)
		if _, err := strconv.Atoi(parts[1]); err == nil {
			return parts[0]
		}
	}
	return strings.Trim(host, "[]")
}
