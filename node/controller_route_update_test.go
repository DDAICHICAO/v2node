package node

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

func TestApplyPendingNodeInfoDefersRoutesOnlyToCoordinator(t *testing.T) {
	current := testOfflineNodeInfo(1)
	next, err := cloneRouteRuntimeNodeInfo(current)
	if err != nil {
		t.Fatal(err)
	}
	next.Common.Routes = []panel.Route{{Id: 2, Action: "block", Match: []string{"domain:new.test"}}}
	if kind := classifyNodeInfoChange(current, next); kind != nodeInfoRoutesOnly {
		t.Fatalf("change kind=%d, want routes-only", kind)
	}
	reloadCh := make(chan struct{}, 1)
	coordinator := newRouteUpdateCoordinator(nil, nil, nil, testRouteCoordinatorOptions())
	defer coordinator.Close()
	c := &Controller{
		server:                 &core.V2Core{ReloadCh: reloadCh},
		info:                   current,
		pendingNodeInfo:        next,
		pendingNodeInfoVersion: "v2",
		activeNodeInfoVersion:  "v1",
		routeCoordinator:       coordinator,
	}

	if err := c.applyPendingNodeInfo(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloadCh:
		t.Fatal("routes-only change queued a global reload")
	default:
	}
	if c.pendingNodeInfo == nil {
		t.Fatal("routes-only pending state was cleared before publication")
	}
	if got := coordinator.epoch.Load(); got != 1 {
		t.Fatalf("coordinator epoch=%d, want 1", got)
	}
	if err := c.applyPendingNodeInfo(); err != nil {
		t.Fatal(err)
	}
	if got := coordinator.epoch.Load(); got != 1 {
		t.Fatalf("same pending version notified coordinator repeatedly: epoch=%d", got)
	}
}

func TestRouteHotReloadDisabledKeepsGlobalReloadFallback(t *testing.T) {
	current := testOfflineNodeInfo(1)
	next, err := cloneRouteRuntimeNodeInfo(current)
	if err != nil {
		t.Fatal(err)
	}
	next.Common.Routes = []panel.Route{{Id: 2, Action: "block", Match: []string{"domain:new.test"}}}
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	reloadCh := make(chan struct{}, 1)
	v2core := &core.V2Core{
		Config:   &conf.Conf{EnableRouteHotReload: false},
		ReloadCh: reloadCh,
	}
	if routeHotReloadEnabled(v2core) {
		t.Fatal("disabled config would start the route coordinator")
	}
	c := &Controller{
		apiClient:       &panel.Client{},
		conf:            &cfg,
		store:           newOfflineStateStore(t.TempDir()),
		server:          v2core,
		info:            current,
		pendingNodeInfo: next,
		userList:        []panel.UserInfo{},
		aliveMap:        map[int]int{},
		deviceAliveMap:  map[int]int{},
	}

	if err := c.applyPendingNodeInfo(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloadCh:
	default:
		t.Fatal("disabled hot reload did not use the existing global reload path")
	}
}

func TestCommitRouteNodeInfoKeepsNewerPendingVersion(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	current := testOfflineNodeInfo(1)
	published, err := cloneRouteRuntimeNodeInfo(current)
	if err != nil {
		t.Fatal(err)
	}
	published.Common.Routes = []panel.Route{{Id: 2, Action: "block", Match: []string{"domain:v2.test"}}}
	newer, err := cloneRouteRuntimeNodeInfo(current)
	if err != nil {
		t.Fatal(err)
	}
	newer.Common.Routes = []panel.Route{{Id: 3, Action: "block", Match: []string{"domain:v3.test"}}}
	coordinator := newRouteUpdateCoordinator(nil, nil, nil, testRouteCoordinatorOptions())
	defer coordinator.Close()
	c := &Controller{
		apiClient:              &panel.Client{},
		conf:                   &cfg,
		store:                  newOfflineStateStore(t.TempDir()),
		info:                   current,
		pendingNodeInfo:        newer,
		pendingNodeInfoVersion: "v3",
		activeNodeInfoVersion:  "v1",
		routeCoordinator:       coordinator,
		userList:               []panel.UserInfo{},
		aliveMap:               map[int]int{},
		deviceAliveMap:         map[int]int{},
	}

	if err := c.CommitRouteNodeInfo(published, "v2"); err != nil {
		t.Fatal(err)
	}
	state := c.ObservedState()
	if state.Active.Common.Routes[0].Id != 2 || c.activeNodeInfoVersion != "v2" {
		t.Fatalf("published state was not activated: %+v", state.Active.Common.Routes)
	}
	if state.Observed.Common.Routes[0].Id != 3 || state.Version != "v3" || c.pendingNodeInfo == nil {
		t.Fatalf("newer pending state was lost: %+v", state)
	}
	if got := coordinator.epoch.Load(); got != 1 {
		t.Fatalf("newer pending state did not notify coordinator: epoch=%d", got)
	}
}

func TestCommitRouteNodeInfoDoesNotDiscardNewerEquivalentVersion(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	current := testOfflineNodeInfo(1)
	published, err := cloneRouteRuntimeNodeInfo(current)
	if err != nil {
		t.Fatal(err)
	}
	published.Common.Routes = []panel.Route{{Id: 2, Action: "block", Match: []string{"domain:v2.test"}}}
	newerEquivalent, err := cloneRouteRuntimeNodeInfo(published)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newRouteUpdateCoordinator(nil, nil, nil, testRouteCoordinatorOptions())
	defer coordinator.Close()
	c := &Controller{
		apiClient:              &panel.Client{},
		conf:                   &cfg,
		store:                  newOfflineStateStore(t.TempDir()),
		info:                   current,
		pendingNodeInfo:        newerEquivalent,
		pendingNodeInfoVersion: "v3",
		activeNodeInfoVersion:  "v1",
		routeCoordinator:       coordinator,
		userList:               []panel.UserInfo{},
		aliveMap:               map[int]int{},
		deviceAliveMap:         map[int]int{},
	}

	if err := c.CommitRouteNodeInfo(published, "v2"); err != nil {
		t.Fatal(err)
	}
	if c.pendingNodeInfo == nil || c.pendingNodeInfoVersion != "v3" {
		t.Fatal("newer response version was discarded because parsed content matched")
	}
	if got := coordinator.epoch.Load(); got != 1 {
		t.Fatalf("newer equivalent version did not notify coordinator: epoch=%d", got)
	}
}

func TestCommitRouteNodeInfoIsSafeWithConcurrentRuntimeReaders(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	current := testOfflineNodeInfo(1)
	c := &Controller{
		apiClient:             &panel.Client{},
		conf:                  &cfg,
		store:                 newOfflineStateStore(t.TempDir()),
		info:                  current,
		activeNodeInfoVersion: "v1",
		userList:              []panel.UserInfo{},
		aliveMap:              map[int]int{},
		deviceAliveMap:        map[int]int{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wait sync.WaitGroup
	var invalid atomic.Bool
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for ctx.Err() == nil {
				if c.currentNodeInfo() == nil || c.ObservedState().Active == nil {
					invalid.Store(true)
					return
				}
				_ = c.supportsDeviceLimitByUUID()
				_ = c.supportsDeviceAliveReport()
				_ = c.supportsDeviceTrafficReport()
				_ = c.aliveStateRefreshInterval()
			}
		}()
	}
	for version := 2; version <= 25; version++ {
		next, err := cloneRouteRuntimeNodeInfo(current)
		if err != nil {
			t.Fatal(err)
		}
		next.Common.Routes = []panel.Route{{Id: version, Action: "block", Match: []string{"domain:concurrent.test"}}}
		if err := c.CommitRouteNodeInfo(next, fmt.Sprintf("v%d", version)); err != nil {
			t.Fatal(err)
		}
		current = next
	}
	cancel()
	wait.Wait()
	if invalid.Load() {
		t.Fatal("runtime reader observed incomplete node info")
	}
}
