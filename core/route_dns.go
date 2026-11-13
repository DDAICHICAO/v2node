package core

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/xtls/xray-core/common"
	xnet "github.com/xtls/xray-core/common/net"
	xraycore "github.com/xtls/xray-core/core"
	featuredns "github.com/xtls/xray-core/features/dns"
)

// routeNodeDNS owns independent resolver/cache instances for one generation.
// Probe aliases share only their owning node's resolver.
type routeNodeDNS struct {
	fallback  featuredns.Client
	legacy    featuredns.Client
	byInbound map[string]featuredns.Client
	backends  []featuredns.Client
	closeOnce sync.Once
	closeErr  error
}

func createRouteNodeDNS(server *xraycore.Instance, compiled *CompiledRouteRuntime) (featuredns.Client, error) {
	group := &routeNodeDNS{byInbound: make(map[string]featuredns.Client)}
	fallback, err := createRouteDNSBackend(server, compiled.DNSConfig)
	if err != nil {
		return nil, err
	}
	group.fallback = fallback
	group.legacy = fallback
	group.backends = append(group.backends, fallback)
	tags := make([]string, 0, len(compiled.NodeDNSConfigs))
	for tag := range compiled.NodeDNSConfigs {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		backend, err := createRouteDNSBackend(server, compiled.NodeDNSConfigs[tag])
		if err != nil {
			_ = group.Close()
			return nil, err
		}
		group.backends = append(group.backends, backend)
		for _, alias := range routeInboundTags(tag) {
			if _, exists := group.byInbound[alias]; exists {
				_ = group.Close()
				return nil, errors.New("route DNS inbound aliases overlap")
			}
			group.byInbound[alias] = backend
		}
	}
	// Preserve the legacy context-free API for a single logical node. Multiple
	// nodes without an inbound context use only the default system configuration.
	if len(tags) == 1 {
		group.legacy = group.byInbound[tags[0]]
	}
	return group, nil
}

func (*routeNodeDNS) Type() interface{} { return featuredns.ClientType() }
func (g *routeNodeDNS) Start() error {
	for _, backend := range g.backends {
		if err := backend.Start(); err != nil {
			_ = g.Close()
			return err
		}
	}
	return nil
}
func (g *routeNodeDNS) Close() error {
	g.closeOnce.Do(func() {
		var errs []error
		for _, backend := range g.backends {
			errs = append(errs, common.Close(backend))
		}
		g.closeErr = errors.Join(errs...)
	})
	return g.closeErr
}
func (g *routeNodeDNS) ForInbound(tag string) featuredns.Client {
	if backend := g.byInbound[tag]; backend != nil {
		return backend
	}
	return g.fallback
}
func (g *routeNodeDNS) LookupIP(domain string, option featuredns.IPOption) ([]xnet.IP, uint32, error) {
	return g.legacy.LookupIP(domain, option)
}
func (g *routeNodeDNS) IsOwnLink(ctx context.Context) bool {
	for _, backend := range g.backends {
		if verifier, ok := backend.(interface{ IsOwnLink(context.Context) bool }); ok && verifier.IsOwnLink(ctx) {
			return true
		}
	}
	return false
}
