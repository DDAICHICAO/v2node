package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

type bootstrapPanel interface {
	GetNodeInfo(context.Context) (*panel.NodeInfo, error)
	GetUserList(context.Context) ([]panel.UserInfo, error)
	GetUserAlive(context.Context) (map[int]int, error)
	GetUserDeviceAliveState(context.Context) (*panel.DeviceAliveMap, error)
	SetUserSyncSeq(int64)
	UserSyncSeq() int64
}

type bootstrapDeltaPanel interface {
	GetUserDeltaSince(context.Context, int64) (*panel.UserDeltaData, error)
	GetFullUserList(context.Context) ([]panel.UserInfo, error)
}

const bootstrapSnapshotMaxAge = 24 * time.Hour

func loadBootstrapUsers(ctx context.Context, client bootstrapPanel, cached *offlineState, onlineConfig bool, cfg conf.NodeConfig) ([]panel.UserInfo, bool, error) {
	forceFull := false
	if deltaClient, ok := client.(bootstrapDeltaPanel); ok && onlineConfig && cached != nil &&
		cached.FastSyncReady && cached.ConfigFingerprint == offlineConfigFingerprint(cfg) && cached.UserSyncSeq > 0 && cached.SavedAt <= time.Now().Unix() && time.Since(time.Unix(cached.SavedAt, 0)) <= bootstrapSnapshotMaxAge {
		delta, err := collectUserDeltaPages(ctx, cached.UserSyncSeq, 10, deltaClient.GetUserDeltaSince)
		if err == nil && delta != nil && !delta.FullRequired && !delta.HasMore && validateBootstrapUserDelta(delta, cached.UserSyncSeq) == nil {
			users, _ := applyUserDeltaEvents(cached.Users, delta.Events)
			pruneAt := time.Now().Unix()
			if delta.ServerTime > pruneAt {
				pruneAt = delta.ServerTime
			}
			users, _ = removeExpiredUsers(users, pruneAt)
			client.SetUserSyncSeq(delta.LatestSeq)
			return append([]panel.UserInfo{}, users...), false, nil
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if panel.IsValidationError(err) {
			return nil, false, err
		}
		// A rejected or incomplete delta cannot authorize a stale snapshot.
		forceFull = err == nil || !errors.Is(err, panel.ErrUserDeltaUnsupported)
	}
	var users []panel.UserInfo
	var err error
	if deltaClient, ok := client.(bootstrapDeltaPanel); ok && forceFull {
		users, err = deltaClient.GetFullUserList(ctx)
	} else {
		users, err = client.GetUserList(ctx)
	}
	if err != nil || users == nil {
		if forceFull || panel.IsValidationError(err) || cached == nil || cached.Users == nil {
			return nil, false, fmt.Errorf("get user list without an authorized snapshot: %w", nonNilError(err))
		}
		client.SetUserSyncSeq(cached.UserSyncSeq)
		users, _ = removeExpiredUsers(cached.Users, time.Now().Unix())
		return append([]panel.UserInfo{}, users...), true, nil
	}
	users, _ = removeExpiredUsers(users, time.Now().Unix())
	return append([]panel.UserInfo{}, users...), false, nil
}

func loadBootstrapState(ctx context.Context, client bootstrapPanel, cfg conf.NodeConfig, cached *offlineState) (*offlineState, bool, error) {
	started := time.Now()
	defer func() { logBootstrapDuration(cfg.NodeID, "bootstrap", started) }()
	if cached != nil && validateOfflineState(cfg, cached) != nil {
		cached = nil
	}
	state := &offlineState{
		Version: offlineStateVersion, APIHost: normalizeAPIHost(cfg.APIHost), NodeID: cfg.NodeID,
		SavedAt: time.Now().Unix(), DeviceAlive: map[int]int{},
		UUIDIPFanout: panel.UUIDIPFanoutGlobalState{States: []panel.UUIDIPFanoutGlobalDecision{}},
	}
	infoStarted := time.Now()
	info, err := client.GetNodeInfo(ctx)
	logBootstrapDuration(cfg.NodeID, "config", infoStarted)
	if errors.Is(err, panel.ErrNodeNotFound) {
		return nil, false, err
	}
	if cached != nil && cached.Deleted {
		if panel.IsValidationError(err) {
			return nil, false, err
		}
		if err != nil || info == nil {
			return nil, false, panel.ErrNodeNotFound
		}
		cached = nil
	}
	offlineConfig := err != nil || info == nil
	if offlineConfig {
		if panel.IsValidationError(err) || cached == nil {
			return nil, false, fmt.Errorf("get node info and no authorized offline snapshot: %w", nonNilError(err))
		}
		info = cached.NodeInfo
	}
	state.NodeInfo = info
	var wg sync.WaitGroup
	var usersOffline, aliveOffline, deviceOffline bool
	var usersErr, aliveErr, deviceErr error
	wg.Go(func() {
		start := time.Now()
		defer logBootstrapDuration(cfg.NodeID, "users", start)
		state.Users, usersOffline, usersErr = loadBootstrapUsers(ctx, client, cached, !offlineConfig, cfg)
	})
	wg.Go(func() {
		start := time.Now()
		defer logBootstrapDuration(cfg.NodeID, "alive", start)
		alive, err := client.GetUserAlive(ctx)
		if err != nil || alive == nil {
			if panel.IsValidationError(err) || cached == nil || cached.Alive == nil {
				aliveErr = fmt.Errorf("get alive state and no offline snapshot: %w", nonNilError(err))
				return
			}
			alive = cached.Alive
			aliveOffline = true
		}
		state.Alive = cloneIntMap(alive)
	})
	if info.Common != nil && info.Common.BaseConfig != nil && (info.Common.BaseConfig.DeviceLimitByUUID || info.Common.BaseConfig.DeviceAliveReport) {
		wg.Go(func() {
			start := time.Now()
			defer logBootstrapDuration(cfg.NodeID, "device_alive", start)
			deviceState, err := client.GetUserDeviceAliveState(ctx)
			if err != nil || deviceState == nil {
				if panel.IsValidationError(err) || cached == nil || cached.DeviceAlive == nil {
					deviceErr = fmt.Errorf("get device alive state and no offline snapshot: %w", nonNilError(err))
					return
				}
				state.DeviceAlive = cloneIntMap(cached.DeviceAlive)
				state.UUIDIPFanout = cloneUUIDIPFanoutGlobalState(cached.UUIDIPFanout)
				deviceOffline = true
			} else {
				state.DeviceAlive = cloneIntMap(deviceState.AliveDevices)
				state.UUIDIPFanout = cloneUUIDIPFanoutGlobalState(deviceState.UUIDIPFanout)
			}
		})
	}
	wg.Wait()
	if err := errors.Join(usersErr, aliveErr, deviceErr); err != nil {
		return nil, false, err
	}
	state.UserSyncSeq = client.UserSyncSeq()
	state.UsersAuthoritative = !usersOffline
	state.FastSyncReady = !usersOffline || (cached != nil && cached.FastSyncReady)
	usedSnapshot := offlineConfig || usersOffline || aliveOffline || deviceOffline
	if usedSnapshot {
		state.SavedAt = cached.SavedAt
	}
	return state, usedSnapshot, nil
}

func logBootstrapDuration(nodeID int, stage string, start time.Time) {
	duration := time.Since(start)
	entry := log.WithFields(log.Fields{"node_id": nodeID, "stage": stage, "elapsed_ms": duration.Milliseconds()})
	if duration >= 5*time.Second {
		entry.Warn("SNTP node startup stage was slow")
	} else {
		entry.Info("SNTP node startup stage completed")
	}
}

func nonNilError(err error) error {
	if err != nil {
		return err
	}
	return errors.New("panel returned no data")
}

// Startup cannot leave unknown authorization events unapplied and then advance the cursor.
func validateBootstrapUserDelta(delta *panel.UserDeltaData, since int64) error {
	if err := validateUserDelta(delta); err != nil {
		return err
	}
	seen := make(map[int64]bool, len(delta.Events))
	for _, event := range delta.Events {
		if event.Seq <= since || seen[event.Seq] {
			return errors.New("invalid bootstrap user event sequence")
		}
		seen[event.Seq] = true
		userID := event.UserID
		if userID <= 0 && len(event.Users) > 0 {
			userID = event.Users[0].Id
		}
		if userID <= 0 {
			return errors.New("missing bootstrap user event identity")
		}
		if event.Action != panel.UserDeltaActionUpsert && event.Action != panel.UserDeltaActionDelete {
			return errors.New("unknown bootstrap user event action")
		}
		for _, user := range event.Users {
			if user.Id != userID || user.Uuid == "" {
				return errors.New("invalid bootstrap user event rows")
			}
		}
	}
	return nil
}
