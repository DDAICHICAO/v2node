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
	"sync"
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
	server                   *core.V2Core
	apiClient                *panel.Client
	tag                      string
	limiter                  *limiter.Limiter
	userList                 []panel.UserInfo
	aliveMap                 map[int]int
	deviceAliveMap           map[int]int
	uuidIPFanoutGlobal       panel.UUIDIPFanoutGlobalState
	netSampler               *netstat.Sampler
	conf                     *conf.NodeConfig
	info                     *panel.NodeInfo
	nodeInfoMonitorPeriodic  *task.Task
	aliveStatePeriodic       *task.Task
	userReportPeriodic       *task.Task
	renewCertPeriodic        *task.Task
	managedTLSStatusPeriodic *task.Task
	managedTLS               *managedTLSManager
	managedTLSStatusMu       sync.Mutex
	managedTLSActivationMu   sync.Mutex
	managedTLSRuntimeVersion uint64
	replaceManagedTLSInbound func(string, *panel.NodeInfo, []panel.UserInfo) error
	runtime                  controllerRuntimeLifecycle
	store                    *offlineStateStore
	bootstrap                *offlineState
	startedOffline           bool
	offlineTracker           offlineTracker
	pendingNodeInfo          *panel.NodeInfo
	userSyncMu               sync.Mutex
	stateMu                  sync.Mutex
	userSyncCancel           context.CancelFunc
	userSyncDone             chan struct{}
	userSyncRuntime          *userSyncRuntime
	fullSyncBreaker          fullSyncBreaker
	expiryWakeCh             chan struct{}
	expiryCancel             context.CancelFunc
	expiryDone               chan struct{}
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
	if c.usesManagedTLS() {
		store := newManagedTLSFileStore("/etc/v2node/certificates", c.conf.NodeID)
		c.managedTLS = newManagedTLSManager(
			c.apiClient,
			store,
			&managedTLSLegoIssuer{},
			node.Common.TlsSettings.CertificateScopeID,
			node.Common.TlsSettings.PrimaryServerName(),
		)
		prepareErr := c.managedTLS.Prepare(time.Now().UTC())
		if prepareErr != nil && !errors.Is(prepareErr, ErrManagedTLSPending) {
			return fmt.Errorf("prepare managed TLS certificate: %w", prepareErr)
		}
		if prepareErr == nil {
			if err := c.startRuntime(); err != nil {
				return err
			}
			c.managedTLSRuntimeVersion = c.managedTLS.Snapshot().Version
			c.managedTLS.Start(context.Background(), c.activateManagedTLSRuntime)
			return nil
		}

		c.startManagedTLSStatusReporter()
		c.managedTLS.Start(context.Background(), c.activateManagedTLSRuntime)
		log.WithFields(log.Fields{
			"tag":      c.tag,
			"node_id":  c.conf.NodeID,
			"scope_id": node.Common.TlsSettings.CertificateScopeID,
		}).Warning("Managed TLS certificate is not ready; this node is waiting without blocking other nodes")
		return nil
	}

	return c.startRuntime()
}

func (c *Controller) usesManagedTLS() bool {
	return c != nil && c.info != nil && c.info.Security == panel.Tls &&
		c.info.Common != nil && c.info.Common.CertInfo != nil &&
		strings.EqualFold(strings.TrimSpace(c.info.Common.CertInfo.CertMode), "managed")
}

func (c *Controller) activateManagedTLSRuntime() error {
	if c.managedTLS == nil {
		return errors.New("managed TLS manager is nil")
	}
	return c.activateManagedTLSRuntimeVersion(c.managedTLS.Snapshot().Version)
}

func (c *Controller) activateManagedTLSRuntimeVersion(version uint64) error {
	if version == 0 {
		return errors.New("managed TLS runtime version is empty")
	}
	c.managedTLSActivationMu.Lock()
	defer c.managedTLSActivationMu.Unlock()

	if !c.runtime.Started() {
		if err := c.startRuntime(); err != nil {
			log.WithFields(log.Fields{"tag": c.tag, "err": err}).Error("Start node after managed TLS became ready failed")
			return err
		}
		c.managedTLSRuntimeVersion = version
		c.stopManagedTLSStatusReporter()
		return nil
	}
	if c.managedTLSRuntimeVersion == version {
		return nil
	}

	c.stateMu.Lock()
	users := append([]panel.UserInfo(nil), c.userList...)
	replace := c.replaceManagedTLSInbound
	if replace == nil {
		replace = c.server.ReplaceNode
	}
	err := replace(c.tag, c.info, users)
	c.stateMu.Unlock()
	if err != nil {
		return fmt.Errorf("activate managed TLS runtime version %d: %w", version, err)
	}
	c.managedTLSRuntimeVersion = version

	nodeID := 0
	if c.conf != nil {
		nodeID = c.conf.NodeID
	}
	scopeID := uint64(0)
	if c.managedTLS != nil {
		scopeID = c.managedTLS.Snapshot().ScopeID
	}
	log.WithFields(log.Fields{
		"tag": c.tag, "node_id": nodeID,
		"scope_id": scopeID, "version": version,
	}).Info("Managed TLS runtime certificate activated")
	return nil
}

func (c *Controller) startRuntime() error {
	return c.runtime.Start(func() error {
		node := c.info

		// add limiter
		l := limiter.AddLimiter(c.info.Type, c.tag, c.userList, c.aliveMap, c.deviceAliveMap, c.supportsDeviceLimitByUUID())
		l.UpdateUUIDIPFanoutConfig(uuidIPFanoutLimiterConfig(c.info.Common.BaseConfig.UUIDIPFanoutGuard))
		l.UpdateUUIDIPFanoutGlobal(uuidIPFanoutLimiterGlobal(c.uuidIPFanoutGlobal), time.Now())
		c.limiter = l
		c.installUUIDIPFanoutReservation(l)
		if node.Security == panel.Tls && !c.usesManagedTLS() {
			if err := c.requestCert(); err != nil {
				limiter.DeleteLimiter(c.tag)
				c.limiter = nil
				return fmt.Errorf("request cert error: %s", err)
			}
		}
		// Add new tag
		if err := c.server.AddNode(c.tag, node); err != nil {
			limiter.DeleteLimiter(c.tag)
			c.limiter = nil
			return fmt.Errorf("add new node error: %s", err)
		}
		added, err := c.server.AddUsers(&core.AddUsersParams{
			Tag:      c.tag,
			Users:    c.userList,
			NodeInfo: node,
		})
		if err != nil {
			if deleteErr := c.server.DelNode(c.tag); deleteErr != nil {
				log.WithFields(log.Fields{"tag": c.tag, "err": deleteErr}).Warning("Rollback node after adding users failed")
			}
			limiter.DeleteLimiter(c.tag)
			c.limiter = nil
			return fmt.Errorf("add users error: %s", err)
		}
		log.WithField("tag", c.tag).Infof("Added %d new users", added)
		if _, _, err := c.netSampler.Sample(); err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Debug("Prime network throughput sampler failed")
		}
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
	})
}

func (c *Controller) startManagedTLSStatusReporter() {
	c.managedTLSStatusMu.Lock()
	defer c.managedTLSStatusMu.Unlock()
	if c.managedTLSStatusPeriodic != nil {
		return
	}
	c.managedTLSStatusPeriodic = &task.Task{
		Name:            "managedTLSStatusTask",
		Interval:        time.Minute,
		Execute:         c.reportManagedTLSRuntimeStatus,
		ReloadOnTimeout: false,
	}
	_ = c.managedTLSStatusPeriodic.Start(true)
}

func (c *Controller) stopManagedTLSStatusReporter() {
	c.managedTLSStatusMu.Lock()
	periodic := c.managedTLSStatusPeriodic
	c.managedTLSStatusPeriodic = nil
	c.managedTLSStatusMu.Unlock()
	if periodic != nil {
		periodic.Close()
	}
}

func (c *Controller) persistOfflineState(info *panel.NodeInfo) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.persistOfflineStateAtLocked(
		info,
		c.userList,
		c.apiClient.UserSyncSeq(),
	)
}

func (c *Controller) persistOfflineStateAt(
	info *panel.NodeInfo,
	users []panel.UserInfo,
	seq int64,
) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.persistOfflineStateAtLocked(info, users, seq)
}

func (c *Controller) persistOfflineStateAtLocked(
	info *panel.NodeInfo,
	users []panel.UserInfo,
	seq int64,
) error {
	if c.store == nil {
		return errors.New("offline state store is nil")
	}
	state := &offlineState{
		Version:      offlineStateVersion,
		APIHost:      normalizeAPIHost(c.conf.APIHost),
		NodeID:       c.conf.NodeID,
		SavedAt:      time.Now().Unix(),
		NodeInfo:     info,
		Users:        append([]panel.UserInfo{}, users...),
		Alive:        cloneIntMap(c.aliveMap),
		DeviceAlive:  cloneIntMap(c.deviceAliveMap),
		UUIDIPFanout: cloneUUIDIPFanoutGlobalState(c.uuidIPFanoutGlobal),
		UserSyncSeq:  seq,
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
	if c.managedTLS != nil {
		c.managedTLS.Close()
	}
	c.stopManagedTLSStatusReporter()
	c.cancelUserExpiryScheduler()
	if c.nodeInfoMonitorPeriodic != nil {
		c.nodeInfoMonitorPeriodic.Close()
	}
	if c.aliveStatePeriodic != nil {
		c.aliveStatePeriodic.Close()
	}
	if c.userReportPeriodic != nil {
		c.userReportPeriodic.Close()
	}
	if c.renewCertPeriodic != nil {
		c.renewCertPeriodic.Close()
	}
	c.closeUserSyncRuntime()
	c.waitUserExpiryScheduler()
	return c.runtime.Close(func() error {
		limiter.DeleteLimiter(c.tag)
		c.limiter = nil
		if err := c.server.DelNode(c.tag); err != nil {
			return fmt.Errorf("del node error: %s", err)
		}
		return nil
	})
}

type controllerRuntimeLifecycle struct {
	mu      sync.Mutex
	started bool
	closed  bool
}

func (l *controllerRuntimeLifecycle) Start(start func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("controller runtime is closed")
	}
	if l.started {
		return nil
	}
	if err := start(); err != nil {
		return err
	}
	l.started = true
	return nil
}

func (l *controllerRuntimeLifecycle) Started() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.started
}

func (l *controllerRuntimeLifecycle) Close(stop func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if !l.started {
		return nil
	}
	if err := stop(); err != nil {
		return err
	}
	l.started = false
	return nil
}
