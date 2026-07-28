package node

import (
	"context"
	"errors"
	"fmt"
	"time"

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

func loadBootstrapState(ctx context.Context, client bootstrapPanel, cfg conf.NodeConfig, cached *offlineState) (*offlineState, bool, error) {
	state := &offlineState{
		Version:      offlineStateVersion,
		APIHost:      normalizeAPIHost(cfg.APIHost),
		NodeID:       cfg.NodeID,
		SavedAt:      time.Now().Unix(),
		DeviceAlive:  map[int]int{},
		UUIDIPFanout: panel.UUIDIPFanoutGlobalState{States: []panel.UUIDIPFanoutGlobalDecision{}},
	}
	usedSnapshot := false

	info, err := client.GetNodeInfo(ctx)
	if err != nil || info == nil {
		if cached == nil {
			return nil, false, fmt.Errorf("get node info and no offline snapshot: %w", nonNilError(err))
		}
		info = cached.NodeInfo
		usedSnapshot = true
	}
	state.NodeInfo = info

	users, err := client.GetUserList(ctx)
	if err != nil || users == nil {
		if cached == nil || cached.Users == nil {
			return nil, false, fmt.Errorf("get user list and no offline snapshot: %w", nonNilError(err))
		}
		users = cached.Users
		client.SetUserSyncSeq(cached.UserSyncSeq)
		usedSnapshot = true
	}
	state.Users = append([]panel.UserInfo(nil), users...)
	if state.Users == nil {
		state.Users = []panel.UserInfo{}
	}

	alive, err := client.GetUserAlive(ctx)
	if err != nil || alive == nil {
		if cached == nil || cached.Alive == nil {
			return nil, false, fmt.Errorf("get alive state and no offline snapshot: %w", nonNilError(err))
		}
		alive = cached.Alive
		usedSnapshot = true
	}
	state.Alive = cloneIntMap(alive)

	if info.Common != nil && info.Common.BaseConfig != nil &&
		(info.Common.BaseConfig.DeviceLimitByUUID || info.Common.BaseConfig.DeviceAliveReport) {
		deviceState, err := client.GetUserDeviceAliveState(ctx)
		if err != nil || deviceState == nil {
			if cached == nil || cached.DeviceAlive == nil {
				return nil, false, fmt.Errorf("get device alive state and no offline snapshot: %w", nonNilError(err))
			}
			state.DeviceAlive = cloneIntMap(cached.DeviceAlive)
			state.UUIDIPFanout = cloneUUIDIPFanoutGlobalState(cached.UUIDIPFanout)
			usedSnapshot = true
		} else {
			state.DeviceAlive = cloneIntMap(deviceState.AliveDevices)
			state.UUIDIPFanout = cloneUUIDIPFanoutGlobalState(deviceState.UUIDIPFanout)
		}
	}
	state.UserSyncSeq = client.UserSyncSeq()
	if usedSnapshot {
		state.SavedAt = cached.SavedAt
	}
	return state, usedSnapshot, nil
}

func nonNilError(err error) error {
	if err != nil {
		return err
	}
	return errors.New("panel returned no data")
}
