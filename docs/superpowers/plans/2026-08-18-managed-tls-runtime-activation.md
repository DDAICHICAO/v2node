# Managed TLS Runtime Activation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让托管证书的新版本只有在对应 NodeID 的运行时入站真正加载成功后才进入迁移准备状态，并在不重启整个 v2node 的情况下完成后续证书更新。

**Architecture:** `managedTLSManager` 分开记录证书落盘版本和运行时激活版本，并按版本重复触发激活回调。`Controller` 串行化证书激活与用户同步，调用 `V2Core.ReplaceNode` 仅替换当前 tag 的入站；Core 在移除旧入站前准备好候选与回滚入站及完整用户快照，失败时立即恢复旧配置。v2board 的真实 TLS 入口探测继续作为最终切换门禁。

**Tech Stack:** Go、Xray Core inbound manager、现有 managed TLS 文件存储、Go `testing`、PowerShell、Git。

---

## 文件结构

- Create: `node/managed_tls_runtime_activation_test.go` — 管理器版本门禁和 Controller 激活回归测试。
- Create: `core/inbound_reload.go` — 候选入站准备、单 tag 替换和旧入站恢复。
- Create: `core/inbound_reload_test.go` — 单入站替换顺序与回滚测试。
- Modify: `node/managed_tls_manager.go` — 用 activated version 替代一次性 ready 通知。
- Modify: `node/controller.go` — 记录运行时证书版本并调用单入站替换。
- Modify: `core/inbound.go` — 拆出只构建、不注册的 inbound handler 工厂。
- Modify: `core/user.go` — 复用用户协议对象构建和 handler 级用户安装。
- Modify: `core/node.go` — 暴露 `ReplaceNode` 入口。
- Modify: `D:/常用项目/v2board/LESSONS_LEARNED.md` — 记录“落盘成功不等于运行时证书生效”的故障链。

### Task 1: 按证书版本控制迁移准备状态

**Files:**
- Create: `node/managed_tls_runtime_activation_test.go`
- Modify: `node/managed_tls_manager.go`

- [ ] **Step 1: 写入失败测试，证明落盘后不能立即上报 prepared**

```go
package node

import (
	"context"
	"errors"
	"testing"
)

func migrationCertificateForActivationTest(version uint64) *managedTLSLocalCertificate {
	return &managedTLSLocalCertificate{Metadata: managedTLSMetadata{
		ScopeID: 1,
		Domain: "old.example.com",
		Domains: []string{"new.example.com", "old.example.com"},
		MigrationID: 9,
		Version: version,
		NotAfter: 2_000_000_000,
	}}
}

func TestManagedTLSReadyNotificationTracksActivatedVersion(t *testing.T) {
	m := &managedTLSManager{
		domain: "old.example.com",
		snapshot: managedTLSStatusSnapshot{Managed: true},
	}
	m.setFromLocal(managedTLSReady, migrationCertificateForActivationTest(1), "")
	if m.Snapshot().MigrationPrepared {
		t.Fatal("certificate must not be prepared before runtime activation")
	}

	calls := 0
	m.notifyReady(context.Background(), func() error {
		calls++
		return nil
	})
	if !m.Snapshot().MigrationPrepared {
		t.Fatal("certificate must be prepared after runtime activation")
	}

	m.setFromLocal(managedTLSReady, migrationCertificateForActivationTest(2), "")
	if m.Snapshot().MigrationPrepared {
		t.Fatal("new installed version must wait for runtime activation")
	}
	m.notifyReady(context.Background(), func() error {
		calls++
		return nil
	})
	m.notifyReady(context.Background(), func() error {
		calls++
		return nil
	})
	if calls != 2 {
		t.Fatalf("activation callbacks = %d, want 2", calls)
	}
}

func TestManagedTLSReadyNotificationRetriesFailedActivation(t *testing.T) {
	m := &managedTLSManager{
		domain: "old.example.com",
		snapshot: managedTLSStatusSnapshot{Managed: true},
	}
	m.setFromLocal(managedTLSReady, migrationCertificateForActivationTest(3), "")

	attempts := 0
	m.notifyReady(context.Background(), func() error {
		attempts++
		return errors.New("activate failed")
	})
	if m.Snapshot().MigrationPrepared {
		t.Fatal("failed activation must keep migration gate closed")
	}
	m.setFromLocal(managedTLSReady, migrationCertificateForActivationTest(3), "")
	m.notifyReady(context.Background(), func() error {
		attempts++
		return nil
	})
	if attempts != 2 || !m.Snapshot().MigrationPrepared {
		t.Fatalf("attempts=%d prepared=%v", attempts, m.Snapshot().MigrationPrepared)
	}
}
```

- [ ] **Step 2: 运行目标测试并确认 RED**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLSReadyNotification' -count=1
```

Expected: FAIL，第一条测试显示当前 `MigrationPrepared` 在 `setFromLocal(...ready...)` 后已经错误地变为 `true`，或新版本未再次调用回调。

- [ ] **Step 3: 用 activated version 替代一次性布尔通知**

在 `managedTLSManager` 中删除 `readyNotified bool`，增加：

```go
activatedVersion uint64
```

增加锁内辅助函数，并从 `setFromLocal` 和成功回调处调用：

```go
func (m *managedTLSManager) refreshMigrationPreparedLocked() {
	m.snapshot.MigrationPrepared =
		m.snapshot.Status == managedTLSReady &&
		m.snapshot.MigrationID > 0 &&
		m.snapshot.PreparedVersion == m.snapshot.Version &&
		m.activatedVersion == m.snapshot.Version
}
```

`setFromLocal` 只记录迁移元数据，不再以 `status == managedTLSReady` 直接放行：

```go
if err == nil && local.Metadata.MigrationID > 0 && managedTLSDomainsContain(domains, m.domain) {
	m.snapshot.MigrationID = local.Metadata.MigrationID
	m.snapshot.PreparedVersion = local.Metadata.Version
}
m.refreshMigrationPreparedLocked()
```

将 `notifyReady` 改为按当前 snapshot version 去重，失败时保留未激活状态，成功时推进版本：

```go
func (m *managedTLSManager) notifyReady(ctx context.Context, callback func() error) {
	if callback == nil || ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	version := m.snapshot.Version
	if version == 0 || m.activatedVersion == version || m.closed {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	if ctx.Err() != nil {
		return
	}
	if err := callback(); err != nil {
		m.mu.Lock()
		if !m.closed && m.snapshot.Version == version {
			m.snapshot.Status = managedTLSDegraded
			m.snapshot.LastErrorCode = "managed_tls_runtime_activation_failed"
			m.refreshMigrationPreparedLocked()
		}
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	if !m.closed && m.snapshot.Version == version {
		m.activatedVersion = version
		m.snapshot.Status = managedTLSReady
		m.snapshot.LastErrorCode = ""
		m.refreshMigrationPreparedLocked()
	}
	m.mu.Unlock()
}
```

- [ ] **Step 4: 运行测试并确认 GREEN**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLSReadyNotification' -count=1
```

Expected: PASS，2 tests passed。

- [ ] **Step 5: 提交版本门禁改动**

```powershell
git add -- node/managed_tls_manager.go node/managed_tls_runtime_activation_test.go
git commit -m "修复：按运行时版本确认托管证书就绪" -m "区分证书落盘版本与运行时激活版本。`n`n激活失败时保持域名迁移门禁关闭，并允许相同版本后续重试。"
```

### Task 2: 准备候选入站并提供可回滚的单 tag 替换

**Files:**
- Create: `core/inbound_reload.go`
- Create: `core/inbound_reload_test.go`
- Modify: `core/inbound.go`
- Modify: `core/user.go`
- Modify: `core/node.go`

- [ ] **Step 1: 写入失败测试，固定替换和恢复顺序**

```go
package core

import (
	"errors"
	"reflect"
	"testing"

	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/features/inbound"
)

type reloadTestHandler struct{ tag string }

func (h *reloadTestHandler) Start() error { return nil }
func (h *reloadTestHandler) Close() error { return nil }
func (h *reloadTestHandler) Tag() string { return h.tag }
func (h *reloadTestHandler) ReceiverSettings() *serial.TypedMessage { return nil }
func (h *reloadTestHandler) ProxySettings() *serial.TypedMessage { return nil }

func TestSwapPreparedInboundSuccess(t *testing.T) {
	candidate := &reloadTestHandler{tag: "node-302"}
	rollback := &reloadTestHandler{tag: "node-302"}
	var calls []string
	err := swapPreparedInbound("node-302", candidate, rollback, inboundSwapHooks{
		remove: func(tag string) error { calls = append(calls, "remove:"+tag); return nil },
		add: func(handler inbound.Handler) error { calls = append(calls, "add:candidate"); return nil },
		close: func(handler inbound.Handler) error { calls = append(calls, "close:rollback"); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"remove:node-302", "add:candidate", "close:rollback"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}

func TestSwapPreparedInboundRestoresRollbackAfterCandidateFailure(t *testing.T) {
	candidate := &reloadTestHandler{tag: "node-302"}
	rollback := &reloadTestHandler{tag: "node-302"}
	var calls []string
	addCalls := 0
	err := swapPreparedInbound("node-302", candidate, rollback, inboundSwapHooks{
		remove: func(tag string) error {
			if addCalls == 0 {
				calls = append(calls, "remove:old")
			} else {
				calls = append(calls, "remove:failed-candidate")
			}
			return nil
		},
		add: func(handler inbound.Handler) error {
			addCalls++
			if addCalls == 1 {
				calls = append(calls, "add:candidate")
				return errors.New("candidate failed")
			}
			calls = append(calls, "add:rollback")
			return nil
		},
		close: func(handler inbound.Handler) error { calls = append(calls, "close:orphan"); return nil },
	})
	if err == nil {
		t.Fatal("expected candidate activation error")
	}
	want := []string{"remove:old", "add:candidate", "remove:failed-candidate", "add:rollback"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}
```

- [ ] **Step 2: 运行 Core 目标测试并确认 RED**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core -run 'TestSwapPreparedInbound' -count=1
```

Expected: build FAIL，`swapPreparedInbound` 和 `inboundSwapHooks` 尚不存在。

- [ ] **Step 3: 拆出 handler 构建和注册函数**

在 `core/inbound.go` 中把当前 `addInbound` 拆成：

```go
func (v *V2Core) createInboundHandler(config *core.InboundHandlerConfig) (inbound.Handler, error) {
	rawHandler, err := core.CreateObject(v.Server, config)
	if err != nil {
		return nil, err
	}
	handler, ok := rawHandler.(inbound.Handler)
	if !ok {
		return nil, errors.New("created object is not an inbound handler")
	}
	return handler, nil
}

func (v *V2Core) addInboundHandler(handler inbound.Handler) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return v.ihm.AddHandler(ctx, handler)
}

func (v *V2Core) addInbound(config *core.InboundHandlerConfig) error {
	handler, err := v.createInboundHandler(config)
	if err != nil {
		return err
	}
	return v.addInboundHandler(handler)
}
```

- [ ] **Step 4: 抽取 handler 级用户准备函数**

在 `core/user.go` 中增加并让现有 `AddUsers` 复用：

```go
func buildCoreUsers(p *AddUsersParams) ([]*protocol.User, error) {
	switch p.NodeInfo.Type {
	case "vmess":
		return buildVmessUsers(p.Tag, p.Users), nil
	case "vless":
		return buildVlessUsers(p.Tag, p.Users, p.Common.Flow), nil
	case "trojan":
		return buildTrojanUsers(p.Tag, p.Users), nil
	case "shadowsocks":
		return buildSSUsers(p.Tag, p.Users, p.Common.Cipher, p.Common.ServerKey), nil
	case "hysteria2":
		return buildHysteria2Users(p.Tag, p.Users), nil
	case "tuic":
		return buildTuicUsers(p.Tag, p.Users), nil
	case "anytls":
		return buildAnyTLSUsers(p.Tag, p.Users), nil
	default:
		return nil, fmt.Errorf("unsupported node type: %s", p.NodeInfo.Type)
	}
}

func inboundUserManager(handler inbound.Handler, tag string) (proxy.UserManager, error) {
	provider, ok := handler.(proxy.GetInbound)
	if !ok {
		return nil, fmt.Errorf("handler %s does not expose inbound", tag)
	}
	manager, ok := provider.GetInbound().(proxy.UserManager)
	if !ok {
		return nil, fmt.Errorf("handler %s does not implement user manager", tag)
	}
	return manager, nil
}
```

`AddUsers` 先调用 `buildCoreUsers`，再调用现有 `addManagedUsers`；原有 UID map、link activation 和回滚行为保持不变。

- [ ] **Step 5: 实现候选/回滚入站准备和 swap helper**

创建 `core/inbound_reload.go`：

```go
package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	xraycore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
)

type inboundSwapHooks struct {
	remove func(string) error
	add func(inbound.Handler) error
	close func(inbound.Handler) error
}

func swapPreparedInbound(tag string, candidate, rollback inbound.Handler, hooks inboundSwapHooks) error {
	if err := hooks.remove(tag); err != nil {
		_ = hooks.close(candidate)
		_ = hooks.close(rollback)
		return fmt.Errorf("remove current inbound: %w", err)
	}
	if err := hooks.add(candidate); err == nil {
		_ = hooks.close(rollback)
		return nil
	} else {
		cleanupErr := hooks.remove(tag)
		if cleanupErr != nil {
			_ = hooks.close(candidate)
		}
		if rollbackErr := hooks.add(rollback); rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("activate candidate inbound: %w", err),
				cleanupErr,
				fmt.Errorf("restore previous inbound: %w", rollbackErr),
			)
		}
		return errors.Join(
			fmt.Errorf("activate candidate inbound: %w", err),
			cleanupErr,
		)
	}
}

func (v *V2Core) prepareNodeInbound(tag string, info *panel.NodeInfo, users []panel.UserInfo) (inbound.Handler, error) {
	config, err := buildInbound(info, tag)
	if err != nil {
		return nil, fmt.Errorf("build inbound: %w", err)
	}
	return v.prepareInboundFromConfig(config, &AddUsersParams{Tag: tag, Users: users, NodeInfo: info})
}

func (v *V2Core) prepareInboundFromConfig(config *xraycore.InboundHandlerConfig, params *AddUsersParams) (inbound.Handler, error) {
	handler, err := v.createInboundHandler(config)
	if err != nil {
		return nil, err
	}
	manager, err := inboundUserManager(handler, params.Tag)
	if err != nil {
		_ = handler.Close()
		return nil, err
	}
	users, err := buildCoreUsers(params)
	if err != nil {
		_ = handler.Close()
		return nil, err
	}
	if _, err := v.addManagedUsers(manager, params.Tag, params.Users, users); err != nil {
		_ = handler.Close()
		return nil, err
	}
	return handler, nil
}

func (v *V2Core) currentInboundSnapshot(tag string) (*xraycore.InboundHandlerConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handler, err := v.ihm.GetHandler(ctx, tag)
	if err != nil {
		return nil, err
	}
	return &xraycore.InboundHandlerConfig{
		Tag: tag,
		ReceiverSettings: handler.ReceiverSettings(),
		ProxySettings: handler.ProxySettings(),
	}, nil
}
```

在 `core/node.go` 增加：

```go
func (v *V2Core) ReplaceNode(tag string, info *panel.NodeInfo, users []panel.UserInfo) error {
	if isSntpEclipseNode(info) || isMieruNode(info) {
		return errors.New("managed TLS runtime replacement is unsupported for this node type")
	}
	rollbackConfig, err := v.currentInboundSnapshot(tag)
	if err != nil {
		return fmt.Errorf("snapshot current inbound: %w", err)
	}
	candidate, err := v.prepareNodeInbound(tag, info, users)
	if err != nil {
		return fmt.Errorf("prepare candidate inbound: %w", err)
	}
	rollback, err := v.prepareInboundFromConfig(rollbackConfig, &AddUsersParams{Tag: tag, Users: users, NodeInfo: info})
	if err != nil {
		_ = candidate.Close()
		return fmt.Errorf("prepare rollback inbound: %w", err)
	}
	return swapPreparedInbound(tag, candidate, rollback, inboundSwapHooks{
		remove: v.removeInbound,
		add: v.addInboundHandler,
		close: func(handler inbound.Handler) error { return handler.Close() },
	})
}
```

- [ ] **Step 6: 运行 Core 测试并确认 GREEN**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core -run 'TestSwapPreparedInbound' -count=1
go test ./core -count=1
```

Expected: PASS；候选失败测试确认执行了 rollback add，且没有使用全局 reload channel。

- [ ] **Step 7: 提交单入站替换能力**

```powershell
git add -- core/inbound_reload.go core/inbound_reload_test.go core/inbound.go core/user.go core/node.go
git commit -m "功能：支持托管证书单入站热替换" -m "在移除旧监听前准备候选与回滚入站及完整用户快照。`n`n候选启动失败时恢复旧入站，不触发整个 v2node 配置重载。"
```

### Task 3: Controller 激活新版本并与用户同步串行化

**Files:**
- Modify: `node/controller.go`
- Modify: `node/managed_tls_runtime_activation_test.go`

- [ ] **Step 1: 写入失败测试，固定单 NodeID 激活行为**

把 `panel "github.com/wyx2685/v2node/api/v2board"` 加入测试文件 import，并追加：

```go
func TestControllerActivatesOnlyChangedManagedTLSVersion(t *testing.T) {
	info := &panel.NodeInfo{Tag: "node-302"}
	users := []panel.UserInfo{{Id: 1, Uuid: "user-1"}}
	c := &Controller{
		info: info,
		tag: info.Tag,
		userList: append([]panel.UserInfo(nil), users...),
		managedTLSRuntimeVersion: 1,
	}
	c.runtime.started = true

	calls := 0
	c.replaceManagedTLSInbound = func(tag string, gotInfo *panel.NodeInfo, gotUsers []panel.UserInfo) error {
		calls++
		if tag != "node-302" || gotInfo != info || len(gotUsers) != 1 || gotUsers[0].Uuid != "user-1" {
			t.Fatalf("unexpected replacement input: tag=%s users=%v", tag, gotUsers)
		}
		return nil
	}

	if err := c.activateManagedTLSRuntimeVersion(2); err != nil {
		t.Fatal(err)
	}
	if err := c.activateManagedTLSRuntimeVersion(2); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || c.managedTLSRuntimeVersion != 2 {
		t.Fatalf("calls=%d active=%d", calls, c.managedTLSRuntimeVersion)
	}
}

func TestControllerKeepsPreviousVersionWhenReplacementFails(t *testing.T) {
	c := &Controller{
		info: &panel.NodeInfo{Tag: "node-302"},
		tag: "node-302",
		managedTLSRuntimeVersion: 4,
		replaceManagedTLSInbound: func(string, *panel.NodeInfo, []panel.UserInfo) error {
			return errors.New("replace failed")
		},
	}
	c.runtime.started = true
	if err := c.activateManagedTLSRuntimeVersion(5); err == nil {
		t.Fatal("expected replacement failure")
	}
	if c.managedTLSRuntimeVersion != 4 {
		t.Fatalf("active version=%d want=4", c.managedTLSRuntimeVersion)
	}
}
```

- [ ] **Step 2: 运行 Controller 测试并确认 RED**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestController(Activates|Keeps)' -count=1
```

Expected: build FAIL，Controller 尚无运行时版本字段、替换函数和激活方法。

- [ ] **Step 3: 实现 Controller 激活状态和串行化**

在 `Controller` 增加：

```go
managedTLSActivationMu sync.Mutex
managedTLSRuntimeVersion uint64
replaceManagedTLSInbound func(string, *panel.NodeInfo, []panel.UserInfo) error
```

增加方法：

```go
func (c *Controller) activateManagedTLSRuntime() error {
	if c.managedTLS == nil {
		return errors.New("managed TLS manager is nil")
	}
	return c.activateManagedTLSRuntimeVersion(c.managedTLS.Snapshot().Version)
}

func (c *Controller) activateManagedTLSRuntimeVersion(version uint64) error {
	if version == 0 {
		return errors.New("managed TLS runtime version is empty")
	}
	c.managedTLSActivationMu.Lock()
	defer c.managedTLSActivationMu.Unlock()

	if !c.runtime.Started() {
		if err := c.startRuntime(); err != nil {
			return err
		}
		c.managedTLSRuntimeVersion = version
		c.stopManagedTLSStatusReporter()
		return nil
	}
	if c.managedTLSRuntimeVersion == version {
		return nil
	}

	c.stateMu.Lock()
	users := append([]panel.UserInfo(nil), c.userList...)
	replace := c.replaceManagedTLSInbound
	if replace == nil {
		replace = c.server.ReplaceNode
	}
	err := replace(c.tag, c.info, users)
	c.stateMu.Unlock()
	if err != nil {
		return fmt.Errorf("activate managed TLS runtime version %d: %w", version, err)
	}
	c.managedTLSRuntimeVersion = version
	nodeID := 0
	if c.conf != nil {
		nodeID = c.conf.NodeID
	}
	scopeID := uint64(0)
	if c.managedTLS != nil {
		scopeID = c.managedTLS.Snapshot().ScopeID
	}
	log.WithFields(log.Fields{
		"tag": c.tag, "node_id": nodeID,
		"scope_id": scopeID,
		"version": version,
	}).Info("Managed TLS runtime certificate activated")
	return nil
}
```

更新 `Start`：

- `Prepare` 成功后，首次 `startRuntime()` 成功即把 `managedTLSRuntimeVersion` 设为当前 snapshot version；
- 两处 `c.managedTLS.Start(...)` 回调都改为 `c.activateManagedTLSRuntime`；
- 删除只适合首次启动的 `startRuntimeAfterManagedTLSReady`。

- [ ] **Step 4: 运行 node 目标测试和现有回归测试**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLSReadyNotification|TestController(Activates|Keeps)' -count=1
go test ./node -count=1
```

Expected: PASS；现有首次启动、离线快照、用户同步和连接生命周期测试保持通过。

- [ ] **Step 5: 提交 Controller 接线**

```powershell
git add -- node/controller.go node/managed_tls_runtime_activation_test.go
git commit -m "修复：激活托管证书后再放行域名迁移" -m "证书版本变化时仅替换对应 NodeID 入站，并与用户状态更新串行化。`n`n运行时替换失败不会推进激活版本或迁移准备状态。"
```

### Task 4: 故障记录、全量验证和交付检查

**Files:**
- Modify: `D:/常用项目/v2board/LESSONS_LEARNED.md`

- [ ] **Step 1: 在故障经验中加入可检索记录**

在 `LESSONS_LEARNED.md` 的托管 TLS 章节追加：

```markdown
### 托管证书落盘后入口仍提供旧证书

- 症状：证书作用域迁移显示候选证书已同步到全部实例，但新 SNI 的真实入口探测持续报证书名称不匹配。
- 影响链：v2board 证书迁移 -> v2node managed TLS store -> Xray TLS runtime -> ingress probe。
- 根因：v2node 只确认了证书文件和 `current` 链接；Xray 文件轮询在 OCSP 错误分支提前返回，已读取的新证书没有进入运行时。
- 修复：按版本区分 installed 与 activated；新版本安装后单独替换对应 NodeID 入站，只有替换成功才上报 migration prepared。
- 验证：检查磁盘证书 SAN、v2node 运行时激活日志、进程 PID/NRestarts、旧/新 SNI 握手和 v2board ingress probe，五者必须一致。
- 下次优先检查：`node/managed_tls_manager.go`、`node/controller.go`、`core/inbound_reload.go`、`V2nodeManagedTlsIngressProbeService`。
```

仅暂存本次新增段落，不覆盖 `LESSONS_LEARNED.md` 中用户已有的未提交改动。

- [ ] **Step 2: 格式化并执行目标测试**

Run:

```powershell
gofmt -w node/managed_tls_manager.go node/managed_tls_runtime_activation_test.go node/controller.go core/inbound.go core/user.go core/node.go core/inbound_reload.go core/inbound_reload_test.go
$env:GOEXPERIMENT='jsonv2'
go test ./core ./node -count=1
```

Expected: PASS，无 panic、race 提示或编译警告。

- [ ] **Step 3: 执行完整 Go 测试和静态差异检查**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./... -count=1
git diff --check
git status --short
```

Expected: 全部包 PASS；`git diff --check` 无输出；v2node 只剩计划内改动，v2board 的既有用户改动仍被保留。

- [ ] **Step 4: 复查证书激活调用链**

逐项确认：

```text
managedTLSManager install vN
  -> notifyReady sees activatedVersion != vN
  -> Controller snapshots users under stateMu
  -> V2Core prepares candidate and rollback handlers
  -> only current tag is replaced
  -> Controller records runtime version vN
  -> manager marks vN activated
  -> migration_prepared becomes true
  -> v2board still requires real ingress probe
```

同时确认不存在 `ReloadCh <-`、`scheduleServiceRestart()`、硬编码 NodeID `302` 或端口 `15014`。

- [ ] **Step 5: 提交故障记录并记录最终提交哈希**

在 v2board `my-feature` 上仅提交新增经验段落：

```powershell
git add -p -- LESSONS_LEARNED.md
git commit -m "文档：记录托管证书运行时未激活故障" -m "记录证书落盘、Xray 运行时和入口探测之间的判断边界。`n`n补充 installed 与 activated 版本分离后的排查与验证路径。"
```

回到 v2node，确认实现提交和工作区：

```powershell
git log -5 --oneline
git status --short --branch
```

Expected: v2node `dev` 包含三个中文实现提交和前置设计/计划提交；不执行 push，不连接生产服务。

## 部署后验收清单（不在本计划中自动执行）

1. 低流量窗口部署新 v2node；本次二进制替换会发生一次进程重启。
2. 确认 `systemctl show v2node -p ActiveState -p MainPID -p NRestarts`。
3. 确认 `current/metadata.json` 的 version、migration ID 和双 SAN 域名。
4. 使用 PROXY protocol v1 分别对旧 SNI、新 SNI 进行真实 TLS 握手。
5. 确认两个 SNI 返回相同的双 SAN 证书 serial/fingerprint。
6. 在 v2board 重新执行入口探测；只有通过后才允许最终切换。
7. 最终切换后继续保留旧域名 24～48 小时，再结束迁移。
