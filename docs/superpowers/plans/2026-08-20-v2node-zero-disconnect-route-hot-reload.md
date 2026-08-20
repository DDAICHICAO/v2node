# v2node 路由规则零断线热更新 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不关闭 Xray Core、Inbound 和已有 Link 的前提下，把同机全部 NodeID 的纯路由变化合并为一次严格校验后的原子运行代次切换。

**Architecture:** 先把面板配置的 observed/pending/active 状态和严格路由编译器独立出来，再在自定义 Dispatcher 内引入不可变 `RouteRuntimeGeneration`、稳定 DNS feature 和按 Link 持有的 lease。`V2Core` 负责旁路构建 DNS、Router、版本化 Outbound 并发布，`RouteUpdateCoordinator` 负责刷新全部 Controller、确认配置向量稳定、合并 N 个节点变化和原子持久化最后可用快照。

**Tech Stack:** Go 1.26、`GOEXPERIMENT=jsonv2`、Xray Core feature/router/outbound 接口、Resty、标准库并发原语、Go `testing` 与 `-race`。

---

## 文件结构

| 文件 | 职责 |
| --- | --- |
| `api/v2board/panel.go`、`api/v2board/node.go` | 串行化节点配置拉取，并暴露已确认响应体版本 |
| `node/node_info_change.go` | 统一判定 unchanged、托管域名、纯路由、全量重载 |
| `core/route_compile.go` | 严格编译整机 DNS、Router 和版本化自定义 Outbound |
| `core/app/dispatcher/route_runtime.go` | 管理 active/retired 运行代次和引用安全发布 |
| `core/app/dispatcher/runtime_dns.go` | 为 Xray 系统拨号器提供地址稳定、后端随代次切换的 DNS feature |
| `core/app/dispatcher/route_link.go` | 将代次 lease 绑定到真实 Link 生命周期 |
| `core/app/dispatcher/default.go` | 所有 forced/default/routed 流量从同一运行代次解析 handler |
| `core/route_outbound_pool.go` | 创建、共享、回滚和回收版本化 Outbound handler |
| `core/route_runtime.go`、`core/core.go` | 初始化 generation 0，旁路构建候选并原子发布 |
| `node/route_runtime_state.go` | 原子保存整机最后可用 NodeInfo 配置向量 |
| `node/route_update_coordinator.go` | 稳定窗口、全节点刷新、单构建、重试和提交 |
| `node/controller.go`、`node/task.go`、`node/node.go` | 接入 observed/pending/active 状态和协调器生命周期 |
| `conf/conf.go` | 默认开启 `EnableRouteHotReload` 人工回退开关 |

## Task 1：配置拉取串行化与统一变更分类

**Files:**
- Create: `node/node_info_change.go`
- Create: `node/node_info_change_test.go`
- Modify: `api/v2board/panel.go`
- Modify: `api/v2board/node.go`
- Create: `api/v2board/node_config_test.go`

- [ ] **Step 1：先写变更分类失败测试**

在 `node/node_info_change_test.go` 写表驱动测试：

```go
func TestClassifyNodeInfoChange(t *testing.T) {
	base := routeChangeNodeInfo()
	cases := []struct {
		name string
		mutate func(*panel.NodeInfo)
		want nodeInfoChangeKind
	}{
		{name: "unchanged", mutate: func(*panel.NodeInfo) {}, want: nodeInfoUnchanged},
		{name: "routes only", mutate: func(n *panel.NodeInfo) {
			n.Common.Routes = []panel.Route{{Id: 9, Action: "block", Match: []string{"domain:example.com"}}}
		}, want: nodeInfoRoutesOnly},
		{name: "port requires reload", mutate: func(n *panel.NodeInfo) {
			n.Common.ServerPort++
		}, want: nodeInfoFullReload},
		{name: "route and port require reload", mutate: func(n *panel.NodeInfo) {
			n.Common.ServerPort++
			n.Common.Routes = []panel.Route{{Id: 9, Action: "block", Match: []string{"domain:example.com"}}}
		}, want: nodeInfoFullReload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := cloneNodeInfoForTest(base)
			tc.mutate(next)
			if got := classifyNodeInfoChange(base, next); got != tc.want {
				t.Fatalf("kind=%v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2：运行测试确认 RED**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run TestClassifyNodeInfoChange -count=1
```

Expected: FAIL，提示 `nodeInfoChangeKind` 或 `classifyNodeInfoChange` 未定义。

- [ ] **Step 3：实现最小分类器**

```go
type nodeInfoChangeKind uint8

const (
	nodeInfoUnchanged nodeInfoChangeKind = iota
	nodeInfoManagedTLSDomainOnly
	nodeInfoRoutesOnly
	nodeInfoFullReload
)

func classifyNodeInfoChange(current, next *panel.NodeInfo) nodeInfoChangeKind {
	if reflect.DeepEqual(current, next) {
		return nodeInfoUnchanged
	}
	if _, ok := managedTLSDomainOnlyChange(current, next); ok {
		return nodeInfoManagedTLSDomainOnly
	}
	if current == nil || next == nil || current.Common == nil || next.Common == nil {
		return nodeInfoFullReload
	}
	currentCopy, nextCopy := *current, *next
	currentCommon, nextCommon := *current.Common, *next.Common
	currentCommon.Routes = nil
	nextCommon.Routes = nil
	currentCopy.Common = &currentCommon
	nextCopy.Common = &nextCommon
	if reflect.DeepEqual(&currentCopy, &nextCopy) {
		return nodeInfoRoutesOnly
	}
	return nodeInfoFullReload
}
```

- [ ] **Step 4：写配置版本提交时机失败测试**

在 `api/v2board/node_config_test.go` 使用 `httptest.Server` 覆盖：有效响应返回版本；无效 JSON 不推进版本；相同 body 的新 ETag 被记住；并发拉取不产生竞态。核心断言：

```go
info, err := client.GetNodeInfo(context.Background())
if err != nil || info == nil || client.NodeInfoVersion() == "" {
	t.Fatalf("info=%v version=%q err=%v", info, client.NodeInfoVersion(), err)
}
version := client.NodeInfoVersion()
serverBody.Store([]byte(`{"protocol":`))
if _, err := client.GetNodeInfo(context.Background()); err == nil {
	t.Fatal("expected invalid JSON error")
}
if client.NodeInfoVersion() != version {
	t.Fatal("invalid response advanced node config version")
}
```

- [ ] **Step 5：运行测试确认 RED**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./api/v2board -run 'TestNodeConfigVersion|TestNodeConfigFetchSerializes' -count=1
```

Expected: FAIL，提示 `NodeInfoVersion` 未定义，或无效响应错误地推进版本。

- [ ] **Step 6：串行化 Client 配置状态**

给 `panel.Client` 增加：

```go
nodeInfoMu        sync.Mutex
nodeConfigVersion string
```

`GetNodeInfo` 在托管凭据预处理之后持有 `nodeInfoMu`，先解析并构建完整 `NodeInfo`，成功后才提交 `responseBodyHash`、`nodeEtag` 和 `nodeConfigVersion`。相同 body 但 ETag 改变时只更新 ETag。增加：

```go
func (c *Client) NodeInfoVersion() string {
	c.nodeInfoMu.Lock()
	defer c.nodeInfoMu.Unlock()
	return c.nodeConfigVersion
}
```

- [ ] **Step 7：运行目标测试和 race 测试确认 GREEN**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node ./api/v2board -run 'TestClassifyNodeInfoChange|TestNodeConfigVersion|TestNodeConfigFetchSerializes' -count=1
go test -race ./api/v2board -run TestNodeConfigFetchSerializes -count=1
```

Expected: PASS，race detector 无报告。

- [ ] **Step 8：提交**

```powershell
git add node/node_info_change.go node/node_info_change_test.go api/v2board/panel.go api/v2board/node.go api/v2board/node_config_test.go
git commit -m "区分节点配置变更类型" -m "串行化节点配置拉取，并仅在解析成功后推进配置版本。"
```

## Task 2：严格编译路由和版本化 Outbound

**Files:**
- Create: `core/route_compile.go`
- Create: `core/route_compile_test.go`
- Modify: `core/custom.go`
- Modify: `core/custom_test.go`

- [ ] **Step 1：写严格失败和版本化标签测试**

测试非法 `action_value`、相同 tag 的相同配置复用、相同 tag 的不同配置拒绝、Router 引用物理 tag、链式代理引用重写及缺失依赖拒绝：

```go
func TestCompileRouteRuntimeRejectsConflictingOutboundTag(t *testing.T) {
	a := `{"tag":"proxy-a","protocol":"freedom","settings":{}}`
	b := `{"tag":"proxy-a","protocol":"blackhole","settings":{}}`
	infos := []*panel.NodeInfo{
		routeCompileNode(1, panel.Route{Id: 10, Action: "route", Match: []string{"domain:a.test"}, ActionValue: &a}),
		routeCompileNode(2, panel.Route{Id: 11, Action: "route", Match: []string{"domain:b.test"}, ActionValue: &b}),
	}
	_, err := CompileRouteRuntime(infos)
	if err == nil || !strings.Contains(err.Error(), "node_id=2") || !strings.Contains(err.Error(), "action=route") {
		t.Fatalf("unexpected error: %v", err)
	}
}
```

- [ ] **Step 2：运行测试确认 RED**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core -run 'TestCompileRouteRuntime' -count=1
```

Expected: FAIL，提示 `CompileRouteRuntime` 未定义。

- [ ] **Step 3：定义确定性编译结果**

```go
type CompiledRouteRuntime struct {
	DNSConfig         *dns.Config
	RouterConfig      *router.Config
	CustomOutbounds   []*xraycore.OutboundHandlerConfig
	LogicalToPhysical map[string]string
	ConfigHash        string
}

func physicalOutboundTag(logical string, config *xraycore.OutboundHandlerConfig) (string, error) {
	clone := proto.Clone(config).(*xraycore.OutboundHandlerConfig)
	clone.Tag = logical
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%s@%s", logical, hex.EncodeToString(sum[:6])), nil
}
```

保留 `Default`、`block`、`dns_out` 为内建 tag，自定义 Outbound 不得覆盖。

- [ ] **Step 4：实现两遍严格编译**

第一遍按规则顺序收集 DNS 片段和自定义 `OutboundDetourConfig`，使用逻辑 tag 构建确定性 protobuf 并计算物理 tag。相同逻辑 tag 的 protobuf 相同则复用，不同则报冲突。第二遍重写 `ProxySettings.Tag` 与 `StreamSetting.SocketSettings.DialerProxy`，构建物理 Outbound 和 Router 规则。

```go
func routeCompileError(info *panel.NodeInfo, index int, route panel.Route, err error) error {
	return fmt.Errorf("compile route: node_id=%d rule_index=%d action=%s: %w", info.Id, index, route.Action, err)
}
```

非法 JSON、空 tag、未知 action、缺失引用或 `Build()` 错误均返回错误，不记录 `ActionValue`。

- [ ] **Step 5：兼容启动复用同一基础设施**

`GetCustomConfig` 保留签名，调用 `compileRouteRuntime(infos, strict bool)`。启动兼容模式对历史错误记录 warning 并跳过；热更新入口始终严格：

```go
func CompileRouteRuntime(infos []*panel.NodeInfo) (*CompiledRouteRuntime, error) {
	return compileRouteRuntime(infos, true)
}
```

- [ ] **Step 6：运行目标测试确认 GREEN**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core -run 'TestCompileRouteRuntime|TestApplyDNSRouteConfig' -count=1
```

Expected: PASS，原有 DNS 路由测试继续通过。

- [ ] **Step 7：提交**

```powershell
git add core/route_compile.go core/route_compile_test.go core/custom.go core/custom_test.go
git commit -m "严格编译路由运行配置" -m "为自定义出口生成稳定物理标签，并拒绝残缺或冲突的候选规则。"
```

## Task 3：不可变运行代次与稳定 DNS feature

**Files:**
- Create: `core/app/dispatcher/route_runtime.go`
- Create: `core/app/dispatcher/route_runtime_test.go`
- Create: `core/app/dispatcher/runtime_dns.go`
- Create: `core/app/dispatcher/runtime_dns_test.go`

- [ ] **Step 1：写 Acquire/Publish/Retire 失败测试**

```go
func TestRouteRuntimeRetiresOnlyAfterLastLease(t *testing.T) {
	var cleaned atomic.Int32
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{
		ID: 1,
		Router: &fakeRouter{name: "old"},
		Cleanup: func() error { cleaned.Add(1); return nil },
	})
	lease, err := manager.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2, Router: &fakeRouter{name: "new"}}); err != nil {
		t.Fatal(err)
	}
	if cleaned.Load() != 0 {
		t.Fatal("old generation cleaned while lease was active")
	}
	lease.Release()
	if cleaned.Load() != 1 {
		t.Fatalf("cleanup count=%d", cleaned.Load())
	}
}
```

再覆盖嵌套 context 取得旧代次和 Acquire/Publish 并发只见完整状态。

- [ ] **Step 2：运行测试确认 RED**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core/app/dispatcher -run 'TestRouteRuntime' -count=1
```

Expected: FAIL，运行代次类型未定义。

- [ ] **Step 3：实现引用安全 Runtime Manager**

```go
type RouteRuntimeGeneration struct {
	ID       uint64
	Router   routing.Router
	DNS      dns.Client
	Handlers map[string]outbound.Handler
	Default  outbound.Handler
	Cleanup  func() error
}

type RouteRuntimeLease struct {
	manager    *RouteRuntimeManager
	generation *routeRuntimeGeneration
	once       sync.Once
}

func (l *RouteRuntimeLease) Router() routing.Router
func (l *RouteRuntimeLease) Handler(tag string) outbound.Handler
func (l *RouteRuntimeLease) DefaultHandler() outbound.Handler
func (l *RouteRuntimeLease) Context(context.Context) context.Context
func (l *RouteRuntimeLease) Release()
```

一个短 `sync.Mutex` 同时保护 active、registry、retired 和引用递增，禁止先无锁读指针再递增。cleanup 只在取得一次清理权后于锁外执行。

Task 4 的生命周期测试需要同步读取状态，因此本任务同时定义最小只读统计接口：

```go
type RouteRuntimeStats struct {
	ActiveGeneration uint64
	Retired          int
	ActiveLeases     int64
	OldestRetired    time.Duration
}

func (m *RouteRuntimeManager) Stats() RouteRuntimeStats
```

- [ ] **Step 4：写稳定 DNS 查询失败测试**

```go
func TestRuntimeDNSClientPinsOneBackendPerLookup(t *testing.T) {
	oldDNS := newBlockingDNS("192.0.2.1")
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{ID: 1, DNS: oldDNS})
	client := NewRuntimeDNSClient()
	client.Bind(manager)
	result := make(chan string, 1)
	go func() {
		ips, _, _ := client.LookupIP("example.com", dns.IPOption{IPv4Enable: true})
		result <- ips[0].String()
	}()
	<-oldDNS.started
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2, DNS: &fixedDNS{ip: "198.51.100.2"}}); err != nil {
		t.Fatal(err)
	}
	close(oldDNS.release)
	if got := <-result; got != "192.0.2.1" {
		t.Fatalf("old lookup result=%s", got)
	}
}
```

- [ ] **Step 5：实现 RuntimeDNSClient**

`RuntimeDNSClient` 实现 `features/dns.Client`，`LookupIP` 取得 DNS lease 并在查询后释放。`Start` 无副作用，`Close` 标记 feature 已关闭。使用现有未单独注册的 `dispatcher.SessionConfig` protobuf 作为 feature marker：

```go
func RuntimeDNSFeatureConfig() *SessionConfig { return &SessionConfig{} }
```

在 `init` 中为 `SessionConfig` 注册构造器并返回 `NewRuntimeDNSClient()`，不修改外部 Xray fork。

- [ ] **Step 6：运行测试和 race 测试确认 GREEN**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core/app/dispatcher -run 'TestRouteRuntime|TestRuntimeDNSClient' -count=1
go test -race ./core/app/dispatcher -run 'TestRouteRuntime|TestRuntimeDNSClient' -count=1
```

Expected: PASS，cleanup 次数准确且无 data race。

- [ ] **Step 7：提交**

```powershell
git add core/app/dispatcher/route_runtime.go core/app/dispatcher/route_runtime_test.go core/app/dispatcher/runtime_dns.go core/app/dispatcher/runtime_dns_test.go
git commit -m "增加不可变路由运行代次" -m "通过引用安全发布和稳定 DNS feature 保持新旧查询资源连续。"
```

## Task 4：Dispatcher 绑定代次 lease 到真实 Link

**Files:**
- Create: `core/app/dispatcher/route_link.go`
- Create: `core/app/dispatcher/route_link_test.go`
- Modify: `core/app/dispatcher/default.go`
- Modify: `core/app/dispatcher/linkmanager_test.go`

- [ ] **Step 1：写 Link 未结束不得回收失败测试**

构造 fake Router、old/new handler 和可控 Reader/Writer，调用真实 `routedDispatch`。handler 的 `Dispatch` 即使已经返回，只要 Link 未结束，旧 generation 仍不得 cleanup：

```go
func TestRoutedDispatchKeepsGenerationUntilLinkEnds(t *testing.T) {
	reader := newBlockingLinkReader()
	writer := newBlockingLinkWriter()
	oldHandler := &recordingHandler{tag: "old"}
	d := newRuntimeDispatcherForTest("old", oldHandler)
	d.routedDispatch(testRoutingContext("node-1"), &transport.Link{Reader: reader, Writer: writer}, testDestination())
	if err := d.runtime.Publish(testGeneration(2, "new", &recordingHandler{tag: "new"})); err != nil {
		t.Fatal(err)
	}
	if d.runtime.Stats().Retired != 1 {
		t.Fatal("old generation was not retained")
	}
	reader.Interrupt()
	_ = writer.Close()
	waitForRetiredCount(t, d.runtime, 0)
}
```

- [ ] **Step 2：运行测试确认 RED**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core/app/dispatcher -run 'TestRoutedDispatchKeepsGeneration|TestRouteLink' -count=1
```

Expected: FAIL，Dispatcher 尚未使用 runtime lease 或 Link 包装不存在。

- [ ] **Step 3：实现双向 Link 生命周期包装**

```go
type routeLinkTracker struct {
	lease     *RouteRuntimeLease
	remaining atomic.Int32
	finishOnce sync.Once
	done      chan struct{}
}

func newRouteTrackedLink(ctx context.Context, link *transport.Link, lease *RouteRuntimeLease) *transport.Link
```

Reader 在终止错误、`Close` 或 `Interrupt` 时结束读侧；Writer 在写错误或 `Close` 时结束写侧；两侧都结束或 context 取消时通过 `sync.Once` 释放 lease。context watcher 同时监听 tracker.done，双侧结束后立即退出。包装实现 `buf.TimeoutReader` 并把 Close/Interrupt 传递到底层对象。

- [ ] **Step 4：所有出站选择使用同一 generation**

给 `DefaultDispatcher` 增加：

```go
runtime *RouteRuntimeManager

func (d *DefaultDispatcher) BindRouteRuntime(manager *RouteRuntimeManager) {
	d.runtime = manager
}
```

`routedDispatch` 开始时 Acquire。forced tag、Router tag 和默认 handler 都从 lease 解析；错误路径先 Release 再关闭 Link。调用 handler 前把 generation ID 写入 context 并用 `newRouteTrackedLink` 接管 lease。嵌套 Dispatch 由 context 取得同一 generation。

未绑定 manager 时保留现有 `d.router`/`d.ohm` 路径，保证现有单元测试可运行；生产启动必须在 Inbound 启动前完成 Bind。

- [ ] **Step 5：写并发发布与新连接风暴测试**

循环发布 old/new generation，同时启动至少 1,000 次 routed dispatch。每次必须命中 old 或 new handler，缺失 handler、主动关闭 Link 和 panic 计数均为零。测试最后关闭全部 Link 并断言 retired 数量回落。

- [ ] **Step 6：运行测试和 race 测试确认 GREEN**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core/app/dispatcher -run 'TestRoutedDispatchKeepsGeneration|TestRouteLink|TestConcurrentRoutePublishAndDispatch' -count=1
go test -race ./core/app/dispatcher -run TestConcurrentRoutePublishAndDispatch -count=1
```

Expected: PASS，新连接失败计数为零且无 data race。

- [ ] **Step 7：提交**

```powershell
git add core/app/dispatcher/route_link.go core/app/dispatcher/route_link_test.go core/app/dispatcher/default.go core/app/dispatcher/linkmanager_test.go
git commit -m "让连接持有路由运行代次" -m "统一 forced、默认和规则出站解析，并在 Link 结束后回收旧代次。"
```

## Task 5：V2Core 旁路构建、Outbound 资源池和原子发布

**Files:**
- Create: `core/route_outbound_pool.go`
- Create: `core/route_outbound_pool_test.go`
- Create: `core/route_runtime.go`
- Create: `core/route_runtime_test.go`
- Modify: `core/core.go`

- [ ] **Step 1：写 Outbound 候选回滚和共享失败测试**

使用完整实现 `outbound.Manager` 的内存 manager，验证相同物理 tag 只创建一次、部分启动失败撤销本候选新增 handler、共享 handler 不被提前关闭、最后引用释放后先 Remove 再 Close：

```go
func TestRouteOutboundPoolRollsBackPartialCandidate(t *testing.T) {
	manager := newMemoryOutboundManager()
	pool := newRouteOutboundPool(manager, fakeOutboundFactory(map[string]error{
		"bad@222": errors.New("start failed"),
	}))
	_, release, err := pool.Acquire(context.Background(), []*xraycore.OutboundHandlerConfig{
		{Tag: "good@111", ProxySettings: testFreedomSettings()},
		{Tag: "bad@222", ProxySettings: testFreedomSettings()},
	})
	if err == nil {
		t.Fatal("expected candidate failure")
	}
	if release != nil || manager.GetHandler("good@111") != nil {
		t.Fatal("partial candidate handler was not rolled back")
	}
}
```

- [ ] **Step 2：运行测试确认 RED**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core -run 'TestRouteOutboundPool|TestApplyRouteRuntime' -count=1
```

Expected: FAIL，资源池和 `ApplyRouteRuntime` 未定义。

- [ ] **Step 3：实现 Outbound 资源池**

```go
type outboundFactory func(*xraycore.OutboundHandlerConfig) (outbound.Handler, error)

func (p *routeOutboundPool) Acquire(
	ctx context.Context,
	configs []*xraycore.OutboundHandlerConfig,
) (map[string]outbound.Handler, func() error, error)
```

生产 factory 使用 `xraycore.CreateObject(v.Server, config)` 构造 handler，再调用 `ohm.AddHandler`。回收顺序为 `ohm.RemoveHandler` 后 `common.Close(handler)`。返回的 release 幂等；候选失败立即释放，发布后纳入 generation Cleanup。

- [ ] **Step 4：Core 初始化改为显式错误结果**

```go
type coreBuild struct {
	server          *xraycore.Instance
	dnsConfig       *dns.Config
	routerConfig    *router.Config
	outboundConfigs []*xraycore.OutboundHandlerConfig
}

func buildCore(c *conf.Conf, infos []*panel.NodeInfo) (*coreBuild, error)
```

App 不再注册具体 `dns.Config`，改为 `dispatcher.RuntimeDNSFeatureConfig()`；Router 和 DNS Outbound 从 Core 创建起捕获稳定 feature。`V2Core.Start` 在 `server.Start()` 前构造初始具体 DNS、收集初始 handlers、创建 generation 0、Bind RuntimeDNSClient、Bind DefaultDispatcher。全部绑定后才能启动 feature 和后续 Inbound。

- [ ] **Step 5：实现候选构建与发布**

```go
func (v *V2Core) ApplyRouteRuntime(infos []*panel.NodeInfo) (uint64, error)
```

固定顺序：严格编译；用 `xraycore.CreateObject` 构造并 Start 新 DNS；从资源池 Acquire 自定义 handlers；构造并 Start 新 Router；组装 built-in、逻辑和物理 handler map；验证全部 Router tag 可解析；调用 manager.Publish。发布前错误按 Router、DNS、Outbound 逆序清理，active generation 不变。

generation Cleanup 关闭 Router 和 DNS，再释放 Outbound pool；清理错误记录但不得撤销已发布的新代次。

- [ ] **Step 6：写真实 V2Core 热发布和兼容预检测试**

启动无 Inbound 的 `V2Core`，从 block 路由提交 freedom 自定义 Outbound。断言 generation 递增，`V2Core.Server`、dispatcher、inbound manager 指针不变；非法候选不改变 generation；旧 lease 释放后旧自定义 handler 从 manager 消失。再用一份会被旧兼容编译跳过、但会被严格编译拒绝的历史规则启动：generation 0 必须可用，严格预检状态必须为 degraded，不能把残缺候选提升为最后可用配置；修正规则后严格 Apply 成功并解除 degraded。

- [ ] **Step 7：运行测试和回归测试确认 GREEN**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core -run 'TestRouteOutboundPool|TestApplyRouteRuntime|TestInboundReload' -count=1
go test -race ./core -run TestApplyRouteRuntime -count=1
```

Expected: PASS，Core 与 Inbound 身份保持不变。

- [ ] **Step 8：提交**

```powershell
git add core/route_outbound_pool.go core/route_outbound_pool_test.go core/route_runtime.go core/route_runtime_test.go core/core.go
git commit -m "支持核心路由原子热发布" -m "旁路构建 DNS、Router 和版本化出口，失败时保持当前运行代次。"
```

## Task 6：整机最后可用路由快照

**Files:**
- Create: `node/route_runtime_state.go`
- Create: `node/route_runtime_state_test.go`
- Modify: `node/node.go`
- Modify: `node/offline_state_test.go`

- [ ] **Step 1：写原子快照和恢复覆盖失败测试**

覆盖两个 NodeID 完整保存、损坏候选不删除上一版、identity 不匹配拒绝、文件权限不宽于 `0600`，以及离线启动只覆盖旧单节点快照的 NodeInfo、不覆盖 Users/Alive/UserSyncSeq：

```go
func TestRouteRuntimeStateOverlaysNodeInfoOnly(t *testing.T) {
	state := testOfflineState(conf.NodeConfig{APIHost: "https://panel.test", NodeID: 1})
	state.Users = []panel.UserInfo{{Id: 7}}
	runtimeInfo := testOfflineNodeInfo(1)
	runtimeInfo.Common.Routes = []panel.Route{{Id: 99, Action: "block"}}
	overlayRouteRuntimeState(state, routeRuntimeEntry{
		APIHost: "https://panel.test", NodeID: 1, NodeInfo: runtimeInfo,
	})
	if state.NodeInfo.Common.Routes[0].Id != 99 || state.Users[0].Id != 7 {
		t.Fatalf("overlay changed wrong fields: %+v", state)
	}
}
```

- [ ] **Step 2：运行测试确认 RED**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestRouteRuntimeState|TestRouteRuntimeStateOverlays' -count=1
```

Expected: FAIL，整机快照类型未定义。

- [ ] **Step 3：实现快照结构和原子写入**

```go
const routeRuntimeStateVersion = 1

type routeRuntimeState struct {
	Version    int                 `json:"version"`
	Generation uint64              `json:"generation"`
	SavedAt    int64               `json:"saved_at"`
	VectorHash string              `json:"vector_hash"`
	Entries    []routeRuntimeEntry `json:"entries"`
}

type routeRuntimeEntry struct {
	APIHost       string          `json:"api_host"`
	NodeID        int             `json:"node_id"`
	ConfigVersion string          `json:"config_version"`
	NodeInfo      *panel.NodeInfo `json:"node_info"`
}
```

Store 使用 `StatePath/route-runtime.json`、目录 `0700`、临时文件 `0600`、Sync、Close、原子 Rename。校验 NodeID/APIHost 唯一且与 NodeInfo 一致，失败不删除旧文件。

- [ ] **Step 4：接入启动恢复**

`Node.New` 只加载一次整机快照。单节点 offline state 加载成功后，若整机 entry identity 匹配，则只替换 `cached.NodeInfo`；用户、在线状态和序列保留。面板在线返回配置仍优先。

- [ ] **Step 5：运行目标测试确认 GREEN**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestRouteRuntimeState|TestRouteRuntimeStateOverlays|TestOfflineState' -count=1
```

Expected: PASS。

- [ ] **Step 6：提交**

```powershell
git add node/route_runtime_state.go node/route_runtime_state_test.go node/node.go node/offline_state_test.go
git commit -m "持久化整机最后可用路由" -m "原子保存多节点配置向量，并在离线启动时一致恢复 NodeInfo。"
```

## Task 7：多节点稳定窗口协调器

**Files:**
- Create: `node/route_update_coordinator.go`
- Create: `node/route_update_coordinator_test.go`
- Modify: `node/controller.go`
- Modify: `node/task.go`
- Modify: `node/node.go`

- [ ] **Step 1：写 N 节点一次发布失败测试**

内存 target 断言最终 active 状态、generation 数和 pending 状态，不断言 mock 调用本身。两个节点先后报告同一次变化只能 Apply 一次；两次配置向量不同必须重新稳定；任一刷新失败 active 保持旧值；首次 Apply 失败且后续刷新为 `304` 时仍使用 retained pending 重试；连续失败的退避不得超过 30 秒且新 Notify 必须立即打断退避：

```go
func TestRouteUpdateCoordinatorCoalescesAllNodeIDs(t *testing.T) {
	targets := []*fakeRouteUpdateTarget{
		newFakeRouteTarget(1, "v1", "v2"),
		newFakeRouteTarget(2, "v1", "v2"),
	}
	applier := &recordingRouteApplier{}
	c := newRouteUpdateCoordinatorForTest(targets, applier, t.TempDir())
	c.Notify()
	waitForApplyCount(t, applier, 1)
	if got := applier.LastNodeIDs(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("published node ids=%v", got)
	}
	for _, target := range targets {
		if target.ActiveVersion() != "v2" || target.HasPending() {
			t.Fatalf("target not committed: %+v", target)
		}
	}
}
```

- [ ] **Step 2：运行测试确认 RED**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run TestRouteUpdateCoordinator -count=1
```

Expected: FAIL，协调器接口未定义。

- [ ] **Step 3：定义生产 target/applier 边界**

```go
type routeObservedState struct {
	NodeID   int
	APIHost  string
	Version  string
	Active   *panel.NodeInfo
	Observed *panel.NodeInfo
}

type routeUpdateTarget interface {
	RefreshObserved(context.Context) (routeObservedState, error)
	ObservedState() routeObservedState
	CommitRouteNodeInfo(*panel.NodeInfo, string) error
}

type routeRuntimeApplier interface {
	ApplyRouteRuntime([]*panel.NodeInfo) (uint64, error)
}
```

Controller 使用 `configFetchMu` 串行普通轮询和协调器刷新，`nodeInfoApplyMu` 串行 pending 分类与 commit，`stateMu` 继续保护 active info 和用户状态。

- [ ] **Step 4：实现单 worker 稳定向量状态机**

协调器只维护容量 1 的 wake channel 和单调 dirty epoch。默认稳定窗口 500ms、两次 sweep 间隔 250ms、刷新并发上限 4。每次 sweep 刷新全部 target，按 NodeID 排序并计算 `NodeID + version` 向量哈希。

只有两次向量相同、每个 target 都是 unchanged/routes-only、发布前 epoch 未变化时才 Apply。失败保留 pending，使用带抖动指数退避，最大 30 秒；新 Notify 打断退避。Close 取消刷新并等待 worker 退出。

- [ ] **Step 5：提交成功状态和快照**

Apply 成功后调用每个 target 的 `CommitRouteNodeInfo` 更新 active 并尝试单节点快照，再一次性保存 `routeRuntimeState`。持久化失败记录 degraded 并后台重试，不回滚 generation。若 commit 时已经有更新版本，只提交本次版本并保留较新 pending，再次 Notify。

- [ ] **Step 6：Controller 轮询接入协调器**

`nodeInfoMonitor` 拉到新配置后保存 observed/pending。`applyPendingNodeInfo` 使用统一分类：managed TLS 调现有路径；routes-only 且开关开启时只 Notify，不清 pending、不 queue reload；其他变化继续持久化并 `queueReload`。协调器为空或开关关闭时纯路由沿用旧 reload。

`Node.Start` 在 Controller 启动任务前创建并启动协调器，注入全部 Controller 和 `V2Core`；`Node.Close` 先关闭协调器，再关闭 Controller。

- [ ] **Step 7：运行测试和 race 测试确认 GREEN**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestRouteUpdateCoordinator|TestNodeInfoMonitor' -count=1
go test -race ./node -run TestRouteUpdateCoordinator -count=1
```

Expected: PASS，N 节点只发布一次，失败时 active 不变且无 data race。

- [ ] **Step 8：提交**

```powershell
git add node/route_update_coordinator.go node/route_update_coordinator_test.go node/controller.go node/task.go node/node.go
git commit -m "合并同机多节点路由更新" -m "刷新完整配置向量并在稳定后只发布一个运行代次。"
```

## Task 8：配置开关、可观测状态和端到端不重载验证

**Files:**
- Modify: `conf/conf.go`
- Modify: `conf/conf_test.go`
- Modify: `core/app/dispatcher/route_runtime.go`
- Modify: `core/app/dispatcher/route_runtime_test.go`
- Modify: `node/route_update_coordinator.go`
- Create: `node/route_hot_reload_integration_test.go`

- [ ] **Step 1：写默认开关和人工回退失败测试**

```go
func TestRouteHotReloadDefaultsEnabled(t *testing.T) {
	c := New()
	if !c.EnableRouteHotReload {
		t.Fatal("route hot reload must default to enabled")
	}
}
```

再加载显式 `EnableRouteHotReload: false` 的临时配置，断言关闭后纯路由 pending 进入现有 reload channel，不启动协调器。

- [ ] **Step 2：运行测试确认 RED**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./conf ./node -run 'TestRouteHotReloadDefaultsEnabled|TestRouteHotReloadDisabled' -count=1
```

Expected: FAIL，配置字段不存在。

- [ ] **Step 3：增加最小人工回退配置**

```go
EnableRouteHotReload bool `mapstructure:"EnableRouteHotReload"`
```

`conf.New()` 默认 true。加载时保留用户显式 false；不增加稳定窗口、并发和退避等额外用户配置。

- [ ] **Step 4：补齐协调器统计和结构化日志**

为 Task 3 已定义的 `RouteRuntimeStats` 增加测试，确认并发读取不影响 Acquire/Publish。协调器用 atomic 计数 attempts、coalesced、success、failure、stale，并只记录 generation、NodeID 数量、向量哈希前缀和耗时。日志不得输出 Match、ActionValue 或 Outbound JSON。

- [ ] **Step 5：写端到端纯路由不重载测试**

测试名固定为 `TestRouteHotReloadKeepsExistingAndNewConnectionsOnline`。使用两个 Controller 状态、真实协调器、可控 panel HTTP server、本地 TCP/UDP echo 和记录型 applier，验证：两个 NodeID 的 ETag 同时变化；持续 TCP 双向传输和 UDP Link 保持打开；Apply 期间新连接全部命中 old/new handler；`ReloadCh` 为空；Core/Inbound 身份不变；generation 只增加一次；面板请求持续成功。再重复切换至少 100 次，关闭全部测试 Link 后断言 goroutine、lease、retired generation 和 Outbound 资源数回落到基线容差内。

- [ ] **Step 6：运行端到端和重复压力测试**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./conf ./node ./core/app/dispatcher -run 'TestRouteHotReload|TestConcurrentRoutePublishAndDispatch' -count=10
go test -race ./node ./core/app/dispatcher -run 'TestRouteHotReload|TestConcurrentRoutePublishAndDispatch' -count=1
```

Expected: 每轮 PASS，连接失败、ReloadCh 写入和 data race 均为零。

- [ ] **Step 7：提交**

```powershell
git add conf/conf.go conf/conf_test.go core/app/dispatcher/route_runtime.go core/app/dispatcher/route_runtime_test.go node/route_update_coordinator.go node/route_hot_reload_integration_test.go
git commit -m "默认启用路由零断线热更新" -m "补充运行统计、人工回退开关和连接连续性集成验证。"
```

## Task 9：设计校准、经验记录和完整验证

**Files:**
- Modify: `docs/superpowers/specs/2026-08-20-v2node-zero-disconnect-route-hot-reload-design.md`
- Modify: `LESSONS_LEARNED.md`

- [ ] **Step 1：校准 DNS 设计文字**

把“DNS Outbound 已建立 Link 固定旧 DNS handler”的表述校准为可验证边界：Router/Outbound handler 按 Link 固定代次；通过稳定 DNS feature 的查询按单次 Lookup 固定后端。说明 Xray `dns.Client.LookupIP` 不携带连接 context，但该边界仍保证没有 nil、已关闭后端或连接建立失败窗口。

- [ ] **Step 2：更新经验记录**

在 `LESSONS_LEARNED.md` 增加：症状；v2board config ETag → `nodeInfoMonitor` → `ReloadCh` → Core Close/Start 链路；根因；稳定向量、严格编译、运行代次、Link lease 修复；验证命令；设计和计划路径。不得记录 token、代理凭据、完整路由值或用户数据。

- [ ] **Step 3：运行格式和差异检查**

```powershell
gofmt -w api/v2board/panel.go api/v2board/node.go api/v2board/node_config_test.go node/node_info_change.go node/node_info_change_test.go node/route_runtime_state.go node/route_runtime_state_test.go node/route_update_coordinator.go node/route_update_coordinator_test.go node/route_hot_reload_integration_test.go node/controller.go node/task.go node/node.go core/route_compile.go core/route_compile_test.go core/route_outbound_pool.go core/route_outbound_pool_test.go core/route_runtime.go core/route_runtime_test.go core/core.go core/custom.go core/custom_test.go core/app/dispatcher/route_runtime.go core/app/dispatcher/route_runtime_test.go core/app/dispatcher/runtime_dns.go core/app/dispatcher/runtime_dns_test.go core/app/dispatcher/route_link.go core/app/dispatcher/route_link_test.go core/app/dispatcher/default.go conf/conf.go conf/conf_test.go
git diff --check
```

Expected: 两条命令退出码均为 0。

- [ ] **Step 4：运行分层测试**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./api/v2board ./conf ./core/app/dispatcher ./core ./node -count=1
go test -race ./core/app/dispatcher ./core ./node -run 'TestRoute|TestConcurrentRoutePublishAndDispatch' -count=1
```

Expected: 目标包 PASS，race detector 无报告。

- [ ] **Step 5：运行完整测试**

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./... -count=1
```

Expected: 全仓库 PASS。若失败，记录准确包、测试名和输出，修复后重新完整执行该命令。

- [ ] **Step 6：审阅最终差异和敏感信息**

```powershell
git status --short
git diff --stat
git diff -- api/v2board node core conf docs/superpowers/specs LESSONS_LEARNED.md
rg -n "ApiKey|token=|password|action_value.*\{" node core docs/superpowers/specs LESSONS_LEARNED.md
```

Expected: 只有计划内文件变化，未写入真实凭据、完整 ActionValue 或用户数据。

- [ ] **Step 7：提交文档和最终修整**

```powershell
git add docs/superpowers/specs/2026-08-20-v2node-zero-disconnect-route-hot-reload-design.md LESSONS_LEARNED.md
git commit -m "记录路由热更新运行边界" -m "补充故障链、DNS 查询边界、验证入口和后续排查位置。"
```

- [ ] **Step 8：请求最终独立审查**

以设计文档和本计划为要求，审查从计划基线到当前 HEAD 的全部差异。Critical 和 Important 问题必须修复并重新执行相关测试；最终重新运行 `GOEXPERIMENT=jsonv2 go test ./... -count=1` 和 `git diff --check`，之后才能进入分支交付流程。
