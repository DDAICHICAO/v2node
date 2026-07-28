package node

import (
	"errors"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/netstat"
	"github.com/wyx2685/v2node/common/task"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
)

type Controller struct {
	server                  *core.V2Core
	apiClient               *panel.Client
	tag                     string
	limiter                 *limiter.Limiter
	userList                []panel.UserInfo
	aliveMap                map[int]int
	deviceAliveMap          map[int]int
	uuidIPFanoutGlobal      panel.UUIDIPFanoutGlobalState
	lastAliveRefresh        time.Time
	netSampler              *netstat.Sampler
	conf                    *conf.NodeConfig
	info                    *panel.NodeInfo
	nodeInfoMonitorPeriodic *task.Task
	userReportPeriodic      *task.Task
	renewCertPeriodic       *task.Task
	store                   *offlineStateStore
	bootstrap               *offlineState
	startedOffline          bool
	offlineTracker          offlineTracker
	pendingNodeInfo         *panel.NodeInfo
}

// NewController return a Node controller with default parameters.
func NewController(api *panel.Client, conf *conf.NodeConfig, store *offlineStateStore, bootstrap *offlineState, startedOffline bool) *Controller {
	controller := &Controller{
		apiClient:      api,
		conf:           conf,
		netSampler:     netstat.NewSampler(),
		store:          store,
		bootstrap:      bootstrap,
		startedOffline: startedOffline,
	}
	return controller
}

// Start implement the Start() function of the service interface
func (c *Controller) Start(x *core.V2Core) error {
	// Init Core
	c.server = x
	if c.bootstrap == nil || c.bootstrap.NodeInfo == nil {
		return errors.New("bootstrap state is incomplete")
	}
	node := c.bootstrap.NodeInfo
	c.info = node
	c.userList = make([]panel.UserInfo, len(c.bootstrap.Users))
	copy(c.userList, c.bootstrap.Users)
	c.aliveMap = cloneIntMap(c.bootstrap.Alive)
	c.deviceAliveMap = cloneIntMap(c.bootstrap.DeviceAlive)
	c.uuidIPFanoutGlobal = cloneUUIDIPFanoutGlobalState(c.bootstrap.UUIDIPFanout)
	if c.aliveMap == nil || c.deviceAliveMap == nil {
		return errors.New("bootstrap user state is incomplete")
	}
	c.tag = node.Tag

	// add limiter
	l := limiter.AddLimiter(c.info.Type, c.tag, c.userList, c.aliveMap, c.deviceAliveMap, c.supportsDeviceLimitByUUID())
	l.UpdateUUIDIPFanoutConfig(uuidIPFanoutLimiterConfig(c.info.Common.BaseConfig.UUIDIPFanoutGuard))
	l.UpdateUUIDIPFanoutGlobal(uuidIPFanoutLimiterGlobal(c.uuidIPFanoutGlobal), time.Now())
	c.limiter = l
	if node.Security == panel.Tls {
		err := c.requestCert()
		if err != nil {
			return fmt.Errorf("request cert error: %s", err)
		}
	}
	// Add new tag
	err := c.server.AddNode(c.tag, node)
	if err != nil {
		return fmt.Errorf("add new node error: %s", err)
	}
	added, err := c.server.AddUsers(&core.AddUsersParams{
		Tag:      c.tag,
		Users:    c.userList,
		NodeInfo: node,
	})
	if err != nil {
		return fmt.Errorf("add users error: %s", err)
	}
	log.WithField("tag", c.tag).Infof("Added %d new users", added)
	if _, _, err := c.netSampler.Sample(); err != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Debug("Prime network throughput sampler failed")
	}
	c.info = node
	if !c.startedOffline {
		if err := c.persistOfflineState(node); err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Persist initial offline snapshot failed")
		}
	} else {
		offlineErr := errors.New("panel unavailable during bootstrap; using offline snapshot")
		c.recordPanelFailureAt("sync", "bootstrap", offlineErr, time.Unix(c.bootstrap.SavedAt, 0))
		c.recordPanelFailureAt("sync", "bootstrap", offlineErr, time.Now())
	}
	c.startTasks(node)
	return nil
}

func (c *Controller) persistOfflineState(info *panel.NodeInfo) error {
	if c.store == nil {
		return errors.New("offline state store is nil")
	}
	state := &offlineState{
		Version:      offlineStateVersion,
		APIHost:      normalizeAPIHost(c.conf.APIHost),
		NodeID:       c.conf.NodeID,
		SavedAt:      time.Now().Unix(),
		NodeInfo:     info,
		Users:        append([]panel.UserInfo{}, c.userList...),
		Alive:        cloneIntMap(c.aliveMap),
		DeviceAlive:  cloneIntMap(c.deviceAliveMap),
		UUIDIPFanout: cloneUUIDIPFanoutGlobalState(c.uuidIPFanoutGlobal),
		UserSyncSeq:  c.apiClient.UserSyncSeq(),
	}
	return c.store.Save(*c.conf, state)
}

func (c *Controller) supportsDeviceLimitByUUID() bool {
	return c.info != nil &&
		c.info.Common != nil &&
		c.info.Common.BaseConfig != nil &&
		c.info.Common.BaseConfig.DeviceLimitByUUID
}

func (c *Controller) supportsDeviceAliveReport() bool {
	return c.info != nil &&
		c.info.Common != nil &&
		c.info.Common.BaseConfig != nil &&
		c.info.Common.BaseConfig.DeviceAliveReport
}

func (c *Controller) supportsDeviceTrafficReport() bool {
	return c.info != nil &&
		c.info.Common != nil &&
		c.info.Common.BaseConfig != nil &&
		c.info.Common.BaseConfig.DeviceTrafficReport
}

func uuidIPFanoutLimiterConfig(config *panel.UUIDIPFanoutConfig) limiter.UUIDIPFanoutConfig {
	if config == nil {
		return limiter.UUIDIPFanoutConfig{}
	}
	return limiter.UUIDIPFanoutConfig{
		Enabled:        config.Enabled,
		Mode:           config.Mode,
		Window:         time.Duration(config.WindowSeconds) * time.Second,
		MaxUniqueIPs:   config.MaxUniqueIPs,
		EventCooldown:  time.Duration(config.EventCooldownSeconds) * time.Second,
		WhitelistCIDRs: append([]string(nil), config.WhitelistCIDRs...),
	}
}

func uuidIPFanoutLimiterGlobal(state panel.UUIDIPFanoutGlobalState) limiter.UUIDIPFanoutGlobalState {
	result := limiter.UUIDIPFanoutGlobalState{
		Revision: state.Revision,
		Mode:     state.Mode,
		States:   make([]limiter.UUIDIPFanoutGlobalDecision, 0, len(state.States)),
	}
	for _, decision := range state.States {
		result.States = append(result.States, limiter.UUIDIPFanoutGlobalDecision{
			UserID:            decision.UserID,
			UUID:              decision.UUID,
			AllowedIPHashes:   append([]string(nil), decision.AllowedIPHashes...),
			DecisionExpiresAt: decision.DecisionExpiresAt,
			WindowSeconds:     decision.WindowSeconds,
			Threshold:         decision.Threshold,
		})
	}
	return result
}

func cloneUUIDIPFanoutGlobalState(state panel.UUIDIPFanoutGlobalState) panel.UUIDIPFanoutGlobalState {
	result := panel.UUIDIPFanoutGlobalState{
		Revision: state.Revision,
		Mode:     state.Mode,
		States:   make([]panel.UUIDIPFanoutGlobalDecision, 0, len(state.States)),
	}
	for _, decision := range state.States {
		decision.AllowedIPHashes = append([]string(nil), decision.AllowedIPHashes...)
		result.States = append(result.States, decision)
	}
	return result
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	limiter.DeleteLimiter(c.tag)
	if c.nodeInfoMonitorPeriodic != nil {
		c.nodeInfoMonitorPeriodic.Close()
	}
	if c.userReportPeriodic != nil {
		c.userReportPeriodic.Close()
	}
	if c.renewCertPeriodic != nil {
		c.renewCertPeriodic.Close()
	}
	err := c.server.DelNode(c.tag)
	if err != nil {
		return fmt.Errorf("del node error: %s", err)
	}
	return nil
}
