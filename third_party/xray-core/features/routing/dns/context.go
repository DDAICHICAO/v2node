package dns

import (
	"context"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/routing"
)

// ResolvableContext is an implementation of routing.Context, with domain resolving capability.
type ResolvableContext struct {
	routing.Context
	dnsClient dns.Client
	cacheIPs  []net.IP
	hasError  bool
}

// GetTargetIPs overrides original routing.Context's implementation.
func (ctx *ResolvableContext) GetTargetIPs() []net.IP {
	if len(ctx.cacheIPs) > 0 {
		return ctx.cacheIPs
	}

	if ctx.hasError {
		return nil
	}

	if domain := ctx.GetTargetDomain(); len(domain) != 0 {
		queryContext := context.Background()
		if source, ok := ctx.Context.(interface{ GetContext() context.Context }); ok && source.GetContext() != nil {
			queryContext = source.GetContext()
		}
		ips, _, err := dns.LookupIP(queryContext, ctx.dnsClient, domain, dns.IPOption{
			IPv4Enable: true,
			IPv6Enable: true,
			FakeEnable: false,
		})
		if err == nil {
			ctx.cacheIPs = ips
			return ips
		}
		errors.LogInfoInner(context.Background(), err, "resolve ip for ", domain)
	}

	if ips := ctx.Context.GetTargetIPs(); len(ips) != 0 {
		ctx.cacheIPs = ips
		return ips
	}

	ctx.hasError = true
	return nil
}

// ContextWithDNSClient creates a new routing context with domain resolving capability.
// Resolved domain IPs can be retrieved by GetTargetIPs().
func ContextWithDNSClient(ctx routing.Context, client dns.Client) routing.Context {
	return &ResolvableContext{Context: ctx, dnsClient: client}
}

func (ctx *ResolvableContext) GetContext() context.Context {
	if source, ok := ctx.Context.(interface{ GetContext() context.Context }); ok {
		return source.GetContext()
	}
	return context.Background()
}
