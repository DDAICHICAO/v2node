package node

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

func TestRouteHotReloadKeepsExistingAndNewConnectionsOnline(t *testing.T) {
	t.Setenv("V2NODE_TEST_VERSION_HELPER", "1")
	baselineGoroutines := runtime.NumGoroutine()

	var panelVersion atomic.Int64
	panelVersion.Store(1)
	var panelRequests atomic.Int64
	panelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/server/config" {
			http.NotFound(w, r)
			return
		}
		nodeID, err := strconv.Atoi(r.URL.Query().Get("node_id"))
		if err != nil || nodeID <= 0 {
			http.Error(w, "invalid node id", http.StatusBadRequest)
			return
		}
		panelRequests.Add(1)
		version := panelVersion.Load()
		etag := fmt.Sprintf(`"node-%d-v%d"`, nodeID, version)
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"protocol":"vless","server_port":%d,"base_config":{"push_interval":60,"pull_interval":60},"routes":[{"id":%d,"action":"block","match":["domain:node-%d-v%d.test"]}]}`,
			12000+nodeID, version*100+int64(nodeID), nodeID, version)
	}))

	statePath := t.TempDir()
	configs := []conf.NodeConfig{
		{APIHost: panelServer.URL, NodeID: 1, Key: "test", MachineIP: "127.0.0.1"},
		{APIHost: panelServer.URL, NodeID: 2, Key: "test", MachineIP: "127.0.0.1"},
	}
	controllers := make([]*Controller, 0, len(configs))
	initialInfos := make([]*panel.NodeInfo, 0, len(configs))
	for index := range configs {
		client, err := panel.New(&configs[index])
		if err != nil {
			t.Fatal(err)
		}
		info, err := client.GetNodeInfo(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info == nil {
			t.Fatal("initial panel configuration was not returned")
		}
		controller := &Controller{
			apiClient:             client,
			conf:                  &configs[index],
			store:                 newOfflineStateStore(statePath),
			info:                  info,
			activeNodeInfoVersion: client.NodeInfoVersion(),
			userList:              []panel.UserInfo{},
			aliveMap:              map[int]int{},
			deviceAliveMap:        map[int]int{},
		}
		controllers = append(controllers, controller)
		initialInfos = append(initialInfos, info)
	}

	config := conf.New()
	config.LogConfig.Level = "warning"
	v2core := core.New(config)
	reloadCh := make(chan struct{}, 1)
	v2core.ReloadCh = reloadCh
	if err := v2core.Start(initialInfos); err != nil {
		t.Fatal(err)
	}
	serverBefore := v2core.Server
	applier := &continuityRouteApplier{core: v2core}
	targets := make([]routeUpdateTarget, len(controllers))
	for index, controller := range controllers {
		controller.server = v2core
		targets[index] = controller
	}
	options := testRouteCoordinatorOptions()
	options.StabilizationWindow = time.Millisecond
	options.SweepInterval = time.Millisecond
	coordinator := newRouteUpdateCoordinator(
		targets,
		applier,
		newRouteRuntimeStateStore(statePath),
		options,
	)
	for _, controller := range controllers {
		controller.routeCoordinator = coordinator
	}
	coordinator.Start()

	tcpAddress, udpAddress, closeEcho := startRouteContinuityEchoServers(t)
	tcpConn, err := net.DialTimeout("tcp", tcpAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	udpConn, err := net.DialTimeout("udp", udpAddress, time.Second)
	if err != nil {
		_ = tcpConn.Close()
		t.Fatal(err)
	}
	heartbeatCtx, stopHeartbeats := context.WithCancel(context.Background())
	heartbeatErrors := make(chan error, 2)
	var tcpHeartbeats atomic.Int64
	var udpHeartbeats atomic.Int64
	var heartbeatWait sync.WaitGroup
	heartbeatWait.Add(2)
	go routeContinuityHeartbeat(heartbeatCtx, tcpConn, "tcp", &tcpHeartbeats, heartbeatErrors, &heartbeatWait)
	go routeContinuityHeartbeat(heartbeatCtx, udpConn, "udp", &udpHeartbeats, heartbeatErrors, &heartbeatWait)
	waitForRouteHeartbeat(t, &tcpHeartbeats, 2)
	waitForRouteHeartbeat(t, &udpHeartbeats, 2)

	cleaned := false
	cleanup := func() {
		if cleaned {
			return
		}
		cleaned = true
		stopHeartbeats()
		_ = tcpConn.Close()
		_ = udpConn.Close()
		heartbeatWait.Wait()
		coordinator.Close()
		_ = v2core.Close()
		closeEcho()
		panelServer.Close()
	}
	defer cleanup()

	panelVersion.Store(2)
	coordinator.Notify()
	waitForContinuityApplyCount(t, applier, 1)
	waitForRouteControllerVersion(t, controllers, 2)
	if got := v2core.RouteRuntimeStats().ActiveGeneration; got != 1 {
		t.Fatalf("first multi-node update generation=%d, want 1", got)
	}
	if v2core.Server != serverBefore {
		t.Fatal("route update replaced the Xray Core instance")
	}
	assertNoRouteReload(t, reloadCh)
	for _, controller := range controllers {
		state := controller.ObservedState()
		if state.Active == nil || len(state.Active.Common.Routes) != 1 || state.Active.Common.Routes[0].Id/100 != 2 {
			t.Fatalf("node %d did not activate version 2: %+v", state.NodeID, state.Active)
		}
	}

	newConnectionErrors := make(chan error, 1)
	go func() {
		for index := 0; index < 250; index++ {
			conn, err := net.DialTimeout("tcp", tcpAddress, time.Second)
			if err != nil {
				newConnectionErrors <- err
				return
			}
			payload := []byte(fmt.Sprintf("new-%d", index))
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if _, err = conn.Write(payload); err == nil {
				buffer := make([]byte, len(payload))
				_, err = io.ReadFull(conn, buffer)
				if err == nil && string(buffer) != string(payload) {
					err = fmt.Errorf("unexpected echo payload %q", buffer)
				}
			}
			_ = conn.Close()
			if err != nil {
				newConnectionErrors <- err
				return
			}
		}
		newConnectionErrors <- nil
	}()

	const repeatedSwitches = 100
	for version := int64(3); version < 3+repeatedSwitches; version++ {
		panelVersion.Store(version)
		coordinator.Notify()
		waitForContinuityApplyCount(t, applier, int(version-1))
		waitForRouteControllerVersion(t, controllers, version)
		assertNoRouteReload(t, reloadCh)
		if v2core.Server != serverBefore {
			t.Fatal("stress update replaced the Xray Core instance")
		}
	}
	if err := <-newConnectionErrors; err != nil {
		t.Fatalf("new connection failed during route publishing: %v", err)
	}
	waitForRouteHeartbeat(t, &tcpHeartbeats, 10)
	waitForRouteHeartbeat(t, &udpHeartbeats, 10)
	select {
	case err := <-heartbeatErrors:
		t.Fatalf("existing connection failed during route publishing: %v", err)
	default:
	}
	if got := applier.Count(); got != repeatedSwitches+1 {
		t.Fatalf("apply count=%d, want %d", got, repeatedSwitches+1)
	}
	if got := v2core.RouteRuntimeStats(); got.ActiveGeneration != repeatedSwitches+1 || got.Retired != 0 || got.ActiveLeases != 0 {
		t.Fatalf("route runtime did not return to baseline: %+v", got)
	}
	if stats := coordinator.Stats(); stats.Successes != repeatedSwitches+1 || stats.Failures != 0 {
		t.Fatalf("coordinator stats=%+v", stats)
	}
	if panelRequests.Load() < int64((repeatedSwitches+1)*len(controllers)*2) {
		t.Fatalf("panel config refreshes stopped unexpectedly: %d", panelRequests.Load())
	}

	cleanup()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baselineGoroutines+24 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines did not return near baseline: before=%d after=%d", baselineGoroutines, runtime.NumGoroutine())
}

type continuityRouteApplier struct {
	mu          sync.Mutex
	core        *core.V2Core
	generations []uint64
}

func (a *continuityRouteApplier) ApplyRouteRuntime(infos []*panel.NodeInfo) (uint64, error) {
	generation, err := a.core.ApplyRouteRuntime(infos)
	if err != nil {
		return generation, err
	}
	a.mu.Lock()
	a.generations = append(a.generations, generation)
	a.mu.Unlock()
	return generation, nil
}

func (a *continuityRouteApplier) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.generations)
}

func waitForContinuityApplyCount(t *testing.T, applier *continuityRouteApplier, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if applier.Count() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("apply count=%d, want %d", applier.Count(), want)
}

func waitForRouteControllerVersion(t *testing.T, controllers []*Controller, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		matched := 0
		for _, controller := range controllers {
			state := controller.ObservedState()
			if state.Active != nil && state.Active.Common != nil && len(state.Active.Common.Routes) == 1 &&
				int64(state.Active.Common.Routes[0].Id)/100 == want && !controller.hasPendingNodeInfo() {
				matched++
			}
		}
		if matched == len(controllers) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("controllers did not commit route version %d", want)
}

func assertNoRouteReload(t *testing.T, reloadCh <-chan struct{}) {
	t.Helper()
	select {
	case <-reloadCh:
		t.Fatal("pure route update queued a global reload")
	default:
	}
}

func routeContinuityHeartbeat(
	ctx context.Context,
	conn net.Conn,
	label string,
	count *atomic.Int64,
	errors chan<- error,
	wait *sync.WaitGroup,
) {
	defer wait.Done()
	sequence := int64(0)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		sequence++
		payload := []byte(fmt.Sprintf("%s-%d", label, sequence))
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write(payload); err != nil {
			if ctx.Err() == nil {
				errors <- fmt.Errorf("%s write: %w", label, err)
			}
			return
		}
		buffer := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, buffer); err != nil {
			if ctx.Err() == nil {
				errors <- fmt.Errorf("%s read: %w", label, err)
			}
			return
		}
		if string(buffer) != string(payload) {
			errors <- fmt.Errorf("%s echo mismatch", label)
			return
		}
		count.Add(1)
		time.Sleep(time.Millisecond)
	}
}

func waitForRouteHeartbeat(t *testing.T, count *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if count.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("heartbeat count=%d, want %d", count.Load(), want)
}

func startRouteContinuityEchoServers(t *testing.T) (string, string, func()) {
	t.Helper()
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpListener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		_ = tcpListener.Close()
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		for {
			conn, err := tcpListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	go func() {
		defer wait.Done()
		buffer := make([]byte, 2048)
		for {
			count, address, err := udpListener.ReadFrom(buffer)
			if err != nil {
				return
			}
			_, _ = udpListener.WriteTo(buffer[:count], address)
		}
	}()
	var once sync.Once
	return tcpListener.Addr().String(), udpListener.LocalAddr().String(), func() {
		once.Do(func() {
			_ = tcpListener.Close()
			_ = udpListener.Close()
			wait.Wait()
		})
	}
}
