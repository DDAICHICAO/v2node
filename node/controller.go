package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
	c.installUUIDIPFanoutReservation(l)
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
	result := limiter.UUIDIPFanoutConfig{
		Enabled:        config.Enabled,
		Mode:           config.Mode,
		Window:         time.Duration(config.WindowSeconds) * time.Second,
		MaxUniqueIPs:   config.MaxUniqueIPs,
		EventCooldown:  time.Duration(config.EventCooldownSeconds) * time.Second,
		WhitelistCIDRs: append([]string(nil), config.WhitelistCIDRs...),
		Strategy:       strings.ToLower(strings.TrimSpace(config.Strategy)),
	}
	if config.Reservation != nil {
		result.ReservationEnabled = config.Reservation.Enabled
		result.ReservationTimeout = time.Duration(config.Reservation.TimeoutMS) * time.Millisecond
	}
	return result
}

func (c *Controller) installUUIDIPFanoutReservation(l *limiter.Limiter) {
	if c == nil || c.apiClient == nil || l == nil {
		return
	}
	l.SetUUIDIPFanoutReservationFunc(func(
		ctx context.Context,
		candidate limiter.UUIDIPFanoutInspection,
	) limiter.UUIDIPFanoutReservationOutcome {
		now := time.Now()
		data, err := c.apiClient.ReserveUUIDIPFanout(ctx, panel.FanoutReservationRequest{
			UserID:     candidate.UserID,
			UUID:       candidate.UUID,
			IP:         candidate.IP,
			RequestID:  fanoutReservationRequestID(c.apiClient.InstanceID(), candidate, now),
			ObservedAt: now.Unix(),
		})
		if panel.FanoutReservationStatus(err) == http.StatusUnprocessableEntity {
			c.apiClient.MarkUserSyncFullRequired()
		}
		return mapFanoutReservationOutcome(data, err)
	})
}

func fanoutReservationRequestID(
	instanceID string,
	candidate limiter.UUIDIPFanoutInspection,
	now time.Time,
) string {
	windowSeconds := candidate.WindowSeconds
	if windowSeconds < 60 {
		windowSeconds = 60
	}
	uuidHash := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(candidate.UUID))))
	ipHash := sha256.Sum256([]byte(strings.TrimSpace(candidate.IP)))
	components := []string{
		strings.TrimSpace(instanceID),
		strconv.Itoa(candidate.UserID),
		hex.EncodeToString(uuidHash[:]),
		hex.EncodeToString(ipHash[:]),
		strconv.FormatInt(now.Unix()/int64(windowSeconds), 10),
	}
	sum := sha256.Sum256([]byte(strings.Join(components, "\x00")))
	return hex.EncodeToString(sum[:])
}

func mapFanoutReservationOutcome(
	data *panel.FanoutReservationData,
	err error,
) limiter.UUIDIPFanoutReservationOutcome {
	if err != nil {
		switch {
		case errors.Is(err, panel.ErrFanoutReservationFallback):
			return limiter.UUIDIPFanoutReservationOutcome{Kind: limiter.UUIDIPFanoutOutcomeFallback}
		case errors.Is(err, panel.ErrFanoutReservationProtocol):
			return limiter.UUIDIPFanoutReservationOutcome{Kind: limiter.UUIDIPFanoutOutcomeProtocol}
		default:
			return limiter.UUIDIPFanoutReservationOutcome{Kind: limiter.UUIDIPFanoutOutcomeFailOpen}
		}
	}
	if data == nil {
		return limiter.UUIDIPFanoutReservationOutcome{Kind: limiter.UUIDIPFanoutOutcomeProtocol}
	}

	outcome := limiter.UUIDIPFanoutReservationOutcome{
		Scope:         data.Scope,
		UniqueIPCount: data.UniqueIPCount,
		Threshold:     data.Threshold,
		WindowSeconds: data.WindowSeconds,
	}
	switch data.Kind {
	case panel.FanoutReservationAllow:
		outcome.Kind = limiter.UUIDIPFanoutOutcomeAllow
	case panel.FanoutReservationReject:
		outcome.Kind = limiter.UUIDIPFanoutOutcomeReject
	default:
		outcome.Kind = limiter.UUIDIPFanoutOutcomeProtocol
	}
	return outcome
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
