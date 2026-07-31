package node

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "version" {
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

func TestCompareUserListDetectsFanoutExemptChange(t *testing.T) {
	oldUsers := []panel.UserInfo{{Id: 7, Uuid: "device-a", FanoutExempt: false}}
	newUsers := []panel.UserInfo{{Id: 7, Uuid: "device-a", FanoutExempt: true}}

	deleted, added, modified := compareUserList(oldUsers, newUsers)
	if len(deleted) != 0 || len(added) != 0 || len(modified) != 1 {
		t.Fatalf("deleted=%+v added=%+v modified=%+v", deleted, added, modified)
	}
	if !modified[0].FanoutExempt {
		t.Fatalf("modified user did not carry fanout exemption: %+v", modified[0])
	}
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

func TestCollectUserDeltaPagesKeepsPageBoundary(t *testing.T) {
	calls := make([]int64, 0, 2)
	pages := map[int64]*panel.UserDeltaData{
		10: {
			LatestSeq: 12,
			HasMore:   true,
			Events: []panel.UserDeltaEvent{
				{Seq: 11, UserID: 101, Action: panel.UserDeltaActionDelete},
				{Seq: 12, UserID: 102, Action: panel.UserDeltaActionDelete},
			},
		},
		12: {
			LatestSeq: 14,
			Events: []panel.UserDeltaEvent{
				{Seq: 13, UserID: 103, Action: panel.UserDeltaActionDelete},
				{Seq: 14, UserID: 104, Action: panel.UserDeltaActionDelete},
			},
		},
	}

	result, err := collectUserDeltaPages(
		context.Background(),
		10,
		10,
		func(_ context.Context, since int64) (*panel.UserDeltaData, error) {
			calls = append(calls, since)
			return pages[since], nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []int64{10, 12}) {
		t.Fatalf("calls=%v", calls)
	}
	if result.LatestSeq != 14 || len(result.Events) != 4 {
		t.Fatalf("result=%+v", result)
	}
}

func TestCollectUserDeltaPagesReturnsDurableProgressAtLimit(t *testing.T) {
	result, err := collectUserDeltaPages(
		context.Background(),
		10,
		2,
		func(_ context.Context, since int64) (*panel.UserDeltaData, error) {
			return &panel.UserDeltaData{
				LatestSeq: since + 1,
				HasMore:   true,
				Events: []panel.UserDeltaEvent{{
					Seq:    since + 1,
					Action: panel.UserDeltaActionDelete,
					UserID: int(since + 1),
				}},
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.LatestSeq != 12 || len(result.Events) != 2 || !result.HasMore {
		t.Fatalf("progress was discarded at page limit: %+v", result)
	}
}

func TestCommitUserStatePersistFailureRestoresPreviousState(t *testing.T) {
	previous := []panel.UserInfo{{Id: 1, Uuid: "old"}}
	next := []panel.UserInfo{{Id: 1, Uuid: "new"}}
	current := append([]panel.UserInfo(nil), previous...)
	seq := int64(10)
	apply := func(users []panel.UserInfo) error {
		current = append([]panel.UserInfo(nil), users...)
		return nil
	}
	persist := func([]panel.UserInfo, int64) error {
		return errors.New("disk full")
	}
	setSeq := func(nextSeq int64) {
		seq = nextSeq
	}

	err := commitUserStateWith(previous, next, 11, apply, persist, setSeq)
	if err == nil {
		t.Fatal("snapshot failure accepted")
	}
	assertUserListEqual(t, current, previous)
	if seq != 10 {
		t.Fatalf("sequence advanced to %d", seq)
	}
}

func TestUserSyncRuntimeMergesWakeupsAndAcknowledgesHighestRevision(t *testing.T) {
	syncCalls := 0
	var applied []panel.UserSyncAppliedMessage
	runtime := &userSyncRuntime{
		syncFn: func(context.Context) (int64, error) {
			syncCalls++
			return 99, nil
		},
		ackFn: func(_ context.Context, message panel.UserSyncAppliedMessage) error {
			applied = append(applied, message)
			return nil
		},
	}
	runtime.enqueueRevision(40)
	runtime.enqueueRevision(42)
	if err := runtime.flushPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if syncCalls != 1 {
		t.Fatalf("sync calls=%d, want 1", syncCalls)
	}
	if len(applied) != 1 || applied[0].Revision != 42 || applied[0].SyncSeq != 99 {
		t.Fatalf("acks=%v", applied)
	}
}

func TestUserSyncRuntimeDoesNotAckFailedSync(t *testing.T) {
	acked := false
	runtime := &userSyncRuntime{
		syncFn: func(context.Context) (int64, error) {
			return 0, errors.New("panel down")
		},
		ackFn: func(context.Context, panel.UserSyncAppliedMessage) error {
			acked = true
			return nil
		},
	}
	runtime.enqueueRevision(42)
	if err := runtime.flushPending(context.Background()); err == nil {
		t.Fatal("failed sync accepted")
	}
	if acked {
		t.Fatal("failed sync was acknowledged")
	}
}

func TestUserSyncRuntimeFallsBackWithStableJitter(t *testing.T) {
	first := stableUserSyncJitter("instance-1")
	second := stableUserSyncJitter("instance-1")
	if first != second || first < 0 || first >= 500*time.Millisecond {
		t.Fatalf("unstable jitter: %v %v", first, second)
	}
}

func TestUserSyncRuntimeCatchesUpBeforePushMode(t *testing.T) {
	runtime := &userSyncRuntime{
		mode: userSyncModeFallback,
		syncFn: func(context.Context) (int64, error) {
			return 0, errors.New("catch-up failed")
		},
	}
	if err := runtime.activatePush(context.Background()); err == nil {
		t.Fatal("failed catch-up accepted")
	}
	if runtime.mode == userSyncModePush {
		t.Fatal("push mode activated before catch-up")
	}
}

func TestUserSyncRuntimeLegacyPanelUsesPullInterval(t *testing.T) {
	runtime := &userSyncRuntime{legacyInterval: 60 * time.Second}
	if got := runtime.pollInterval(nil); got != 60*time.Second {
		t.Fatalf("legacy interval=%v", got)
	}
}

func TestNodeInfoMonitorDoesNotSynchronizeUsers(t *testing.T) {
	source, err := os.ReadFile("task.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), "func (c *Controller) nodeInfoMonitor")
	if start < 0 {
		t.Fatal("nodeInfoMonitor start not found")
	}
	end := strings.Index(string(source)[start:], "func (c *Controller) queueReload")
	if end < 0 {
		t.Fatal("nodeInfoMonitor end not found")
	}
	body := string(source)[start : start+end]
	if strings.Contains(body, "syncUserState(") {
		t.Fatal("config task still synchronizes users")
	}
}

func TestAliveStateRefreshRunsIndependentlyFromUserDelta(t *testing.T) {
	source, err := os.ReadFile("task.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if !strings.Contains(text, `Name:            "refreshAliveStateTask"`) ||
		!strings.Contains(text, "Execute:         c.refreshAliveStateTask") {
		t.Fatal("independent alive task missing")
	}
}

func TestNextUserExpiryChoosesEarliestFutureTimestamp(t *testing.T) {
	users := []panel.UserInfo{
		{Id: 1, ExpiredAt: 200},
		{Id: 2, ExpiredAt: 150},
		{Id: 3, ExpiredAt: 0},
	}
	got, ok := nextUserExpiry(users, 100)
	if !ok || got != 150 {
		t.Fatalf("got=%d ok=%v", got, ok)
	}
}

func TestLocalExpiryRemovesAllRowsForExpiredUser(t *testing.T) {
	users := []panel.UserInfo{
		{Id: 1, Uuid: "device-a", ExpiredAt: 100},
		{Id: 1, Uuid: "device-b", ExpiredAt: 100},
		{Id: 2, Uuid: "active", ExpiredAt: 200},
	}
	got, changed := removeExpiredUsers(users, 101)
	if !changed || len(got) != 1 || got[0].Id != 2 {
		t.Fatalf("expired rows retained: changed=%v users=%v", changed, got)
	}
}

func TestFullSyncBreakerOpensAfterThreeFailuresAndResets(t *testing.T) {
	var breaker fullSyncBreaker
	now := time.Unix(100, 0)
	breaker.Failure(now)
	breaker.Failure(now)
	if breaker.OpenUntil(now).After(now) {
		t.Fatal("breaker opened before third failure")
	}
	breaker.Failure(now)
	if got := breaker.OpenUntil(now); !got.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("open until=%v", got)
	}
	breaker.Success()
	if breaker.OpenUntil(now).After(now) {
		t.Fatal("breaker did not reset")
	}
}

func TestUserSyncRuntimeAddsStableJitterToRetryAfter(t *testing.T) {
	runtime := &userSyncRuntime{instanceID: "instance-1"}
	got, ok := runtime.retryDelay(&panel.UserSyncRetryError{
		StatusCode: http.StatusServiceUnavailable,
		After:      1500 * time.Millisecond,
	})
	want := 1500*time.Millisecond + stableUserSyncJitter("instance-1")
	if !ok || got != want {
		t.Fatalf("delay=%v ok=%v want=%v", got, ok, want)
	}
}

func TestUserSyncRuntimeThrottlesWakeupWarnings(t *testing.T) {
	now := time.Unix(100, 0)
	runtime := &userSyncRuntime{nowFn: func() time.Time { return now }}
	if !runtime.shouldWarnWakeupUnavailable() {
		t.Fatal("first warning was suppressed")
	}
	if runtime.shouldWarnWakeupUnavailable() {
		t.Fatal("repeated warning was not throttled")
	}
	now = now.Add(61 * time.Second)
	if !runtime.shouldWarnWakeupUnavailable() {
		t.Fatal("warning did not reopen after throttle window")
	}
}

func TestSyncUserStateBreakerStopsRepeatedForcedFullRequests(t *testing.T) {
	t.Setenv("V2NODE_TEST_VERSION_HELPER", "1")
	fullRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/server/user-delta":
			_, _ = w.Write([]byte(`{"data":{"full_required":true,"latest_seq":0}}`))
		case "/api/v1/server/UniProxy/user":
			fullRequests++
			w.Header().Set("X-User-Sync-Retry-After-Ms", "500")
			http.Error(w, "snapshot busy", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := conf.NodeConfig{
		APIHost: server.URL,
		NodeID:  1,
		Key:     "test",
	}
	client, err := panel.New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{
		apiClient: client,
		userList:  []panel.UserInfo{{Id: 1, Uuid: "cached"}},
	}
	for i := 0; i < 4; i++ {
		if _, err := controller.syncUserState(context.Background()); err == nil {
			t.Fatalf("sync %d unexpectedly succeeded", i+1)
		}
	}
	if fullRequests != 3 {
		t.Fatalf("forced full requests=%d, want 3", fullRequests)
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

	if _, err := c.syncUserState(context.Background()); err == nil {
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
