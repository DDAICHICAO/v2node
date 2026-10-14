package core

import (
	"errors"
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/accessaudit"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core/app/dispatcher"
	_ "github.com/wyx2685/v2node/core/distro/all"
	"github.com/xtls/xray-core/app/dns"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/serial"
	xraycore "github.com/xtls/xray-core/core"
	featuredns "github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
	coreConf "github.com/xtls/xray-core/infra/conf"
	"google.golang.org/protobuf/proto"
)

type AddUsersParams struct {
	Tag   string
	Users []panel.UserInfo
	*panel.NodeInfo
}

type V2Core struct {
	Config            *conf.Conf
	ReloadCh          chan struct{}
	access            sync.Mutex
	Server            *xraycore.Instance
	users             *UserMap
	ihm               inbound.Manager
	ohm               outbound.Manager
	dispatcher        *dispatcher.DefaultDispatcher
	routeDNS          *dispatcher.RuntimeDNSClient
	routeRuntime      *dispatcher.RouteRuntimeManager
	routeOutboundPool *routeOutboundPool
	routeGeneration   uint64
	routeDegraded     bool
	routeHealthReason string
	eclipse           map[string]*SntpEclipseServer
	mieru             map[string]*MieruServer
}

type UserMap struct {
	uidMap  map[string]int
	mapLock sync.RWMutex
}

func New(config *conf.Conf) *V2Core {
	v2core := &V2Core{
		Config: config,
		users: &UserMap{
			uidMap: make(map[string]int),
		},
		eclipse: make(map[string]*SntpEclipseServer),
		mieru:   make(map[string]*MieruServer),
	}
	return v2core
}

func (v *V2Core) Start(infos []*panel.NodeInfo) error {
	v.access.Lock()
	defer v.access.Unlock()
	if v.eclipse == nil {
		v.eclipse = make(map[string]*SntpEclipseServer)
	}
	if v.mieru == nil {
		v.mieru = make(map[string]*MieruServer)
	}
	build, err := buildCore(v.Config, infos)
	if err != nil {
		return err
	}
	server := build.server
	cleanupServer := func() { _ = server.Close() }

	ihm, ok := server.GetFeature(inbound.ManagerType()).(inbound.Manager)
	if !ok || ihm == nil {
		cleanupServer()
		return errors.New("Xray inbound manager is unavailable")
	}
	ohm, ok := server.GetFeature(outbound.ManagerType()).(outbound.Manager)
	if !ok || ohm == nil {
		cleanupServer()
		return errors.New("Xray outbound manager is unavailable")
	}
	defaultDispatcher, ok := server.GetFeature(routing.DispatcherType()).(*dispatcher.DefaultDispatcher)
	if !ok || defaultDispatcher == nil {
		cleanupServer()
		return errors.New("Xray dispatcher is unavailable")
	}
	runtimeDNS, ok := server.GetFeature(featuredns.ClientType()).(*dispatcher.RuntimeDNSClient)
	if !ok || runtimeDNS == nil {
		cleanupServer()
		return errors.New("runtime DNS feature is unavailable")
	}
	initialRouter, ok := server.GetFeature(routing.RouterType()).(routing.Router)
	if !ok || initialRouter == nil {
		cleanupServer()
		return errors.New("Xray router is unavailable")
	}
	initialDNS, err := createRouteDNSBackend(server, build.compiled.DNSConfig)
	if err != nil {
		cleanupServer()
		return err
	}
	if err := initialDNS.Start(); err != nil {
		_ = common.Close(initialDNS)
		cleanupServer()
		return errors.New("start initial route DNS backend failed")
	}

	pool := newRouteOutboundPool(ohm, func(config *xraycore.OutboundHandlerConfig) (outbound.Handler, error) {
		created, err := xraycore.CreateObject(server, config)
		if err != nil {
			return nil, errors.New("Xray outbound object creation failed")
		}
		handler, ok := created.(outbound.Handler)
		if !ok {
			return nil, errors.New("Xray outbound object has an unexpected type")
		}
		return handler, nil
	})
	initialHandlers := listOutboundHandlers(ohm)
	initialRelease, err := pool.Adopt(build.compiled.CustomOutbounds, initialHandlers)
	if err != nil {
		_ = common.Close(initialDNS)
		cleanupServer()
		return err
	}
	generationHandlers, defaultHandler, err := buildGenerationHandlers(build.compiled, ohm, initialHandlers)
	if err != nil {
		_ = initialRelease()
		_ = common.Close(initialDNS)
		cleanupServer()
		return err
	}
	runtimeManager := dispatcher.NewRouteRuntimeManager(dispatcher.RouteRuntimeGeneration{
		ID:       0,
		Router:   initialRouter,
		DNS:      initialDNS,
		Handlers: generationHandlers,
		Default:  defaultHandler,
		Cleanup:  routeGenerationCleanup(initialRouter, initialDNS, initialRelease),
	})
	runtimeDNS.Bind(runtimeManager)
	defaultDispatcher.BindRouteRuntime(runtimeManager)
	if err := server.Start(); err != nil {
		_ = runtimeManager.Close()
		cleanupServer()
		return err
	}
	auditConfig, err := v.Config.AccessAuditConfig.RuntimeConfig()
	if err != nil {
		_ = runtimeManager.Close()
		cleanupServer()
		return err
	}
	if err := accessaudit.Configure(auditConfig); err != nil {
		_ = runtimeManager.Close()
		cleanupServer()
		return err
	}
	v.Server = server
	v.ihm = ihm
	v.ohm = ohm
	v.dispatcher = defaultDispatcher
	v.routeDNS = runtimeDNS
	v.routeRuntime = runtimeManager
	v.routeOutboundPool = pool
	v.routeGeneration = 0
	if _, strictErr := CompileRouteRuntime(infos); strictErr != nil {
		v.routeDegraded = true
		v.routeHealthReason = strictErr.Error()
	} else {
		v.routeDegraded = false
		v.routeHealthReason = ""
	}
	return nil
}

func (v *V2Core) Close() error {
	v.access.Lock()
	defer v.access.Unlock()
	v.Config = nil
	v.ihm = nil
	v.ohm = nil
	v.dispatcher = nil
	runtimeManager := v.routeRuntime
	v.routeRuntime = nil
	v.routeDNS = nil
	v.routeOutboundPool = nil
	for _, server := range v.eclipse {
		_ = server.Close()
	}
	v.eclipse = nil
	for _, server := range v.mieru {
		_ = server.Close()
	}
	v.mieru = nil
	accessaudit.Shutdown()
	if runtimeManager != nil {
		_ = runtimeManager.Close()
	}
	if v.Server != nil {
		err := v.Server.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

type coreBuild struct {
	server          *xraycore.Instance
	compiled        *CompiledRouteRuntime
	dnsConfig       *dns.Config
	routerConfig    *router.Config
	outboundConfigs []*xraycore.OutboundHandlerConfig
}

func buildCore(c *conf.Conf, infos []*panel.NodeInfo) (*coreBuild, error) {
	if c == nil {
		return nil, errors.New("v2node config is nil")
	}
	dispatcher.SetSntpAccessLogEnabled(c.LogConfig.SNTPAccess)

	// Log Config
	coreLogConfig := &coreConf.LogConfig{
		LogLevel:  c.LogConfig.Level,
		AccessLog: c.LogConfig.CoreAccessLog(),
		ErrorLog:  c.LogConfig.Output,
	}
	// Custom config
	compiled, err := compileRouteRuntime(infos, false)
	if err != nil {
		return nil, fmt.Errorf("build compatible route config: %w", err)
	}
	defaultOutbound, err := buildDefaultOutbound()
	if err != nil {
		return nil, err
	}
	blockOutbound, err := buildBlockOutbound()
	if err != nil {
		return nil, err
	}
	dnsOutbound, err := buildDnsOutbound()
	if err != nil {
		return nil, err
	}
	outboundConfigs := []*xraycore.OutboundHandlerConfig{defaultOutbound, blockOutbound, dnsOutbound}
	outboundConfigs = append(outboundConfigs, compiled.CustomOutbounds...)
	// Inbound config
	var inBoundConfig []*xraycore.InboundHandlerConfig

	// Policy config
	levelPolicyConfig := &coreConf.Policy{
		StatsUserUplink:   true,
		StatsUserDownlink: true,
		Handshake:         proto.Uint32(4),
		ConnectionIdle:    proto.Uint32(120),
		UplinkOnly:        proto.Uint32(2),
		DownlinkOnly:      proto.Uint32(4),
		BufferSize:        proto.Int32(128),
	}
	corePolicyConfig := &coreConf.PolicyConfig{}
	corePolicyConfig.Levels = map[uint32]*coreConf.Policy{0: levelPolicyConfig}
	policyConfig, err := corePolicyConfig.Build()
	if err != nil {
		return nil, fmt.Errorf("build core policy config: %w", err)
	}
	// Build Xray conf
	config := &xraycore.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(coreLogConfig.Build()),
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&stats.Config{}),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(policyConfig),
			serial.ToTypedMessage(dispatcher.RuntimeDNSFeatureConfig()),
			serial.ToTypedMessage(compiled.RouterConfig),
		},
		Inbound:  inBoundConfig,
		Outbound: outboundConfigs,
	}
	server, err := xraycore.New(config)
	if err != nil {
		return nil, fmt.Errorf("create Xray instance: %w", err)
	}
	log.Info("Xray Core Version: ", xraycore.Version())
	return &coreBuild{
		server:          server,
		compiled:        compiled,
		dnsConfig:       compiled.DNSConfig,
		routerConfig:    compiled.RouterConfig,
		outboundConfigs: outboundConfigs,
	}, nil
}
