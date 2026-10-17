package node

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

// Validate prepares certificate files and protocol configuration while old nodes still serve.
func (n *Node) Validate(server *core.V2Core) error {
	if _, err := core.CompileRouteRuntime(n.NodeInfos); err != nil {
		return err
	}
	tags := make(map[string]bool, len(n.controllers))
	for _, c := range n.controllers {
		info := c.bootstrap.NodeInfo
		if info == nil || info.Common == nil || info.Common.BaseConfig == nil {
			return errors.New("incomplete candidate node configuration")
		}
		if tags[info.Tag] {
			return errors.New("duplicate node tag")
		}
		tags[info.Tag] = true
		c.info, c.tag = info, info.Tag
		if info.Security == panel.Tls && !c.usesManagedTLS() {
			if info.Common.CertInfo == nil {
				return errors.New("missing certificate configuration")
			}
			if err := c.requestCert(); err != nil {
				return fmt.Errorf("prepare node %d certificate: %w", c.conf.NodeID, err)
			}
		}
		if err := server.ValidateNode(info, c.bootstrap.Users); err != nil {
			return fmt.Errorf("validate node %d: %w", c.conf.NodeID, err)
		}
	}
	return nil
}

// RefreshPreparedUsers applies verified authorization changes before retaining or stopping
// listeners. A live sync that advanced during preparation must never be rolled back.
func (n *Node) RefreshPreparedUsers(prepared *Node) error {
	byIdentity := n.controllersByIdentity()
	for _, cfg := range prepared.deletedConfigs {
		if old := byIdentity[routeRuntimeIdentity(cfg.APIHost, cfg.NodeID)]; old != nil &&
			offlineConfigFingerprint(*old.conf) == offlineConfigFingerprint(cfg) {
			old.configFetchMu.Lock()
			old.markDeleted()
			old.configFetchMu.Unlock()
		}
	}
	for _, candidate := range prepared.controllers {
		old := byIdentity[routeRuntimeIdentity(candidate.conf.APIHost, candidate.conf.NodeID)]
		if old == nil || old.deleted.Load() || offlineConfigFingerprint(*old.conf) != offlineConfigFingerprint(*candidate.conf) {
			continue
		}
		old.userSyncMu.Lock()
		oldSeq := old.apiClient.UserSyncSeq()
		var err error
		if candidate.bootstrap.UsersAuthoritative && candidate.bootstrap.UserSyncSeq >= oldSeq {
			err = old.commitPreparedUsers(candidate.bootstrap)
		}
		inheritControllerUsers(candidate, old)
		old.userSyncMu.Unlock()
		if err != nil {
			return fmt.Errorf("refresh retained node %d users: %w", old.conf.NodeID, err)
		}
	}
	return nil
}

// Managed TLS controllers waiting for their first certificate have no data plane yet.
// Serialize with activation while updating the same durable authorization state.
func (c *Controller) commitPreparedUsers(state *offlineState) error {
	c.runtime.mu.Lock()
	defer c.runtime.mu.Unlock()
	users, _ := removeExpiredUsers(state.Users, time.Now().Unix())
	c.stateMu.RLock()
	changed := !reflect.DeepEqual(c.userList, users)
	ready := c.fastSyncReady
	c.stateMu.RUnlock()
	if !changed && state.UserSyncSeq == c.apiClient.UserSyncSeq() && ready == state.FastSyncReady {
		return nil
	}
	c.stateMu.Lock()
	c.fastSyncReady = state.FastSyncReady
	c.stateMu.Unlock()
	var err error
	if c.runtime.started {
		err = c.commitUserState(users, state.UserSyncSeq)
	} else {
		c.stateMu.Lock()
		err = c.persistOfflineStateAtLocked(c.info, users, state.UserSyncSeq)
		if err == nil {
			c.userList = append([]panel.UserInfo{}, users...)
			c.apiClient.SetUserSyncSeq(state.UserSyncSeq)
		}
		c.stateMu.Unlock()
	}
	if err != nil {
		c.stateMu.Lock()
		c.fastSyncReady = ready
		c.stateMu.Unlock()
	}
	return err
}

func (n *Node) controllersByIdentity() map[string]*Controller {
	result := make(map[string]*Controller, len(n.controllers))
	for _, c := range n.controllers {
		result[routeRuntimeIdentity(c.conf.APIHost, c.conf.NodeID)] = c
	}
	return result
}

// InheritStoppedUsers runs after tasks stop, closing the preparation/switch sync window.
func (n *Node) InheritStoppedUsers(prepared *Node) {
	byIdentity := n.controllersByIdentity()
	for _, candidate := range prepared.controllers {
		if old := byIdentity[routeRuntimeIdentity(candidate.conf.APIHost, candidate.conf.NodeID)]; old != nil {
			inheritControllerUsers(candidate, old)
		}
	}
}

func inheritControllerUsers(candidate, old *Controller) {
	if old.deleted.Load() {
		return
	}
	if offlineConfigFingerprint(*old.conf) != offlineConfigFingerprint(*candidate.conf) {
		return
	}
	old.stateMu.RLock()
	defer old.stateMu.RUnlock()
	seq := old.apiClient.UserSyncSeq()
	if seq < candidate.bootstrap.UserSyncSeq {
		return
	}
	// Equal sequence can still include locally expired users removed by the live scheduler.
	candidate.bootstrap.Users, _ = removeExpiredUsers(append([]panel.UserInfo{}, old.userList...), time.Now().Unix())
	candidate.bootstrap.FastSyncReady = old.fastSyncReady
	candidate.fastSyncReady = old.fastSyncReady
	candidate.bootstrap.UserSyncSeq = seq
	candidate.apiClient.SetUserSyncSeq(seq)
}

// RestartCopy captures the latest stopped runtime state without re-fetching the panel.
func (n *Node) RestartCopy() (*Node, error) {
	copy := &Node{routeStateStore: n.routeStateStore}
	for _, c := range n.controllers {
		if c.deleted.Load() {
			continue
		}
		replacement, err := restartController(c)
		if err != nil {
			return nil, err
		}
		copy.controllers = append(copy.controllers, replacement)
		copy.NodeInfos = append(copy.NodeInfos, replacement.bootstrap.NodeInfo)
	}
	return copy, nil
}

func restartController(c *Controller) (*Controller, error) {
	c.stateMu.RLock()
	info, err := cloneRouteRuntimeNodeInfo(c.info)
	users := append([]panel.UserInfo{}, c.userList...)
	state := &offlineState{Version: offlineStateVersion, FastSyncReady: c.fastSyncReady, APIHost: normalizeAPIHost(c.conf.APIHost), NodeID: c.conf.NodeID, SavedAt: time.Now().Unix(), NodeInfo: info,
		Users: users, Alive: cloneIntMap(c.aliveMap), DeviceAlive: cloneIntMap(c.deviceAliveMap), UUIDIPFanout: cloneUUIDIPFanoutGlobalState(c.uuidIPFanoutGlobal), UserSyncSeq: c.apiClient.UserSyncSeq()}
	c.stateMu.RUnlock()
	if err != nil {
		return nil, err
	}
	state.Users, _ = removeExpiredUsers(state.Users, time.Now().Unix())
	api, err := panel.New(c.conf)
	if err != nil {
		return nil, err
	}
	api.SetUserSyncSeq(state.UserSyncSeq)
	replacement := NewController(api, c.conf, c.store, state, c.startedOffline)
	replacement.activeNodeInfoVersion = nodeInfoContentVersion(info)
	return replacement, nil
}

func (n *Node) Configs() []conf.NodeConfig {
	configs := make([]conf.NodeConfig, 0, len(n.controllers))
	for _, c := range n.controllers {
		if !c.deleted.Load() {
			configs = append(configs, *c.conf)
		}
	}
	return configs
}

func (n *Node) resetCoordinator(server *core.V2Core) {
	var coordinator *routeUpdateCoordinator
	if routeHotReloadEnabled(server) && len(n.controllers) > 0 {
		targets := make([]routeUpdateTarget, len(n.controllers))
		for i, c := range n.controllers {
			targets[i] = c
		}
		coordinator = newRouteUpdateCoordinator(targets, server, n.routeStateStore, routeUpdateCoordinatorOptions{})
	}
	for _, c := range n.controllers {
		c.nodeInfoApplyMu.Lock()
		c.routeCoordinator = coordinator
		c.nodeInfoApplyMu.Unlock()
	}
	n.routeCoordinator = coordinator
	if coordinator != nil {
		coordinator.Start()
		coordinator.Notify()
	}
}

// Reconcile keeps the shared core and unchanged controllers when compiled routes match.
// true means this path owns both success and rollback; false requests a full core swap.
func (n *Node) Reconcile(prepared *Node, server *core.V2Core) (applied bool, reconcileErr error) {
	if n.routeCoordinator != nil {
		n.routeCoordinator.Close()
		n.routeCoordinator = nil
	}
	defer func() {
		if n.routeCoordinator == nil && routeHotReloadEnabled(server) {
			n.resetCoordinator(server)
		}
	}()
	// A deleted node must never participate in retained-state rollback.
	originalControllers := n.controllers
	active := make([]*Controller, 0, len(n.controllers))
	retired := false
	for _, c := range n.controllers {
		if c.deleted.Load() {
			if err := c.RetireDeletedNode(); err != nil {
				return true, err
			}
			retired = true
			continue
		}
		active = append(active, c)
	}
	n.controllers = active
	n.refreshNodeInfos()
	if retired {
		if !routeHotReloadEnabled(server) {
			return false, nil
		}
		if _, err := server.ApplyRouteRuntime(n.NodeInfos); err != nil {
			// Retain tombstoned targets so the coordinator can retry removing
			// their routes without resurrecting their listeners.
			n.controllers = originalControllers
			n.refreshNodeInfos()
			return true, err
		}
	}
	currentInfos := make([]*panel.NodeInfo, len(n.controllers))
	for i, c := range n.controllers {
		currentInfos[i] = c.currentNodeInfo()
	}
	currentRoutes, err := core.CompileRouteRuntime(currentInfos)
	if err != nil {
		return false, nil
	}
	nextRoutes, err := core.CompileRouteRuntime(prepared.NodeInfos)
	if err != nil {
		return true, err
	}
	if currentRoutes.ConfigHash != nextRoutes.ConfigHash {
		return false, nil
	}
	byIdentity := make(map[string]*Controller, len(n.controllers))
	for _, c := range n.controllers {
		byIdentity[routeRuntimeIdentity(c.conf.APIHost, c.conf.NodeID)] = c
	}
	keep := make(map[*Controller]bool)
	next := make([]*Controller, len(prepared.controllers))
	for i, candidate := range prepared.controllers {
		old := byIdentity[routeRuntimeIdentity(candidate.conf.APIHost, candidate.conf.NodeID)]
		if old != nil && reflect.DeepEqual(*old.conf, *candidate.conf) && reflect.DeepEqual(old.currentNodeInfo(), candidate.bootstrap.NodeInfo) {
			next[i] = old
			keep[old] = true
		} else {
			next[i] = candidate
		}
	}
	removed := make([]*Controller, 0)
	for _, c := range n.controllers {
		if !keep[c] {
			removed = append(removed, c)
		}
	}
	if len(removed) == 0 && len(keep) == len(next) {
		n.controllers = next
		n.refreshNodeInfos()
		n.resetCoordinator(server)
		return true, nil
	}
	if n.routeCoordinator != nil {
		n.routeCoordinator.Close()
	}
	// Preserve the original ordering and ownership for rollback.
	original := append([]*Controller(nil), n.controllers...)
	stopped := make(map[*Controller]bool)
	added := make([]*Controller, 0)
	rollback := func(cause error) (bool, error) {
		var rollbackErrors []error
		for _, c := range added {
			if err := c.Close(); err != nil {
				rollbackErrors = append(rollbackErrors, err)
			}
		}
		restored := make([]*Controller, len(original))
		for i, c := range original {
			restored[i] = c
			if stopped[c] {
				replacement, err := restartController(c)
				if err == nil {
					err = replacement.Start(server)
				}
				if err != nil {
					rollbackErrors = append(rollbackErrors, fmt.Errorf("restore node %d: %w", c.conf.NodeID, err))
					continue
				}
				restored[i] = replacement
			}
		}
		n.controllers = restored
		n.refreshNodeInfos()
		n.resetCoordinator(server)
		if len(rollbackErrors) > 0 {
			return true, &RollbackError{Err: errors.Join(append([]error{cause}, rollbackErrors...)...)}
		}
		return true, cause
	}
	for _, c := range removed {
		stopped[c] = true
		if err := c.Close(); err != nil {
			return rollback(err)
		}
	}
	for _, c := range next {
		if old := byIdentity[routeRuntimeIdentity(c.conf.APIHost, c.conf.NodeID)]; old != nil && stopped[old] {
			inheritControllerUsers(c, old)
		}
		if keep[c] {
			continue
		}
		// Include partially-started candidates in cleanup too.
		added = append(added, c)
		if err := c.Start(server); err != nil {
			return rollback(fmt.Errorf("start replacement node %d: %w", c.conf.NodeID, err))
		}
	}
	n.controllers = next
	n.refreshNodeInfos()
	n.resetCoordinator(server)
	log.WithFields(log.Fields{"kept": len(keep), "stopped": len(removed), "started": len(added)}).Info("SNTP node configuration applied without restarting shared core")
	return true, nil
}

func (n *Node) refreshNodeInfos() {
	n.NodeInfos = make([]*panel.NodeInfo, len(n.controllers))
	for i, c := range n.controllers {
		n.NodeInfos[i] = c.currentNodeInfo()
	}
}

// RollbackError means some previous listeners could not be restored.
type RollbackError struct{ Err error }

func (e *RollbackError) Error() string { return e.Err.Error() }
func (e *RollbackError) Unwrap() error { return e.Err }
