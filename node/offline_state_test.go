package node

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

func testOfflineNodeInfo(nodeID int) *panel.NodeInfo {
	return &panel.NodeInfo{
		Id:           nodeID,
		Type:         "vless",
		Tag:          "[https://panel.example]-vless:1",
		PushInterval: 60 * time.Second,
		PullInterval: 60 * time.Second,
		Common: &panel.CommonNode{
			Protocol:   "vless",
			ServerPort: 443,
			BaseConfig: &panel.BaseConfig{},
		},
	}
}

func testOfflineState(cfg conf.NodeConfig) *offlineState {
	return &offlineState{
		Version:     offlineStateVersion,
		APIHost:     normalizeAPIHost(cfg.APIHost),
		NodeID:      cfg.NodeID,
		SavedAt:     1_700_000_000,
		NodeInfo:    testOfflineNodeInfo(cfg.NodeID),
		Users:       []panel.UserInfo{},
		Alive:       map[int]int{},
		DeviceAlive: map[int]int{},
		UserSyncSeq: 42,
	}
}

func TestOfflineStateStoreRoundTripAllowsEmptyUsers(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example/", NodeID: 1}
	store := newOfflineStateStore(t.TempDir())
	want := testOfflineState(cfg)
	if err := store.Save(cfg, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserSyncSeq != 42 || got.NodeInfo.Id != 1 || got.Users == nil || len(got.Users) != 0 {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
	info, err := os.Stat(store.pathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o, want 600", info.Mode().Perm())
	}
}

func TestOfflineStateStoreReplacesPreviousSnapshot(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	store := newOfflineStateStore(t.TempDir())
	first := testOfflineState(cfg)
	if err := store.Save(cfg, first); err != nil {
		t.Fatal(err)
	}
	second := testOfflineState(cfg)
	second.UserSyncSeq = 43
	if err := store.Save(cfg, second); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserSyncSeq != 43 {
		t.Fatalf("UserSyncSeq=%d, want 43", got.UserSyncSeq)
	}
}

func TestOfflineStateStoreRejectsDifferentNodeIdentity(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	store := newOfflineStateStore(t.TempDir())
	state := testOfflineState(cfg)
	state.NodeID = 2
	if err := store.Save(cfg, state); err == nil {
		t.Fatal("expected identity validation error")
	}
}

func TestOfflineStateStoreInvalidSaveKeepsLastGoodSnapshot(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	store := newOfflineStateStore(t.TempDir())
	good := testOfflineState(cfg)
	if err := store.Save(cfg, good); err != nil {
		t.Fatal(err)
	}
	bad := testOfflineState(cfg)
	bad.NodeInfo = nil
	if err := store.Save(cfg, bad); err == nil {
		t.Fatal("expected invalid snapshot error")
	}
	got, err := store.Load(cfg)
	if err != nil || got.UserSyncSeq != good.UserSyncSeq {
		t.Fatalf("last good snapshot was lost: state=%+v err=%v", got, err)
	}
}

func TestOfflineStateStoreRejectsCorruptFile(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	store := newOfflineStateStore(t.TempDir())
	if err := os.WriteFile(store.pathFor(cfg), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(cfg); err == nil || !strings.Contains(err.Error(), "decode offline state") {
		t.Fatalf("error=%v, want decode offline state", err)
	}
}

func TestOfflineStateStoreRejectsUnknownVersion(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	store := newOfflineStateStore(t.TempDir())
	if err := store.Save(cfg, testOfflineState(cfg)); err != nil {
		t.Fatal(err)
	}
	path := store.pathFor(cfg)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"version":1`, `"version":2`, 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(cfg); err == nil || !strings.Contains(err.Error(), "unsupported offline state version 2") {
		t.Fatalf("error=%v, want unsupported version", err)
	}
}
