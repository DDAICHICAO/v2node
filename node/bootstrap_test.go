package node

import (
	"context"
	"errors"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

type fakeBootstrapPanel struct {
	nodeInfo    *panel.NodeInfo
	users       []panel.UserInfo
	alive       map[int]int
	deviceAlive map[int]int
	fanout      panel.UUIDIPFanoutGlobalState
	err         error
	seq         int64
}

func (f *fakeBootstrapPanel) GetNodeInfo(context.Context) (*panel.NodeInfo, error) {
	return f.nodeInfo, f.err
}

func (f *fakeBootstrapPanel) GetUserList(context.Context) ([]panel.UserInfo, error) {
	return f.users, f.err
}

func (f *fakeBootstrapPanel) GetUserAlive(context.Context) (map[int]int, error) {
	return f.alive, f.err
}

func (f *fakeBootstrapPanel) GetUserDeviceAlive(context.Context) (map[int]int, error) {
	return f.deviceAlive, f.err
}

func (f *fakeBootstrapPanel) GetUserDeviceAliveState(context.Context) (*panel.DeviceAliveMap, error) {
	return &panel.DeviceAliveMap{AliveDevices: f.deviceAlive, UUIDIPFanout: f.fanout}, f.err
}

func (f *fakeBootstrapPanel) SetUserSyncSeq(seq int64) {
	f.seq = seq
}

func (f *fakeBootstrapPanel) UserSyncSeq() int64 {
	return f.seq
}

func TestLoadBootstrapStateUsesSnapshotWhenPanelUnavailable(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	cached := testOfflineState(cfg)
	cached.Users = []panel.UserInfo{{Id: 7, Uuid: "cached-user", SpeedLimit: 10}}
	cached.Alive = map[int]int{7: 1}
	client := &fakeBootstrapPanel{err: errors.New("panel unavailable")}

	got, usedSnapshot, err := loadBootstrapState(context.Background(), client, cfg, cached)
	if err != nil {
		t.Fatal(err)
	}
	if !usedSnapshot || len(got.Users) != 1 || got.Users[0].Uuid != "cached-user" || client.seq != cached.UserSyncSeq {
		t.Fatalf("unexpected bootstrap state: used=%v state=%+v seq=%d", usedSnapshot, got, client.seq)
	}

	cached.Users[0].Uuid = "mutated"
	cached.Alive[7] = 9
	if got.Users[0].Uuid != "cached-user" || got.Alive[7] != 1 {
		t.Fatalf("bootstrap state shares mutable snapshot data: %+v", got)
	}
}

func TestLoadBootstrapStateFailsWithoutPanelOrSnapshot(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	client := &fakeBootstrapPanel{err: errors.New("panel unavailable")}
	if _, _, err := loadBootstrapState(context.Background(), client, cfg, nil); err == nil {
		t.Fatal("expected missing offline snapshot error")
	}
}

func TestLoadBootstrapStatePrefersCompleteOnlineState(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	cached := testOfflineState(cfg)
	cached.Users = []panel.UserInfo{{Id: 7, Uuid: "cached-user"}}
	client := &fakeBootstrapPanel{
		nodeInfo: testOfflineNodeInfo(1),
		users:    []panel.UserInfo{},
		alive:    map[int]int{},
		seq:      99,
	}

	got, usedSnapshot, err := loadBootstrapState(context.Background(), client, cfg, cached)
	if err != nil {
		t.Fatal(err)
	}
	if usedSnapshot || got.Users == nil || len(got.Users) != 0 || got.UserSyncSeq != 99 {
		t.Fatalf("unexpected online bootstrap state: used=%v state=%+v", usedSnapshot, got)
	}
}
