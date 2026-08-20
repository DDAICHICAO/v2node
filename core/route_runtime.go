package core

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/core/app/dispatcher"
	"github.com/xtls/xray-core/app/dns"
	"github.com/xtls/xray-core/common"
	xraycore "github.com/xtls/xray-core/core"
	featuredns "github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
)

func createRouteDNSBackend(server *xraycore.Instance, config *dns.Config) (featuredns.Client, error) {
	if server == nil || config == nil {
		return nil, errors.New("route DNS backend config is incomplete")
	}
	created, err := xraycore.CreateObject(server, config)
	if err != nil {
		return nil, errors.New("create route DNS backend failed")
	}
	backend, ok := created.(featuredns.Client)
	if !ok || backend == nil {
		if created != nil {
			_ = common.Close(created)
		}
		return nil, errors.New("route DNS backend has an unexpected type")
	}
	return backend, nil
}

func createRouteRouter(server *xraycore.Instance, config interface{}) (routing.Router, error) {
	created, err := xraycore.CreateObject(server, config)
	if err != nil {
		return nil, errors.New("create route router failed")
	}
	router, ok := created.(routing.Router)
	if !ok || router == nil {
		if created != nil {
			_ = common.Close(created)
		}
		return nil, errors.New("route router has an unexpected type")
	}
	return router, nil
}

func listOutboundHandlers(manager outbound.Manager) map[string]outbound.Handler {
	handlers := make(map[string]outbound.Handler)
	if manager == nil {
		return handlers
	}
	for _, handler := range manager.ListHandlers(context.Background()) {
		if handler != nil && handler.Tag() != "" {
			handlers[handler.Tag()] = handler
		}
	}
	return handlers
}

func buildGenerationHandlers(compiled *CompiledRouteRuntime, manager outbound.Manager, custom map[string]outbound.Handler) (map[string]outbound.Handler, outbound.Handler, error) {
	if compiled == nil || manager == nil {
		return nil, nil, errors.New("route generation inputs are incomplete")
	}
	handlers := make(map[string]outbound.Handler, len(compiled.CustomOutbounds)+len(compiled.LogicalToPhysical)+3)
	for _, tag := range []string{"Default", "block", "dns_out"} {
		handler := manager.GetHandler(tag)
		if handler == nil {
			return nil, nil, fmt.Errorf("built-in outbound %q is unavailable", tag)
		}
		handlers[tag] = handler
	}
	for _, config := range compiled.CustomOutbounds {
		if config == nil || custom[config.Tag] == nil {
			return nil, nil, fmt.Errorf("candidate outbound %q is unavailable", config.GetTag())
		}
		handlers[config.Tag] = custom[config.Tag]
	}
	for logical, physical := range compiled.LogicalToPhysical {
		handler := handlers[physical]
		if handler == nil {
			return nil, nil, fmt.Errorf("logical outbound %q has no candidate handler", logical)
		}
		handlers[logical] = handler
	}
	for _, rule := range compiled.RouterConfig.GetRule() {
		if tag := rule.GetTag(); tag != "" && handlers[tag] == nil {
			return nil, nil, fmt.Errorf("router references unavailable outbound %q", tag)
		}
	}
	defaultHandler := handlers["Default"]
	if managerDefault := manager.GetDefaultHandler(); managerDefault != nil && managerDefault.Tag() == "Default" {
		defaultHandler = managerDefault
	}
	return handlers, defaultHandler, nil
}

func routeGenerationCleanup(router routing.Router, dnsBackend featuredns.Client, releaseOutbounds func() error) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			var cleanupErrors []error
			if router != nil {
				cleanupErrors = append(cleanupErrors, common.Close(router))
			}
			if dnsBackend != nil {
				cleanupErrors = append(cleanupErrors, common.Close(dnsBackend))
			}
			if releaseOutbounds != nil {
				cleanupErrors = append(cleanupErrors, releaseOutbounds())
			}
			result = errors.Join(cleanupErrors...)
		})
		return result
	}
}

func (v *V2Core) ApplyRouteRuntime(infos []*panel.NodeInfo) (uint64, error) {
	if v == nil {
		return 0, errors.New("V2Core is nil")
	}
	v.access.Lock()
	defer v.access.Unlock()
	if v.Server == nil || v.routeRuntime == nil || v.routeOutboundPool == nil || v.ohm == nil {
		return 0, errors.New("route runtime is not initialized")
	}

	compiled, err := CompileRouteRuntime(infos)
	if err != nil {
		v.setRouteHealthLocked(err)
		return v.routeGeneration, err
	}
	dnsBackend, err := createRouteDNSBackend(v.Server, compiled.DNSConfig)
	if err != nil {
		v.setRouteHealthLocked(err)
		return v.routeGeneration, err
	}
	if err := dnsBackend.Start(); err != nil {
		_ = common.Close(dnsBackend)
		safeErr := errors.New("start candidate route DNS backend failed")
		v.setRouteHealthLocked(safeErr)
		return v.routeGeneration, safeErr
	}

	customHandlers, releaseOutbounds, err := v.routeOutboundPool.Acquire(context.Background(), compiled.CustomOutbounds)
	if err != nil {
		_ = common.Close(dnsBackend)
		v.setRouteHealthLocked(err)
		return v.routeGeneration, err
	}
	routeRouter, err := createRouteRouter(v.Server, compiled.RouterConfig)
	if err != nil {
		_ = releaseOutbounds()
		_ = common.Close(dnsBackend)
		v.setRouteHealthLocked(err)
		return v.routeGeneration, err
	}
	if err := routeRouter.Start(); err != nil {
		_ = common.Close(routeRouter)
		_ = releaseOutbounds()
		_ = common.Close(dnsBackend)
		safeErr := errors.New("start candidate route router failed")
		v.setRouteHealthLocked(safeErr)
		return v.routeGeneration, safeErr
	}

	handlers, defaultHandler, err := buildGenerationHandlers(compiled, v.ohm, customHandlers)
	cleanup := routeGenerationCleanup(routeRouter, dnsBackend, releaseOutbounds)
	if err != nil {
		_ = cleanup()
		v.setRouteHealthLocked(err)
		return v.routeGeneration, err
	}
	nextGeneration := v.routeGeneration + 1
	if err := v.routeRuntime.Publish(dispatcher.RouteRuntimeGeneration{
		ID:       nextGeneration,
		Router:   routeRouter,
		DNS:      dnsBackend,
		Handlers: handlers,
		Default:  defaultHandler,
		Cleanup:  cleanup,
	}); err != nil {
		_ = cleanup()
		v.setRouteHealthLocked(err)
		return v.routeGeneration, err
	}
	v.routeGeneration = nextGeneration
	v.routeDegraded = false
	v.routeHealthReason = ""
	return nextGeneration, nil
}

func (v *V2Core) setRouteHealthLocked(err error) {
	v.routeDegraded = err != nil
	if err == nil {
		v.routeHealthReason = ""
		return
	}
	v.routeHealthReason = err.Error()
}

func (v *V2Core) RouteRuntimeHealth() (bool, string) {
	if v == nil {
		return true, "V2Core is nil"
	}
	v.access.Lock()
	defer v.access.Unlock()
	return v.routeDegraded, v.routeHealthReason
}

func (v *V2Core) RouteRuntimeStats() dispatcher.RouteRuntimeStats {
	if v == nil {
		return dispatcher.RouteRuntimeStats{}
	}
	v.access.Lock()
	manager := v.routeRuntime
	v.access.Unlock()
	if manager == nil {
		return dispatcher.RouteRuntimeStats{}
	}
	return manager.Stats()
}
