# v2node Panel Offline Snapshot Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `v2node` 在面板通讯中断时无限期使用最后一次成功状态继续服务，并在进程重启后从持久化快照恢复。

**Architecture:** 在 `node` 包内新增按 `ApiHost + NodeID` 隔离的原子快照存储，并把启动数据装载集中到一个可测试的 bootstrap 组件。运行中的面板错误只改变离线状态和日志，不改变已生效用户或触发 reload；面板恢复并完整同步成功后再更新快照。

**Tech Stack:** Go 1.26、标准库 JSON/文件原子替换、现有 Resty 面板客户端、现有 `common/task` 周期任务、Go `testing`/`httptest`

---

## 规范与文件结构

实施必须逐条满足已确认规范：`docs/superpowers/specs/2026-07-14-v2node-offline-snapshot-design.md`。

文件职责固定如下：

- `conf/conf.go`：提供 `StatePath` 配置及 `/etc/v2node/offline-state` 默认值。
- `conf/conf_test.go`：验证默认目录与自定义目录归一化。
- `node/offline_state.go`：定义快照格式、身份校验、稳定文件名、`0600` 权限和临时文件原子替换。
- `node/offline_state_test.go`：验证快照往返、空用户列表、损坏/串节点拒绝和失败写入不覆盖旧状态。
- `node/bootstrap.go`：把在线数据和最后快照组合成完整启动状态，不启动真实代理内核。
- `node/bootstrap_test.go`：使用内存 fake 验证在线优先、离线回退和首次无快照失败。
- `node/node.go`：为每个配置节点加载快照、调用 bootstrap，并把准备好的状态交给 controller。
- `node/controller.go`：使用准备好的启动状态初始化 limiter/core，并在完全在线启动成功后保存快照。
- `node/user.go`：把面板上报失败纳入离线状态，但不让上报失败影响代理数据面。
- `node/offline_status.go`：维护进入离线、24 小时、72 小时和恢复事件，确保重复失败不刷屏。
- `node/offline_status_test.go`：验证告警只出现一次且恢复会重置状态。
- `node/task.go`：面板同步失败时保留原状态；完整同步成功后持久化；节点配置 reload 前先保存可启动状态。
- `node/user_delta_test.go`：增加面板不可达时用户列表和 reload 信号保持不变的回归测试。
- `common/task/task.go`：为任务增加显式 `ReloadOnTimeout` 策略。
- `common/task/task_test.go`：验证面板任务超时不 reload，显式恢复任务仍可 reload。
- `cmd/server.go`：启动和 reload 时把 `StatePath` 传给 `node.New`。
- `docs/panel-offline-mode.md`：运维说明、Docker 持久化和故障处理。
- `LESSONS_LEARNED.md`：记录本次控制面拖垮数据面的故障链和排查入口。

## Task 1: 增加离线状态目录配置

**Files:**
- Create: `conf/conf_test.go`
- Modify: `conf/conf.go:10-67`

- [ ] **Step 1: 写默认值和自定义值的失败测试**

```go
package conf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewDefaultsOfflineStatePath(t *testing.T) {
	c := New()
	if c.StatePath != DefaultStatePath {
		t.Fatalf("StatePath=%q, want %q", c.StatePath, DefaultStatePath)
	}
}

func TestLoadFromPathTrimsOfflineStatePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"StatePath":"  /tmp/v2node-state  "}`), 0600); err != nil {
		t.Fatal(err)
	}

	c := New()
	if err := c.LoadFromPath(path); err != nil {
		t.Fatal(err)
	}
	if c.StatePath != "/tmp/v2node-state" {
		t.Fatalf("StatePath=%q", c.StatePath)
	}
}
```

- [ ] **Step 2: 运行测试并确认因字段缺失而失败**

Run: `go test ./conf -run 'Test(NewDefaultsOfflineStatePath|LoadFromPathTrimsOfflineStatePath)' -count=1`

Expected: FAIL，编译错误包含 `StatePath undefined` 或 `DefaultStatePath undefined`。

- [ ] **Step 3: 写最小配置实现**

在 `conf/conf.go` 中加入：

```go
const DefaultStatePath = "/etc/v2node/offline-state"

type Conf struct {
	LogConfig         LogConfig         `mapstructure:"Log"`
	AccessAuditConfig AccessAuditConfig `mapstructure:"AccessAudit"`
	NodeConfigs       []NodeConfig      `mapstructure:"Nodes"`
	StatePath         string            `mapstructure:"StatePath"`
	PprofPort         int               `mapstructure:"PprofPort"`
}
```

在 `New()` 返回值中加入 `StatePath: DefaultStatePath`，并在 `LoadFromPath()` 完成 `Unmarshal` 后加入：

```go
p.StatePath = strings.TrimSpace(p.StatePath)
if p.StatePath == "" {
	p.StatePath = DefaultStatePath
}
```

- [ ] **Step 4: 格式化并运行配置测试**

Run: `gofmt -w conf/conf.go conf/conf_test.go; go test ./conf -count=1`

Expected: PASS。

- [ ] **Step 5: 提交配置改动**

```powershell
git add -- conf/conf.go conf/conf_test.go
git commit -m "功能：增加节点离线状态目录配置" -m "提供默认离线快照目录，并允许 Docker 和自定义部署覆盖保存位置。"
```

## Task 2: 实现安全的最后成功快照存储

**Files:**
- Create: `node/offline_state.go`
- Create: `node/offline_state_test.go`

- [ ] **Step 1: 写快照往返和校验失败测试**

测试中使用以下公共 fixture，避免依赖真实端口：

```go
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
		Version:       offlineStateVersion,
		APIHost:       normalizeAPIHost(cfg.APIHost),
		NodeID:        cfg.NodeID,
		SavedAt:       1_700_000_000,
		NodeInfo:      testOfflineNodeInfo(cfg.NodeID),
		Users:         []panel.UserInfo{},
		Alive:         map[int]int{},
		DeviceAlive:   map[int]int{},
		UserSyncSeq:   42,
	}
}
```

增加三个测试：

```go
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
```

同一步增加损坏文件与未知版本测试：直接向 `store.pathFor(cfg)` 写入 `{`，断言 `Load` 返回 `decode offline state`；保存合法状态后把 JSON 中 `"version":1` 替换为 `"version":2`，断言返回 `unsupported offline state version 2`。这些测试也必须在实现前失败。

- [ ] **Step 2: 运行测试并确认快照类型尚不存在**

Run: `go test ./node -run 'TestOfflineStateStore' -count=1`

Expected: FAIL，编译错误包含 `undefined: offlineState`。

- [ ] **Step 3: 实现快照格式、身份校验和原子替换**

`node/offline_state.go` 必须提供以下完整接口和行为：

```go
package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

const offlineStateVersion = 1

type offlineState struct {
	Version     int                `json:"version"`
	APIHost     string             `json:"api_host"`
	NodeID      int                `json:"node_id"`
	SavedAt     int64              `json:"saved_at"`
	NodeInfo    *panel.NodeInfo    `json:"node_info"`
	Users       []panel.UserInfo   `json:"users"`
	Alive       map[int]int        `json:"alive"`
	DeviceAlive map[int]int        `json:"device_alive"`
	UserSyncSeq int64              `json:"user_sync_seq"`
}

type offlineStateStore struct {
	dir string
}

func newOfflineStateStore(dir string) *offlineStateStore {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = conf.DefaultStatePath
	}
	return &offlineStateStore{dir: dir}
}

func normalizeAPIHost(value string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(value)), "/")
}

func (s *offlineStateStore) pathFor(cfg conf.NodeConfig) string {
	identity := fmt.Sprintf("%s|%d", normalizeAPIHost(cfg.APIHost), cfg.NodeID)
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(s.dir, fmt.Sprintf("node-%d-%s.json", cfg.NodeID, hex.EncodeToString(sum[:8])))
}

func validateOfflineState(cfg conf.NodeConfig, state *offlineState) error {
	if state == nil {
		return errors.New("offline state is nil")
	}
	if state.Version != offlineStateVersion {
		return fmt.Errorf("unsupported offline state version %d", state.Version)
	}
	if state.APIHost != normalizeAPIHost(cfg.APIHost) || state.NodeID != cfg.NodeID {
		return errors.New("offline state identity mismatch")
	}
	if state.SavedAt <= 0 || state.NodeInfo == nil || state.NodeInfo.Id != cfg.NodeID {
		return errors.New("offline state node info is incomplete")
	}
	if state.NodeInfo.Common == nil || state.NodeInfo.Common.BaseConfig == nil || state.NodeInfo.Tag == "" || state.NodeInfo.Type == "" {
		return errors.New("offline state runtime config is incomplete")
	}
	if state.Users == nil || state.Alive == nil || state.DeviceAlive == nil || state.UserSyncSeq < 0 {
		return errors.New("offline state user data is incomplete")
	}
	return nil
}

func (s *offlineStateStore) Load(cfg conf.NodeConfig) (*offlineState, error) {
	data, err := os.ReadFile(s.pathFor(cfg))
	if err != nil {
		return nil, err
	}
	var state offlineState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode offline state: %w", err)
	}
	if err := validateOfflineState(cfg, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *offlineStateStore) Save(cfg conf.NodeConfig, state *offlineState) error {
	if err := validateOfflineState(cfg, state); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("create offline state directory: %w", err)
	}
	if err := os.Chmod(s.dir, 0700); err != nil {
		return fmt.Errorf("protect offline state directory: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode offline state: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".offline-state-*")
	if err != nil {
		return fmt.Errorf("create offline state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.pathFor(cfg)); err != nil {
		return fmt.Errorf("replace offline state: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: 格式化并运行全部快照存储测试**

Run: `gofmt -w node/offline_state.go node/offline_state_test.go; go test ./node -run 'TestOfflineStateStore' -count=1`

Expected: PASS。

- [ ] **Step 5: 提交快照存储**

```powershell
git add -- node/offline_state.go node/offline_state_test.go
git commit -m "功能：持久化节点最后成功快照" -m "按面板地址和节点编号隔离状态，使用权限收紧的临时文件原子替换保护最后可用数据。"
```

## Task 3: 用在线优先、快照兜底装载启动状态

**Files:**
- Create: `node/bootstrap.go`
- Create: `node/bootstrap_test.go`
- Modify: `node/node.go:13-35`
- Modify: `node/controller.go:18-111`
- Modify: `cmd/server.go:68,146`

- [ ] **Step 1: 写 bootstrap 离线回退测试**

在 `node/bootstrap_test.go` 定义只覆盖启动接口的 fake：

```go
type fakeBootstrapPanel struct {
	nodeInfo    *panel.NodeInfo
	users       []panel.UserInfo
	alive       map[int]int
	deviceAlive map[int]int
	err          error
	seq          int64
}

func (f *fakeBootstrapPanel) GetNodeInfo(context.Context) (*panel.NodeInfo, error) { return f.nodeInfo, f.err }
func (f *fakeBootstrapPanel) GetUserList(context.Context) ([]panel.UserInfo, error) { return f.users, f.err }
func (f *fakeBootstrapPanel) GetUserAlive(context.Context) (map[int]int, error) { return f.alive, f.err }
func (f *fakeBootstrapPanel) GetUserDeviceAlive(context.Context) (map[int]int, error) { return f.deviceAlive, f.err }
func (f *fakeBootstrapPanel) SetUserSyncSeq(seq int64) { f.seq = seq }
func (f *fakeBootstrapPanel) UserSyncSeq() int64 { return f.seq }

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
}

func TestLoadBootstrapStateFailsWithoutPanelOrSnapshot(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 1}
	client := &fakeBootstrapPanel{err: errors.New("panel unavailable")}
	if _, _, err := loadBootstrapState(context.Background(), client, cfg, nil); err == nil {
		t.Fatal("expected missing offline snapshot error")
	}
}
```

另加在线优先测试：fake 返回在线 `NodeInfo`、空但非 nil 的用户切片、空 map 和 `seq=99`，即使传入旧快照也要断言 `usedSnapshot=false`、结果序号为 99。

- [ ] **Step 2: 运行测试并确认 bootstrap 尚不存在**

Run: `go test ./node -run 'TestLoadBootstrapState' -count=1`

Expected: FAIL，编译错误包含 `undefined: loadBootstrapState`。

- [ ] **Step 3: 实现可测试的启动装载器**

`node/bootstrap.go` 使用以下接口和顺序：

```go
type bootstrapPanel interface {
	GetNodeInfo(context.Context) (*panel.NodeInfo, error)
	GetUserList(context.Context) ([]panel.UserInfo, error)
	GetUserAlive(context.Context) (map[int]int, error)
	GetUserDeviceAlive(context.Context) (map[int]int, error)
	SetUserSyncSeq(int64)
	UserSyncSeq() int64
}
```

`loadBootstrapState` 必须逐项在线读取；请求失败或返回 nil 时只能从同一有效快照取对应字段。使用快照用户时恢复 `UserSyncSeq`；空但非 nil 的在线用户列表是有效状态。返回状态中的切片和 map 必须复制，不能直接共享快照引用。

```go
func loadBootstrapState(ctx context.Context, client bootstrapPanel, cfg conf.NodeConfig, cached *offlineState) (*offlineState, bool, error) {
	state := &offlineState{
		Version: offlineStateVersion,
		APIHost: normalizeAPIHost(cfg.APIHost),
		NodeID: cfg.NodeID,
		SavedAt: time.Now().Unix(),
		DeviceAlive: map[int]int{},
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

	alive, err := client.GetUserAlive(ctx)
	if err != nil || alive == nil {
		if cached == nil || cached.Alive == nil {
			return nil, false, fmt.Errorf("get alive state and no offline snapshot: %w", nonNilError(err))
		}
		alive = cached.Alive
		usedSnapshot = true
	}
	state.Alive = cloneIntMap(alive)

	if info.Common != nil && info.Common.BaseConfig != nil && info.Common.BaseConfig.DeviceLimitByUUID {
		deviceAlive, err := client.GetUserDeviceAlive(ctx)
		if err != nil || deviceAlive == nil {
			if cached == nil || cached.DeviceAlive == nil {
				return nil, false, fmt.Errorf("get device alive state and no offline snapshot: %w", nonNilError(err))
			}
			deviceAlive = cached.DeviceAlive
			usedSnapshot = true
		}
		state.DeviceAlive = cloneIntMap(deviceAlive)
	}
	state.UserSyncSeq = client.UserSyncSeq()
	if usedSnapshot {
		state.SavedAt = cached.SavedAt
	}
	return state, usedSnapshot, nil
}
```

`nonNilError(nil)` 返回固定错误 `panel returned no data`；`cloneIntMap` 放在 `offline_state.go`，供 bootstrap 和 controller 共用。

```go
func nonNilError(err error) error {
	if err != nil {
		return err
	}
	return errors.New("panel returned no data")
}

func cloneIntMap(input map[int]int) map[int]int {
	output := make(map[int]int, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
```

- [ ] **Step 4: 把 bootstrap 接入启动链**

把 `node.New` 改为：

```go
func New(nodes []conf.NodeConfig, statePath string) (*Node, error)
```

为每个节点创建一个共享 `offlineStateStore`，先 `Load` 快照；`os.ErrNotExist` 只记 Debug，损坏快照记 Warning 后继续尝试在线数据。调用 `loadBootstrapState` 后，把完整状态和 `usedSnapshot` 传给 controller。

把 constructor 改为：

```go
func NewController(api *panel.Client, nodeConf *conf.NodeConfig, store *offlineStateStore, bootstrap *offlineState, startedOffline bool) *Controller
```

Controller 新增 `store`、`bootstrap`、`startedOffline` 字段。`Start` 不再发起启动网络请求，而是从 `bootstrap` 复制 `NodeInfo`、用户和两个在线 map；删除“用户数量必须大于零”的判断。代理内核和用户成功加入后：完全在线启动调用 `persistOfflineState(c.info)`；离线/混合启动先调用 `offlineTracker.Failure("sync", time.Unix(bootstrap.SavedAt, 0))`，再用当前时间调用一次以生成 24/72 小时阈值日志，同时保持旧快照不被混合状态覆盖。

`cmd/server.go` 两处调用统一改为：

```go
nodes, err := node.New(c.NodeConfigs, c.StatePath)
newNodes, err := node.New(newConf.NodeConfigs, newConf.StatePath)
```

- [ ] **Step 5: 运行 bootstrap 与现有 node 测试**

Run: `gofmt -w node/bootstrap.go node/bootstrap_test.go node/node.go node/controller.go cmd/server.go; go test ./node ./cmd -count=1`

Expected: PASS；若 `./cmd` 没有测试文件，应显示 `[no test files]` 而不是编译失败。

- [ ] **Step 6: 提交冷启动离线回退**

```powershell
git add -- node/bootstrap.go node/bootstrap_test.go node/node.go node/controller.go cmd/server.go
git commit -m "功能：支持面板失联时从快照冷启动" -m "启动时在线优先并逐项回退最后成功状态，无快照时保持明确失败边界。"
```

## Task 4: 持久化运行状态并跟踪离线与恢复

**Files:**
- Create: `node/offline_status.go`
- Create: `node/offline_status_test.go`
- Modify: `node/controller.go:18-45`
- Modify: `node/task.go:51-301`
- Modify: `node/user.go:16-197`
- Modify: `node/user_delta_test.go`

- [ ] **Step 1: 写离线告警状态机测试**

```go
func TestOfflineTrackerEmitsThresholdsOnceAndResets(t *testing.T) {
	var tracker offlineTracker
	start := time.Unix(1_700_000_000, 0)
	if got := tracker.Failure("sync", start); !got.Entered {
		t.Fatalf("first failure did not enter offline mode: %+v", got)
	}
	tracker.Failure("report", start.Add(time.Hour))
	if got := tracker.Failure("sync", start.Add(24*time.Hour)); !got.Warn24h || got.Warn72h {
		t.Fatalf("24h transition=%+v", got)
	}
	if got := tracker.Failure("sync", start.Add(72*time.Hour)); got.Warn24h || !got.Warn72h {
		t.Fatalf("72h transition=%+v", got)
	}
	if got := tracker.Failure("sync", start.Add(96*time.Hour)); got.Entered || got.Warn24h || got.Warn72h {
		t.Fatalf("repeated failure should be quiet: %+v", got)
	}
	if _, recovered := tracker.Success("sync", start.Add(97*time.Hour)); recovered {
		t.Fatal("sync recovery must wait for report component")
	}
	duration, recovered := tracker.Success("report", start.Add(97*time.Hour))
	if !recovered || duration != 97*time.Hour {
		t.Fatalf("recovery duration=%v recovered=%v", duration, recovered)
	}
	if got := tracker.Failure("sync", start.Add(98*time.Hour)); !got.Entered {
		t.Fatal("tracker did not reset after recovery")
	}
}
```

- [ ] **Step 2: 运行测试并确认状态机尚不存在**

Run: `go test ./node -run 'TestOfflineTracker' -count=1`

Expected: FAIL，编译错误包含 `undefined: offlineTracker`。

- [ ] **Step 3: 实现线程安全且限频的离线状态机**

`node/offline_status.go` 定义：

```go
type offlineTransition struct {
	Entered bool
	Warn24h bool
	Warn72h bool
	Elapsed time.Duration
}

type offlineTracker struct {
	mu        sync.Mutex
	since     time.Time
	warned24h bool
	warned72h bool
	failed    map[string]struct{}
}
```

`Failure(component, now)` 在首次失败设置 `since`，跨越 24/72 小时时各返回一次标记；`Success(component, now)` 只清除对应组件，所有失败组件都恢复后才返回持续时间并重置离线状态。组件固定使用 `sync` 和 `report`，避免一个成功任务掩盖另一个持续失败任务。任何路径都不得设置停服或过期标记。

```go
func (t *offlineTracker) Failure(component string, now time.Time) offlineTransition {
	t.mu.Lock()
	defer t.mu.Unlock()
	transition := offlineTransition{}
	if t.failed == nil {
		t.failed = make(map[string]struct{})
	}
	t.failed[component] = struct{}{}
	if t.since.IsZero() {
		t.since = now
		transition.Entered = true
	}
	transition.Elapsed = now.Sub(t.since)
	if transition.Elapsed < 0 {
		transition.Elapsed = 0
	}
	if transition.Elapsed >= 24*time.Hour && !t.warned24h {
		t.warned24h = true
		transition.Warn24h = true
	}
	if transition.Elapsed >= 72*time.Hour && !t.warned72h {
		t.warned72h = true
		transition.Warn72h = true
	}
	return transition
}

func (t *offlineTracker) Success(component string, now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failed, component)
	if t.since.IsZero() {
		return 0, false
	}
	if len(t.failed) != 0 {
		return 0, false
	}
	duration := now.Sub(t.since)
	if duration < 0 {
		duration = 0
	}
	t.since = time.Time{}
	t.warned24h = false
	t.warned72h = false
	t.failed = nil
	return duration, true
}
```

离线快照启动时，以快照 `SavedAt` 作为 `since`，再用当前时间计算阈值；因此进程重启不会把已经离线 72 小时的节点重新当作刚刚离线。

- [ ] **Step 4: 写运行中面板失败不改变用户的回归测试**

在 `node/user_delta_test.go` 增加使用 `httptest.Server` 的测试。服务器所有请求返回 503，controller 初始用户为 `cached-user`，调用 `syncUserState` 后必须返回错误且用户保持不变：

```go
func TestSyncUserStateKeepsUsersWhenPanelUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "panel unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := conf.NodeConfig{APIHost: server.URL, NodeID: 1, Key: "test"}
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
```

Run: `go test ./node -run 'TestSyncUserStateKeepsUsersWhenPanelUnavailable' -count=1`

Expected: FAIL，旧实现吞掉面板错误，测试报告 `expected panel error`。

- [ ] **Step 5: 写快照推进和旧快照保护的失败测试**

增加 `TestPersistOfflineStateCapturesControllerState`：使用 `t.TempDir()` 的 store 构造 controller，设置两名用户和同步序号 55，调用 `persistOfflineState` 后 Load 并断言完整内容。再增加 `TestPersistOfflineStateFailureKeepsLastGoodFile`：先保存序号 55，把下一次 store 目录指向一个普通文件使保存失败，再从原目录 Load 并断言旧序号仍为 55。

Run: `go test ./node -run 'TestPersistOfflineState' -count=1`

Expected: FAIL，编译错误包含 `persistOfflineState undefined`。

- [ ] **Step 6: 让同步错误向上返回但保持已生效状态**

修改 `syncUserState`：增量接口失败仍可回退完整列表；完整列表也失败时返回包装错误，不再 `return nil`。修改 `refreshAliveState`：任一所需在线统计请求失败都直接返回错误，并且在全部请求成功前不写 `aliveMap`、`deviceAliveMap` 或 limiter，避免混合半套状态。

在 `nodeInfoMonitor` 中：

1. 配置请求或 `syncUserState` 失败时调用 `recordPanelFailure("sync", operation, err)` 并返回错误。
2. 完整同步成功时调用 `recordPanelSuccess("sync")`，然后调用 `persistOfflineState(c.info)`。
3. 保存失败只记错误，不关闭数据面。
4. 收到新 `NodeInfo` 时先放入 `pendingNodeInfo`，成功保存 `persistOfflineState(newInfo)` 后才发送 reload；保存失败则保留 pending，并在下个周期先重试保存，避免 ETag 已推进后丢失变更。

Controller 的保存方法按当前已生效状态构造完整快照：

```go
func (c *Controller) persistOfflineState(info *panel.NodeInfo) error {
	state := &offlineState{
		Version:       offlineStateVersion,
		APIHost:       normalizeAPIHost(c.conf.APIHost),
		NodeID:        c.conf.NodeID,
		SavedAt:       time.Now().Unix(),
		NodeInfo:      info,
		Users:         append([]panel.UserInfo(nil), c.userList...),
		Alive:         cloneIntMap(c.aliveMap),
		DeviceAlive:   cloneIntMap(c.deviceAliveMap),
		UserSyncSeq:   c.apiClient.UserSyncSeq(),
	}
	return c.store.Save(*c.conf, state)
}
```

`recordPanelFailure` 只在 `Entered/Warn24h/Warn72h` 为真时输出日志；日志字段只能包含节点 tag、操作名、错误和离线时长。`recordPanelSuccess` 只有在 `sync` 与 `report` 都恢复后才输出一次恢复日志，不输出用户、IP、Token 或快照内容。

修改 `node/user.go`，用 `reportErr` 保存本轮最后一个面板上报错误。流量、设备流量、在线用户、在线设备或运行状态上报失败时继续完成本地清理和其他安全步骤，最后调用 `recordPanelFailure("report", "traffic report", reportErr)` 并返回该错误；本轮没有面板错误时调用 `recordPanelSuccess("report")`。把 `reportNodeRuntimeStatus` 改为返回 `error`，本地网卡采样失败仍只记 Debug 并返回 nil，`ReportNodeRuntimeStatus` 失败才向上传递。

- [ ] **Step 7: 运行离线状态、同步失败和持久化测试**

Run: `gofmt -w node/offline_status.go node/offline_status_test.go node/controller.go node/task.go node/user.go node/user_delta_test.go; go test ./node -count=1`

Expected: PASS。

- [ ] **Step 8: 提交运行时离线保活**

```powershell
git add -- node/offline_status.go node/offline_status_test.go node/controller.go node/task.go node/user.go node/user_delta_test.go
git commit -m "修复：面板失联时保留节点运行状态" -m "同步失败不修改用户或在线限制，恢复后再应用最新数据并刷新最后成功快照。"
```

## Task 5: 禁止面板任务超时触发破坏性 reload

**Files:**
- Modify: `common/task/task.go:12-98`
- Modify: `common/task/task_test.go`
- Modify: `node/task.go:14-48`

- [ ] **Step 1: 写超时策略回归测试**

```go
func TestTaskTimeoutDoesNotReloadUnlessEnabled(t *testing.T) {
	reloadCh := make(chan struct{}, 1)
	periodic := &Task{
		Name:     "panel-sync",
		Interval: 5 * time.Millisecond,
		Execute: func(context.Context) error {
			time.Sleep(100 * time.Millisecond)
			return nil
		},
		ReloadCh: reloadCh,
	}
	if err := periodic.ExecuteWithTimeout(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline exceeded", err)
	}
	select {
	case <-reloadCh:
		t.Fatal("panel task timeout triggered reload")
	default:
	}
}

func TestTaskTimeoutReloadsWhenExplicitlyEnabled(t *testing.T) {
	reloadCh := make(chan struct{}, 1)
	periodic := &Task{
		Name:            "certificate-recovery",
		Interval:        5 * time.Millisecond,
		ReloadOnTimeout: true,
		Execute: func(context.Context) error {
			time.Sleep(100 * time.Millisecond)
			return nil
		},
		ReloadCh: reloadCh,
	}
	if err := periodic.ExecuteWithTimeout(); err != nil {
		t.Fatalf("ExecuteWithTimeout error=%v", err)
	}
	select {
	case <-reloadCh:
	default:
		t.Fatal("explicit reload policy did not signal reload")
	}
}
```

- [ ] **Step 2: 运行测试并确认策略字段缺失**

Run: `go test ./common/task -run 'TestTaskTimeout' -count=1`

Expected: FAIL，编译错误包含 `ReloadOnTimeout undefined`。

- [ ] **Step 3: 实现显式超时策略**

在 `Task` 增加：

```go
ReloadOnTimeout bool
```

把 `ExecuteWithTimeout` 的 `ctx.Done()` 分支改为：

```go
case <-ctx.Done():
	if !t.ReloadOnTimeout {
		log.Warningf("Task %s execution timed out, keeping current runtime", t.Name)
		return ctx.Err()
	}
	log.Errorf("Task %s execution timed out, reloading", t.Name)
	if t.ReloadCh == nil {
		return errors.New("reload channel is nil")
	}
	select {
	case t.ReloadCh <- struct{}{}:
	default:
	}
	return nil
```

不再因 `ReloadCh == nil` 调用 `log.Panic`。

- [ ] **Step 4: 为三类现有任务显式赋值**

`nodeInfoMonitor` 和 `reportUserTrafficTask` 设置 `ReloadOnTimeout: false`；`renewCertTask` 设置 `ReloadOnTimeout: true`，保留证书恢复任务原有行为。

- [ ] **Step 5: 运行任务和节点测试**

Run: `gofmt -w common/task/task.go common/task/task_test.go node/task.go; go test ./common/task ./node -count=1`

Expected: PASS，且无 panic。

- [ ] **Step 6: 提交超时隔离修复**

```powershell
git add -- common/task/task.go common/task/task_test.go node/task.go
git commit -m "修复：阻止面板任务超时重载节点" -m "面板同步和流量上报超时后继续重试，只有显式配置的恢复任务才允许触发 reload。"
```

## Task 6: 补充运维说明和故障经验记录

**Files:**
- Create: `docs/panel-offline-mode.md`
- Create: `LESSONS_LEARNED.md`（如果执行时已经存在，则在文件末尾追加同结构条目）

- [ ] **Step 1: 写离线模式运维说明**

文档必须包含以下可直接执行的检查命令和结论：

````markdown
# v2node 面板失联离线模式

## 行为

- 面板不可达时继续使用最后成功快照，无固定停服期限。
- 24/72 小时只告警，不删除用户、不踢连接、不触发 reload。
- 面板恢复后自动同步最新用户与限制。
- 首次安装没有成功快照时不能离线启动。

## 状态目录

默认目录：`/etc/v2node/offline-state`

```bash
sudo ls -lah /etc/v2node/offline-state
sudo stat -c '%a %U:%G %n' /etc/v2node/offline-state/*.json
```

快照文件应为 `600`。不要把内容粘贴到工单或聊天，因为文件可能包含节点私密配置。

## Docker

容器必须持久化整个状态目录；只挂载单个 `config.json` 无法保证容器重建后保留快照：

```yaml
volumes:
  - ./v2node-config:/etc/v2node
```

## 故障检查

```bash
systemctl status v2node --no-pager
journalctl -u v2node --since '30 min ago' --no-pager | grep -E 'offline|snapshot|panel|recovered'
```

损坏快照会被拒绝加载但不会自动删除。先保留文件排查，再在面板恢复并成功生成新快照后清理旧文件。
````

- [ ] **Step 2: 写经验记录**

`LESSONS_LEARNED.md` 条目必须包含：症状、链路、根因、修复、验证、下次检查入口、相关文件与命令。不得记录真实面板域名、用户 UUID、IP、Token 或私密快照内容。

- [ ] **Step 3: 检查文档和提交**

Run: `git diff --check; rg -n "面板失联|offline-state|ReloadOnTimeout|go test" docs/panel-offline-mode.md LESSONS_LEARNED.md`

Expected: `git diff --check` 无输出；搜索结果覆盖离线行为、状态目录、超时策略和验证命令。

```powershell
git add -- docs/panel-offline-mode.md LESSONS_LEARNED.md
git commit -m "文档：补充节点离线模式运维说明" -m "记录状态目录、Docker 持久化、故障检查和控制面失联经验。"
```

## Task 7: 全量验证与变更复核

**Files:**
- Review: `conf/conf.go`
- Review: `node/offline_state.go`
- Review: `node/bootstrap.go`
- Review: `node/controller.go`
- Review: `node/task.go`
- Review: `common/task/task.go`
- Review: `cmd/server.go`
- Review: `docs/panel-offline-mode.md`
- Review: `LESSONS_LEARNED.md`

- [ ] **Step 1: 格式化所有触及的 Go 文件**

Run: `gofmt -w conf/conf.go conf/conf_test.go node/offline_state.go node/offline_state_test.go node/bootstrap.go node/bootstrap_test.go node/offline_status.go node/offline_status_test.go node/node.go node/controller.go node/task.go node/user.go node/user_delta_test.go common/task/task.go common/task/task_test.go cmd/server.go`

Expected: exit 0。

- [ ] **Step 2: 运行目标测试**

Run: `go test ./conf ./common/task ./node -count=1`

Expected: 三个包全部 PASS。

- [ ] **Step 3: 运行仓库 Go 测试**

Run: `go test ./... -count=1`

Expected: exit 0，所有包 PASS 或显示 `[no test files]`，不得出现 FAIL、panic 或 data race 报告。

- [ ] **Step 4: 检查补丁卫生和范围**

Run: `git diff --check; git status --short; git log --oneline -8`

Expected: `git diff --check` 无输出；工作区只包含计划内文件；最近提交均为本计划的中文标题和正文。

- [ ] **Step 5: 按业务链人工复核**

逐项确认：

1. `cmd/server.go -> node.New -> loadBootstrapState -> Controller.Start` 在面板失败且有快照时能组成完整运行状态。
2. 首次无快照仍明确失败，没有创建空授权或空节点配置。
3. `nodeInfoMonitor -> syncUserState -> persistOfflineState` 只有完整成功才刷新快照和同步序号。
4. 失败路径没有调用 `applyUserList`、`DelUsers`、`DelNode` 或 reload。
5. 新节点配置只有在快照保存成功后才发送 reload。
6. 面板任务超时不 reload；证书恢复任务仍可显式 reload。
7. 24/72 小时只有日志行为，没有快照过期或停服分支。
8. 快照日志不包含 API Key、用户 UUID、IP、Token 或配置正文。

- [ ] **Step 6: 如最终复核产生修正，单独提交**

只有实际产生修正时执行：

```powershell
git add -- <本次修正的计划内文件>
git commit -m "修复：完善节点离线模式边界" -m "根据全量测试和调用链复核修正离线启动或恢复同步边界。"
```

没有修正时不要创建空提交。
