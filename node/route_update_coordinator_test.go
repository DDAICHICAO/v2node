package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestRouteUpdateCoordinatorCoalescesAllNodeIDs(t *testing.T) {
	targets := []*fakeRouteUpdateTarget{
		newFakeRouteTarget(1, "v1", "v2"),
		newFakeRouteTarget(2, "v1", "v2"),
	}
	applier := &recordingRouteApplier{}
	coordinator := newTestRouteUpdateCoordinator(targets, applier, &recordingRouteStateWriter{})
	coordinator.Start()
	defer coordinator.Close()
	coordinator.Notify()
	coordinator.Notify()
	waitForRouteApplyCount(t, applier, 1)
	if got := applier.LastNodeIDs(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("published node ids=%v", got)
	}
	for _, target := range targets {
		if target.ActiveVersion() != "v2" || target.HasPending() {
			t.Fatalf("target not committed: %+v", target)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := applier.Count(); got != 1 {
		t.Fatalf("coalesced change applied %d times", got)
	}
}

func TestRouteUpdateCoordinatorRestabilizesChangedVector(t *testing.T) {
	first := newFakeRouteTarget(1, "v1", "v2")
	first.enqueue(fakeRouteRefresh{version: "v3", info: routeCoordinatorInfo(1, 3)})
	second := newFakeRouteTarget(2, "v1", "v2")
	applier := &recordingRouteApplier{}
	coordinator := newTestRouteUpdateCoordinator([]*fakeRouteUpdateTarget{first, second}, applier, &recordingRouteStateWriter{})
	coordinator.Start()
	defer coordinator.Close()
	coordinator.Notify()
	waitForRouteApplyCount(t, applier, 1)
	if first.ActiveVersion() != "v3" {
		t.Fatalf("unstable intermediate version was committed: %q", first.ActiveVersion())
	}
	if got := applier.Count(); got != 1 {
		t.Fatalf("apply count=%d", got)
	}
}

func TestRouteUpdateCoordinatorRetriesRetainedPendingAfterApplyFailureAnd304(t *testing.T) {
	targets := []*fakeRouteUpdateTarget{
		newFakeRouteTarget(1, "v1", "v2"),
		newFakeRouteTarget(2, "v1", "v2"),
	}
	applier := &recordingRouteApplier{failures: 1}
	coordinator := newTestRouteUpdateCoordinator(targets, applier, &recordingRouteStateWriter{})
	coordinator.Start()
	defer coordinator.Close()
	coordinator.Notify()
	waitForRouteApplyCount(t, applier, 2)
	for _, target := range targets {
		if target.ActiveVersion() != "v2" || target.HasPending() {
			t.Fatalf("retained pending was not committed: %+v", target)
		}
	}
}

func TestRouteUpdateCoordinatorNewNotifyInterruptsBackoff(t *testing.T) {
	target := newFakeRouteTarget(1, "v1", "v2")
	target.replaceQueue([]fakeRouteRefresh{{err: errors.New("panel down")}})
	applier := &recordingRouteApplier{}
	options := testRouteCoordinatorOptions()
	options.InitialBackoff = time.Second
	options.MaxBackoff = time.Second
	coordinator := newRouteUpdateCoordinator(
		[]routeUpdateTarget{target}, applier, &recordingRouteStateWriter{}, options,
	)
	coordinator.Start()
	defer coordinator.Close()
	coordinator.Notify()
	select {
	case <-target.refreshFailed:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not enter refresh failure")
	}
	target.enqueue(fakeRouteRefresh{version: "v2", info: routeCoordinatorInfo(1, 2)})
	started := time.Now()
	coordinator.Notify()
	waitForRouteApplyCount(t, applier, 1)
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("Notify did not interrupt backoff: %v", elapsed)
	}
}

func TestRouteUpdateCoordinatorRetriesSnapshotWithoutRepublishing(t *testing.T) {
	target := newFakeRouteTarget(1, "v1", "v2")
	applier := &recordingRouteApplier{}
	writer := &recordingRouteStateWriter{failures: 1}
	coordinator := newTestRouteUpdateCoordinator([]*fakeRouteUpdateTarget{target}, applier, writer)
	coordinator.Start()
	defer coordinator.Close()
	coordinator.Notify()
	waitForRouteStateSaveCount(t, writer, 2)
	if got := applier.Count(); got != 1 {
		t.Fatalf("snapshot retry republished generation %d times", got)
	}
	if target.ActiveVersion() != "v2" || target.CommitCount() < 2 {
		t.Fatalf("persistence retry did not retain committed state: %+v", target)
	}
}

func TestRouteUpdateCoordinatorStatsCountCoalescedNotifications(t *testing.T) {
	coordinator := newTestRouteUpdateCoordinator(
		[]*fakeRouteUpdateTarget{newFakeRouteTarget(1, "v1", "v2")},
		&recordingRouteApplier{},
		&recordingRouteStateWriter{},
	)
	coordinator.Notify()
	coordinator.Notify()
	coordinator.Notify()
	stats := coordinator.Stats()
	if stats.Triggered != 3 || stats.Coalesced != 2 {
		t.Fatalf("stats=%+v", stats)
	}
	coordinator.Close()
}

func TestRouteUpdateCoordinatorStatsTrackFailureAndSuccess(t *testing.T) {
	applier := &recordingRouteApplier{failures: 1}
	coordinator := newTestRouteUpdateCoordinator(
		[]*fakeRouteUpdateTarget{newFakeRouteTarget(1, "v1", "v2")},
		applier,
		&recordingRouteStateWriter{},
	)
	coordinator.Start()
	defer coordinator.Close()
	coordinator.Notify()
	waitForRouteApplyCount(t, applier, 2)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stats := coordinator.Stats()
		if stats.Attempts >= 2 && stats.Failures >= 1 && stats.Successes == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stats=%+v", coordinator.Stats())
}

func newTestRouteUpdateCoordinator[T interface{ routeUpdateTarget }](targets []T, applier routeRuntimeApplier, writer routeRuntimeStateWriter) *routeUpdateCoordinator {
	converted := make([]routeUpdateTarget, len(targets))
	for index := range targets {
		converted[index] = targets[index]
	}
	return newRouteUpdateCoordinator(converted, applier, writer, testRouteCoordinatorOptions())
}

func testRouteCoordinatorOptions() routeUpdateCoordinatorOptions {
	return routeUpdateCoordinatorOptions{
		StabilizationWindow: 2 * time.Millisecond,
		SweepInterval:       time.Millisecond,
		InitialBackoff:      2 * time.Millisecond,
		MaxBackoff:          10 * time.Millisecond,
		MaxConcurrency:      4,
		Jitter:              func(delay time.Duration) time.Duration { return delay },
	}
}

type fakeRouteRefresh struct {
	version string
	info    *panel.NodeInfo
	err     error
}

type fakeRouteUpdateTarget struct {
	mu            sync.Mutex
	nodeID        int
	apiHost       string
	active        *panel.NodeInfo
	activeVersion string
	observed      *panel.NodeInfo
	version       string
	pending       bool
	queue         []fakeRouteRefresh
	commits       int
	refreshFailed chan struct{}
	failureOnce   sync.Once
}

func newFakeRouteTarget(nodeID int, activeVersion string, nextVersion string) *fakeRouteUpdateTarget {
	active := routeCoordinatorInfo(nodeID, 1)
	return &fakeRouteUpdateTarget{
		nodeID:        nodeID,
		apiHost:       "https://panel.test",
		active:        active,
		activeVersion: activeVersion,
		observed:      active,
		version:       activeVersion,
		queue: []fakeRouteRefresh{{
			version: nextVersion,
			info:    routeCoordinatorInfo(nodeID, 2),
		}},
		refreshFailed: make(chan struct{}),
	}
}

func (t *fakeRouteUpdateTarget) enqueue(refresh fakeRouteRefresh) {
	t.mu.Lock()
	t.queue = append(t.queue, refresh)
	t.mu.Unlock()
}

func (t *fakeRouteUpdateTarget) replaceQueue(queue []fakeRouteRefresh) {
	t.mu.Lock()
	t.queue = append([]fakeRouteRefresh(nil), queue...)
	t.mu.Unlock()
}

func (t *fakeRouteUpdateTarget) RefreshObserved(context.Context) (routeObservedState, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queue) > 0 {
		refresh := t.queue[0]
		t.queue = t.queue[1:]
		if refresh.err != nil {
			t.failureOnce.Do(func() { close(t.refreshFailed) })
			return routeObservedState{}, refresh.err
		}
		if refresh.info != nil {
			t.observed = refresh.info
			t.version = refresh.version
			t.pending = true
		}
	}
	return t.stateLocked(), nil
}

func (t *fakeRouteUpdateTarget) ObservedState() routeObservedState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stateLocked()
}

func (t *fakeRouteUpdateTarget) stateLocked() routeObservedState {
	return routeObservedState{
		NodeID: t.nodeID, APIHost: t.apiHost, Version: t.version,
		Active: t.active, Observed: t.observed,
	}
}

func (t *fakeRouteUpdateTarget) CommitRouteNodeInfo(info *panel.NodeInfo, version string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active = info
	t.activeVersion = version
	t.commits++
	if t.version == version {
		t.pending = false
	}
	return nil
}

func (t *fakeRouteUpdateTarget) ActiveVersion() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.activeVersion
}

func (t *fakeRouteUpdateTarget) HasPending() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pending
}

func (t *fakeRouteUpdateTarget) CommitCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.commits
}

func routeCoordinatorInfo(nodeID int, routeID int) *panel.NodeInfo {
	return &panel.NodeInfo{
		Id:   nodeID,
		Type: "vless",
		Tag:  fmt.Sprintf("node-%d", nodeID),
		Common: &panel.CommonNode{
			Protocol:   "vless",
			ServerPort: 443,
			BaseConfig: &panel.BaseConfig{},
			Routes: []panel.Route{{
				Id: routeID, Action: "block", Match: []string{fmt.Sprintf("domain:route-%d.test", routeID)},
			}},
		},
	}
}

type recordingRouteApplier struct {
	mu       sync.Mutex
	calls    [][]*panel.NodeInfo
	failures int
}

func (a *recordingRouteApplier) ApplyRouteRuntime(infos []*panel.NodeInfo) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, append([]*panel.NodeInfo(nil), infos...))
	if a.failures > 0 {
		a.failures--
		return uint64(len(a.calls) - 1), errors.New("candidate failed")
	}
	return uint64(len(a.calls)), nil
}

func (a *recordingRouteApplier) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *recordingRouteApplier) LastNodeIDs() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return nil
	}
	infos := a.calls[len(a.calls)-1]
	ids := make([]int, len(infos))
	for index, info := range infos {
		ids[index] = info.Id
	}
	return ids
}

type recordingRouteStateWriter struct {
	mu       sync.Mutex
	calls    int
	failures int
}

func (w *recordingRouteStateWriter) Save(*routeRuntimeState) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.failures > 0 {
		w.failures--
		return errors.New("disk full")
	}
	return nil
}

func (w *recordingRouteStateWriter) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func waitForRouteApplyCount(t *testing.T, applier *recordingRouteApplier, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if applier.Count() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("apply count=%d, want at least %d", applier.Count(), want)
}

func waitForRouteStateSaveCount(t *testing.T, writer *recordingRouteStateWriter, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if writer.Count() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("save count=%d, want at least %d", writer.Count(), want)
}
