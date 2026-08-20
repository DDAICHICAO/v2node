package node

import (
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

func TestRouteRuntimeStateStoreRoundTrip(t *testing.T) {
	store := newRouteRuntimeStateStore(t.TempDir())
	state := testRouteRuntimeState()
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != state.Generation || got.VectorHash != state.VectorHash || len(got.Entries) != 2 {
		t.Fatalf("state=%+v", got)
	}
	info, err := os.Stat(store.path())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o, want 600", info.Mode().Perm())
	}
}

func TestRouteRuntimeStateInvalidSaveKeepsLastGoodSnapshot(t *testing.T) {
	store := newRouteRuntimeStateStore(t.TempDir())
	good := testRouteRuntimeState()
	if err := store.Save(good); err != nil {
		t.Fatal(err)
	}
	bad := testRouteRuntimeState()
	bad.Generation = 10
	bad.Entries[1].NodeInfo.Id = 99
	if err := store.Save(bad); err == nil {
		t.Fatal("invalid snapshot was accepted")
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != good.Generation {
		t.Fatalf("last good generation=%d, want %d", got.Generation, good.Generation)
	}
}

func TestRouteRuntimeStateRejectsConfiguredIdentityMismatch(t *testing.T) {
	state := testRouteRuntimeState()
	configs := []conf.NodeConfig{
		{APIHost: "https://panel.test", NodeID: 1},
		{APIHost: "https://different.test", NodeID: 2},
	}
	if _, err := routeRuntimeEntriesForConfigs(state, configs); err == nil {
		t.Fatal("identity mismatch was accepted")
	}
}

func TestRouteRuntimeStateOverlaysNodeInfoOnly(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.test", NodeID: 1}
	state := testOfflineState(cfg)
	state.Users = []panel.UserInfo{{Id: 7}}
	state.Alive = map[int]int{7: 1}
	state.UserSyncSeq = 88
	runtimeInfo := testOfflineNodeInfo(1)
	runtimeInfo.Common.Routes = []panel.Route{{Id: 99, Action: "block", Match: []string{"domain:example.com"}}}
	if !overlayRouteRuntimeState(state, routeRuntimeEntry{
		APIHost: "https://panel.test", NodeID: 1, NodeInfo: runtimeInfo,
	}) {
		t.Fatal("matching runtime state was not overlaid")
	}
	if state.NodeInfo.Common.Routes[0].Id != 99 || state.Users[0].Id != 7 || state.Alive[7] != 1 || state.UserSyncSeq != 88 {
		t.Fatalf("overlay changed wrong fields: %+v", state)
	}
}

func TestNodeNewRestoresWholeMachineNodeInfoWithoutReplacingUserState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "panel unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	statePath := t.TempDir()
	retryCount := 0
	cfg := conf.NodeConfig{APIHost: server.URL, NodeID: 1, Timeout: 1, RetryCount: &retryCount}
	offline := testOfflineState(cfg)
	offline.Users = []panel.UserInfo{{Id: 7, Uuid: "cached-user"}}
	offline.Alive = map[int]int{7: 1}
	offline.UserSyncSeq = 77
	if err := newOfflineStateStore(statePath).Save(cfg, offline); err != nil {
		t.Fatal(err)
	}
	runtimeInfo := testOfflineNodeInfo(1)
	runtimeInfo.Common.Routes = []panel.Route{{Id: 99, Action: "block", Match: []string{"domain:example.com"}}}
	if err := newRouteRuntimeStateStore(statePath).Save(&routeRuntimeState{
		Version:    routeRuntimeStateVersion,
		Generation: 5,
		SavedAt:    time.Now().Unix(),
		VectorHash: "vector-5",
		Entries: []routeRuntimeEntry{{
			APIHost: normalizeAPIHost(server.URL), NodeID: 1, ConfigVersion: "config-5", NodeInfo: runtimeInfo,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	nodes, err := New([]conf.NodeConfig{cfg}, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := nodes.NodeInfos[0].Common.Routes; len(got) != 1 || got[0].Id != 99 {
		t.Fatalf("restored routes=%+v", got)
	}
	bootstrap := nodes.controllers[0].bootstrap
	if len(bootstrap.Users) != 1 || bootstrap.Users[0].Id != 7 || bootstrap.Alive[7] != 1 || bootstrap.UserSyncSeq != 77 {
		t.Fatalf("user state was replaced: %+v", bootstrap)
	}
}

func testRouteRuntimeState() *routeRuntimeState {
	first := testOfflineNodeInfo(1)
	first.Common.Routes = []panel.Route{{Id: 1, Action: "block", Match: []string{"domain:first.test"}}}
	second := testOfflineNodeInfo(2)
	second.Tag = "[https://panel.test]-vless:2"
	second.Common.Routes = []panel.Route{{Id: 2, Action: "block_ip", Match: []string{"192.0.2.0/24"}}}
	return &routeRuntimeState{
		Version:    routeRuntimeStateVersion,
		Generation: 4,
		SavedAt:    time.Now().Unix(),
		VectorHash: "vector-4",
		Entries: []routeRuntimeEntry{
			{APIHost: "https://panel.test", NodeID: 1, ConfigVersion: "config-1", NodeInfo: first},
			{APIHost: "https://panel.test", NodeID: 2, ConfigVersion: "config-2", NodeInfo: second},
		},
	}
}
