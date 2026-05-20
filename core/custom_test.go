package core

import (
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	coreConf "github.com/xtls/xray-core/infra/conf"
)

func TestApplyDNSRouteConfigKeepsSimpleDNSRoute(t *testing.T) {
	value := "8.8.8.8"
	config := &coreConf.DNSConfig{}

	applyDNSRouteConfig(config, panel.Route{
		Match:       []string{"geosite:openai"},
		ActionValue: &value,
	})

	if len(config.Servers) != 1 {
		t.Fatalf("expected 1 DNS server, got %d", len(config.Servers))
	}
	if config.Servers[0].Address == nil {
		t.Fatal("expected simple DNS route to set a server address")
	}
	if len(config.Servers[0].Domains) != 1 || config.Servers[0].Domains[0] != "geosite:openai" {
		t.Fatalf("expected route match to become server domains, got %#v", config.Servers[0].Domains)
	}
	if !config.Servers[0].SkipFallback {
		t.Fatal("expected matched DNS route to skip fallback")
	}
}

func TestApplyDNSRouteConfigMergesXrayDNSConfig(t *testing.T) {
	value := `{
		"servers": [
			"1.1.1.1",
			{
				"address": "https+local://hkg.core.access.zznet.fun/dns-query",
				"domains": ["geosite:openai"],
				"skipFallback": true,
				"finalQuery": true,
				"queryStrategy": "UseIPv4"
			}
		],
		"queryStrategy": "UseIPv4",
		"disableFallbackIfMatch": true,
		"tag": "dns_inbound"
	}`
	config := &coreConf.DNSConfig{}

	applyDNSRouteConfig(config, panel.Route{ActionValue: &value})

	if len(config.Servers) != 2 {
		t.Fatalf("expected 2 DNS servers, got %d", len(config.Servers))
	}
	if config.QueryStrategy != "UseIPv4" {
		t.Fatalf("expected query strategy UseIPv4, got %q", config.QueryStrategy)
	}
	if !config.DisableFallbackIfMatch {
		t.Fatal("expected disableFallbackIfMatch to be enabled")
	}
	if config.Tag != "dns_inbound" {
		t.Fatalf("expected tag dns_inbound, got %q", config.Tag)
	}
	unlockServer := config.Servers[1]
	if unlockServer.Address == nil {
		t.Fatal("expected unlock server address")
	}
	if len(unlockServer.Domains) != 1 || unlockServer.Domains[0] != "geosite:openai" {
		t.Fatalf("expected unlock server domains, got %#v", unlockServer.Domains)
	}
	if !unlockServer.SkipFallback || !unlockServer.FinalQuery {
		t.Fatalf("expected skipFallback and finalQuery to be true, got skipFallback=%v finalQuery=%v", unlockServer.SkipFallback, unlockServer.FinalQuery)
	}
}

func TestApplyDNSRouteConfigAppliesMatchToSingleJSONServer(t *testing.T) {
	value := `{"address":"https://dns.google/dns-query","queryStrategy":"UseIPv4"}`
	config := &coreConf.DNSConfig{}

	applyDNSRouteConfig(config, panel.Route{
		Match:       []string{"domain:hulu.jp"},
		ActionValue: &value,
	})

	if len(config.Servers) != 1 {
		t.Fatalf("expected 1 DNS server, got %d", len(config.Servers))
	}
	if len(config.Servers[0].Domains) != 1 || config.Servers[0].Domains[0] != "domain:hulu.jp" {
		t.Fatalf("expected route match to be applied to the JSON server, got %#v", config.Servers[0].Domains)
	}
	if !config.Servers[0].SkipFallback {
		t.Fatal("expected matched JSON DNS server to skip fallback")
	}
	if config.Servers[0].QueryStrategy != "UseIPv4" {
		t.Fatalf("expected server query strategy UseIPv4, got %q", config.Servers[0].QueryStrategy)
	}
}
