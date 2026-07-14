package node

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
)

func TestMain(m *testing.M) {
	if os.Getenv("V2NODE_TEST_VERSION_HELPER") == "1" && len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("v2node test")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestApplyUserDeltaEventsUpsertReplacesUserRows(t *testing.T) {
	oldUsers := []panel.UserInfo{
		{Id: 1, Uuid: "legacy-1", SpeedLimit: 10, DeviceLimit: 1},
		{Id: 1, Uuid: "old-device-1", SpeedLimit: 10, DeviceLimit: 1},
		{Id: 2, Uuid: "legacy-2", SpeedLimit: 20, DeviceLimit: 2},
	}
	events := []panel.UserDeltaEvent{
		{
			Seq:    5,
			Action: panel.UserDeltaActionUpsert,
			UserID: 1,
			Users: []panel.UserInfo{
				{Id: 1, Uuid: "legacy-1", SpeedLimit: 30, DeviceLimit: 3},
				{Id: 1, Uuid: "new-device-1", SpeedLimit: 30, DeviceLimit: 3},
			},
		},
	}

	got, changed := applyUserDeltaEvents(oldUsers, events)
	if !changed {
		t.Fatal("expected delta to change user list")
	}

	want := []panel.UserInfo{
		{Id: 1, Uuid: "legacy-1", SpeedLimit: 30, DeviceLimit: 3},
		{Id: 1, Uuid: "new-device-1", SpeedLimit: 30, DeviceLimit: 3},
		{Id: 2, Uuid: "legacy-2", SpeedLimit: 20, DeviceLimit: 2},
	}
	assertUserListEqual(t, got, want)
}

func TestApplyUserDeltaEventsDeleteRemovesAllRowsForUserID(t *testing.T) {
	oldUsers := []panel.UserInfo{
		{Id: 1, Uuid: "legacy-1"},
		{Id: 1, Uuid: "device-1"},
		{Id: 2, Uuid: "legacy-2"},
	}
	events := []panel.UserDeltaEvent{
		{Seq: 6, Action: panel.UserDeltaActionDelete, UserID: 1},
	}

	got, changed := applyUserDeltaEvents(oldUsers, events)
	if !changed {
		t.Fatal("expected delta to change user list")
	}

	want := []panel.UserInfo{{Id: 2, Uuid: "legacy-2"}}
	assertUserListEqual(t, got, want)
}

func TestApplyUserDeltaEventsSortsEventsBySeq(t *testing.T) {
	oldUsers := []panel.UserInfo{
		{Id: 1, Uuid: "legacy-1"},
	}
	events := []panel.UserDeltaEvent{
		{
			Seq:    2,
			Action: panel.UserDeltaActionUpsert,
			UserID: 1,
			Users: []panel.UserInfo{
				{Id: 1, Uuid: "new-device-1", SpeedLimit: 30},
			},
		},
		{Seq: 1, Action: panel.UserDeltaActionDelete, UserID: 1},
	}

	got, changed := applyUserDeltaEvents(oldUsers, events)
	if !changed {
		t.Fatal("expected sorted delta events to change user list")
	}

	want := []panel.UserInfo{
		{Id: 1, Uuid: "new-device-1", SpeedLimit: 30},
	}
	assertUserListEqual(t, got, want)
}

func TestApplyUserDeltaEventsNoEventsKeepsList(t *testing.T) {
	oldUsers := []panel.UserInfo{{Id: 1, Uuid: "legacy-1"}}

	got, changed := applyUserDeltaEvents(oldUsers, nil)
	if changed {
		t.Fatal("expected empty delta to keep user list unchanged")
	}
	assertUserListEqual(t, got, oldUsers)
}

func TestRemoveExpiredUsers(t *testing.T) {
	oldUsers := []panel.UserInfo{
		{Id: 1, Uuid: "expired", ExpiredAt: 100},
		{Id: 2, Uuid: "active", ExpiredAt: 200},
		{Id: 3, Uuid: "never-expire"},
	}

	got, changed := removeExpiredUsers(oldUsers, 100)
	if !changed {
		t.Fatal("expected expired user to be removed")
	}

	want := []panel.UserInfo{
		{Id: 2, Uuid: "active", ExpiredAt: 200},
		{Id: 3, Uuid: "never-expire"},
	}
	assertUserListEqual(t, got, want)
}

func TestUserDeltaPruneTimeRequiresPanelServerTime(t *testing.T) {
	if _, ok := userDeltaPruneTime(nil); ok {
		t.Fatal("expected nil delta to skip local prune")
	}
	if _, ok := userDeltaPruneTime(&panel.UserDeltaData{}); ok {
		t.Fatal("expected delta without panel server time to skip local prune")
	}

	got, ok := userDeltaPruneTime(&panel.UserDeltaData{ServerTime: 123})
	if !ok {
		t.Fatal("expected panel server time to enable prune")
	}
	if got != 123 {
		t.Fatalf("got prune time %d, want 123", got)
	}
}

func TestValidateUserDeltaRejectsLatestSeqBehindEvent(t *testing.T) {
	err := validateUserDelta(&panel.UserDeltaData{
		LatestSeq: 1,
		Events: []panel.UserDeltaEvent{
			{Seq: 2, Action: panel.UserDeltaActionDelete, UserID: 1},
		},
	})
	if err == nil {
		t.Fatal("expected latest_seq lower than event seq to be rejected")
	}
}

func TestApplyUserListReturnsAddUsersError(t *testing.T) {
	const tag = "apply-user-list-error"
	limiter.Init()
	l := limiter.AddLimiter("unsupported", tag, nil, nil, nil, false)
	c := &Controller{
		server:  core.New(nil),
		tag:     tag,
		limiter: l,
		info: &panel.NodeInfo{
			Type: "unsupported",
		},
	}

	err := c.applyUserList([]panel.UserInfo{{Id: 1, Uuid: "new-user"}})
	if err == nil {
		t.Fatal("expected add users error to be returned")
	}
	if len(c.userList) != 0 {
		t.Fatalf("expected local user list to stay unchanged after add failure, got %+v", c.userList)
	}
}

func TestSyncUserStateKeepsUsersWhenPanelUnavailable(t *testing.T) {
	t.Setenv("V2NODE_TEST_VERSION_HELPER", "1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "panel unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := conf.NodeConfig{APIHost: server.URL, NodeID: 1, Key: "test", MachineIP: "127.0.0.1"}
	client, err := panel.New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	original := []panel.UserInfo{{Id: 1, Uuid: "cached-user"}}
	c := &Controller{apiClient: client, userList: append([]panel.UserInfo(nil), original...)}

	if err := c.syncUserState(context.Background()); err == nil {
		t.Fatal("expected panel error")
	}
	assertUserListEqual(t, c.userList, original)
}

func TestRefreshAliveStateDoesNotApplyPartialPanelData(t *testing.T) {
	t.Setenv("V2NODE_TEST_VERSION_HELPER", "1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/server/UniProxy/alivelist":
			_, _ = w.Write([]byte(`{"alive":{"1":9}}`))
		case "/api/v1/server/UniProxy/deviceAliveList":
			http.Error(w, "panel unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := conf.NodeConfig{APIHost: server.URL, NodeID: 1, Key: "test", MachineIP: "127.0.0.1"}
	client, err := panel.New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{
		apiClient:      client,
		info:           testOfflineNodeInfo(1),
		aliveMap:       map[int]int{1: 1},
		deviceAliveMap: map[int]int{1: 2},
	}
	c.info.Common.BaseConfig.DeviceLimitByUUID = true

	if err := c.refreshAliveState(context.Background()); err == nil {
		t.Fatal("expected device alive panel error")
	}
	if c.aliveMap[1] != 1 || c.deviceAliveMap[1] != 2 {
		t.Fatalf("partial panel state was applied: alive=%v device=%v", c.aliveMap, c.deviceAliveMap)
	}
}

func TestNodeInfoMonitorPersistsNewConfigBeforeReload(t *testing.T) {
	t.Setenv("V2NODE_TEST_VERSION_HELPER", "1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol":"vless","server_port":8443,"base_config":{"push_interval":60,"pull_interval":60}}`))
	}))
	defer server.Close()

	cfg := conf.NodeConfig{APIHost: server.URL, NodeID: 1, Key: "test", MachineIP: "127.0.0.1"}
	client, err := panel.New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	notDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	reloadCh := make(chan struct{}, 1)
	c := &Controller{
		apiClient:      client,
		conf:           &cfg,
		store:          newOfflineStateStore(notDirectory),
		server:         &core.V2Core{ReloadCh: reloadCh},
		tag:            "test-node",
		info:           testOfflineNodeInfo(1),
		userList:       []panel.UserInfo{},
		aliveMap:       map[int]int{},
		deviceAliveMap: map[int]int{},
	}

	if err := c.nodeInfoMonitor(context.Background()); err == nil {
		t.Fatal("expected snapshot save failure")
	}
	if c.pendingNodeInfo == nil {
		t.Fatal("new node info was not retained for retry")
	}
	select {
	case <-reloadCh:
		t.Fatal("reload was signaled before snapshot save succeeded")
	default:
	}

	store := newOfflineStateStore(t.TempDir())
	c.store = store
	if err := c.nodeInfoMonitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloadCh:
	default:
		t.Fatal("reload was not signaled after snapshot save")
	}
	got, err := store.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeInfo.Common.ServerPort != 8443 {
		t.Fatalf("saved server port=%d, want 8443", got.NodeInfo.Common.ServerPort)
	}
}

func TestUpdateTaskIsDowngrade(t *testing.T) {
	cases := []struct {
		name    string
		current string
		target  string
		want    bool
	}{
		{name: "target older", current: "v5.0.0.24", target: "v5.0.0.23", want: true},
		{name: "target same", current: "v5.0.0.24", target: "v5.0.0.24", want: false},
		{name: "target newer", current: "v5.0.0.23", target: "v5.0.0.24", want: false},
		{name: "command output current", current: "v2node v5.0.0.24 (SNTP)", target: "v5.0.0.23", want: true},
		{name: "non numeric current", current: "dev-build", target: "v5.0.0.23", want: false},
		{name: "non numeric target", current: "v5.0.0.24", target: "dev-build", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := updateTaskIsDowngrade(tc.current, tc.target); got != tc.want {
				t.Fatalf("updateTaskIsDowngrade(%q, %q)=%v, want %v", tc.current, tc.target, got, tc.want)
			}
		})
	}
}

func TestUpdateTaskStatusIsAppliedIncludesSkipped(t *testing.T) {
	if !updateTaskStatusIsApplied(updateStatusSuccess) {
		t.Fatal("expected success status to be treated as applied")
	}
	if !updateTaskStatusIsApplied(updateStatusSkipped) {
		t.Fatal("expected skipped status to be treated as applied")
	}
	if updateTaskStatusIsApplied(updateStatusFailed) {
		t.Fatal("expected failed status to remain retryable")
	}
}

func assertUserListEqual(t *testing.T, got, want []panel.UserInfo) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, len(want)=%d; got=%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Id != want[i].Id ||
			got[i].Uuid != want[i].Uuid ||
			got[i].SpeedLimit != want[i].SpeedLimit ||
			got[i].DeviceLimit != want[i].DeviceLimit ||
			got[i].ExpiredAt != want[i].ExpiredAt {
			t.Fatalf("got[%d]=%+v, want=%+v", i, got[i], want[i])
		}
	}
}
