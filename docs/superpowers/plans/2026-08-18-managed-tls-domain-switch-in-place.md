# Managed TLS Domain Switch In-Place Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent a v2board managed-TLS SNI cutover from sending v2node into the global reload path when the current activated certificate already covers the target domain.

**Architecture:** Add a concurrency-safe authoritative-domain commit to `managedTLSManager` that validates the current certificate, persists the new node snapshot through a callback, and only then changes the manager domain. Classify an incoming `NodeInfo` change only after proving that every non-domain field is unchanged, and let `nodeInfoMonitor` apply that narrow change in place. All other node configuration changes keep the existing `ReloadCh` behavior, while a missing target SAN or persistence failure leaves both the current runtime and authoritative state unchanged.

**Tech Stack:** Go, v2node controller/task lifecycle, managed TLS file store, Xray Core, standard `testing`, `GOEXPERIMENT=jsonv2`.

---

### Task 1: Make the managed TLS authoritative domain safely mutable

**Files:**
- Modify: `node/managed_tls_manager.go`
- Test: `node/managed_tls_runtime_activation_test.go`

- [ ] **Step 1: Write the failing domain-update tests**

Append a small in-memory store and tests to `node/managed_tls_runtime_activation_test.go`:

```go
type managedTLSDomainTestStore struct {
	certificate *managedTLSLocalCertificate
}

func (s *managedTLSDomainTestStore) Current(scopeID uint64, domain string, _ time.Time) (*managedTLSLocalCertificate, error) {
	if s.certificate == nil || s.certificate.Metadata.ScopeID != scopeID {
		return nil, errManagedTLSLocalCertificateInvalid
	}
	domains, err := managedTLSMetadataDomains(s.certificate.Metadata)
	if err != nil || !managedTLSDomainsContain(domains, domain) {
		return nil, errManagedTLSLocalCertificateInvalid
	}
	return s.certificate, nil
}

func (s *managedTLSDomainTestStore) Install(managedTLSLocalCertificate) error { return nil }
func (s *managedTLSDomainTestStore) Rollback() error                         { return nil }
func (s *managedTLSDomainTestStore) CertFile() string                        { return "fullchain.pem" }
func (s *managedTLSDomainTestStore) KeyFile() string                         { return "private.key" }

func TestManagedTLSManagerCommitsAuthoritativeDomainCoveredByCurrentCertificate(t *testing.T) {
	certificate := migrationCertificateForActivationTest(2)
	m := &managedTLSManager{
		store:            &managedTLSDomainTestStore{certificate: certificate},
		scopeID:          1,
		domain:           "old.example.com",
		activatedVersion: 2,
		snapshot:         managedTLSStatusSnapshot{Managed: true, ScopeID: 1},
	}
	m.setFromLocal(managedTLSReady, certificate, "")

	persisted := false
	if err := m.CommitDomain("new.example.com", time.Unix(1_800_000_000, 0), func() error {
		persisted = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !persisted {
		t.Fatal("domain commit skipped persistence")
	}
	if got := m.Domain(); got != "new.example.com" {
		t.Fatalf("domain=%q want new.example.com", got)
	}
	if !m.Snapshot().MigrationPrepared {
		t.Fatal("same activated dual-SAN version must remain prepared")
	}
}

func TestManagedTLSManagerRejectsDomainMissingFromCurrentCertificateBeforePersistence(t *testing.T) {
	certificate := migrationCertificateForActivationTest(2)
	m := &managedTLSManager{
		store:    &managedTLSDomainTestStore{certificate: certificate},
		scopeID:  1,
		domain:   "old.example.com",
		snapshot: managedTLSStatusSnapshot{Managed: true, ScopeID: 1},
	}

	persisted := false
	if err := m.CommitDomain("missing.example.com", time.Unix(1_800_000_000, 0), func() error {
		persisted = true
		return nil
	}); err == nil {
		t.Fatal("expected target SAN validation failure")
	}
	if persisted {
		t.Fatal("invalid target domain was persisted")
	}
	if got := m.Domain(); got != "old.example.com" {
		t.Fatalf("domain changed after rejection: %q", got)
	}
}

func TestManagedTLSManagerKeepsDomainWhenPersistenceFails(t *testing.T) {
	certificate := migrationCertificateForActivationTest(2)
	m := &managedTLSManager{
		store: &managedTLSDomainTestStore{certificate: certificate}, scopeID: 1,
		domain: "old.example.com", snapshot: managedTLSStatusSnapshot{Managed: true, ScopeID: 1},
	}

	persistErr := errors.New("persist failed")
	err := m.CommitDomain("new.example.com", time.Unix(1_800_000_000, 0), func() error {
		return persistErr
	})
	if !errors.Is(err, persistErr) {
		t.Fatalf("err=%v want persist failure", err)
	}
	if got := m.Domain(); got != "old.example.com" {
		t.Fatalf("domain changed after persistence failure: %q", got)
	}
}
```

Add `time` to the test imports.

- [ ] **Step 2: Run the tests and verify RED**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLSManager(Commits|Rejects|Keeps)' -count=1
```

Expected: compilation fails because `CommitDomain` and `Domain` do not exist.

- [ ] **Step 3: Implement a serialized manager-domain update**

In `managedTLSManager`, add an operation mutex before the mutable domain:

```go
operationMu sync.Mutex
domain      string
```

Serialize the three operations that read or replace the authoritative domain:

```go
func (m *managedTLSManager) Prepare(now time.Time) error {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	return m.prepare(now)
}

func (m *managedTLSManager) reconcileOnce(ctx context.Context) (time.Duration, error) {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	return m.reconcileOnceLocked(ctx)
}
```

Move the existing bodies unchanged into `prepare` and `reconcileOnceLocked`. Add:

```go
func (m *managedTLSManager) Domain() string {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	return m.domain
}

func (m *managedTLSManager) CommitDomain(domain string, now time.Time, persist func() error) error {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if !validManagedTLSDomain(domain) {
		return errManagedTLSLocalCertificateInvalid
	}
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	if sameManagedTLSDomain(m.domain, domain) {
		return nil
	}
	certificate, err := m.store.Current(m.scopeID, domain, now)
	if err != nil || certificate == nil {
		return errManagedTLSLocalCertificateInvalid
	}
	if persist != nil {
		if err := persist(); err != nil {
			return err
		}
	}
	m.domain = domain
	m.setFromLocal(managedTLSReady, certificate, "")
	return nil
}
```

The wrapper split is required so every existing `m.domain` use remains serialized without holding a mutex across two nested calls. The persistence callback runs while `operationMu` is held, after SAN validation and before `m.domain` changes, so a failed snapshot save cannot leave the manager and offline state disagreeing.

- [ ] **Step 4: Run the target tests and verify GREEN**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLSManager(Commits|Rejects|Keeps)|TestManagedTLSReadyNotification' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the manager boundary**

```powershell
git add -- node/managed_tls_manager.go node/managed_tls_runtime_activation_test.go
git commit -m "功能：支持托管证书权威域名原地更新" -m "切换前验证当前证书覆盖目标 SAN，并序列化证书协调与域名更新。"
```

### Task 2: Classify only managed-TLS domain-only configuration changes

**Files:**
- Create: `node/managed_tls_domain_switch.go`
- Test: `node/managed_tls_runtime_activation_test.go`

- [ ] **Step 1: Write failing classifier tests**

Add a managed-node fixture and table test:

```go
func managedTLSDomainSwitchNode(domain string) *panel.NodeInfo {
	return &panel.NodeInfo{
		Id: 302, Type: "trojan", Security: panel.Tls,
		PushInterval: time.Minute, PullInterval: time.Minute, Tag: "node-302",
		Common: &panel.CommonNode{
			Protocol: "trojan", ServerPort: 15014, BaseConfig: &panel.BaseConfig{},
			Tls: panel.Tls,
			TlsSettings: panel.TlsSettings{
				CertMode: "managed", CertificateScopeID: 1,
				ServerName: domain, ServerNames: []string{domain},
			},
			CertInfo: &panel.CertInfo{
				CertMode: "managed", CertFile: "fullchain.pem", KeyFile: "private.key", CertDomain: domain,
			},
		},
	}
}

func TestManagedTLSDomainOnlyChange(t *testing.T) {
	current := managedTLSDomainSwitchNode("old.example.com")
	target := managedTLSDomainSwitchNode("new.example.com")
	domain, ok := managedTLSDomainOnlyChange(current, target)
	if !ok || domain != "new.example.com" {
		t.Fatalf("domain=%q ok=%v", domain, ok)
	}

	changedPort := managedTLSDomainSwitchNode("new.example.com")
	changedPort.Common.ServerPort++
	if _, ok := managedTLSDomainOnlyChange(current, changedPort); ok {
		t.Fatal("port change must retain the full reload path")
	}

	changedScope := managedTLSDomainSwitchNode("new.example.com")
	changedScope.Common.TlsSettings.CertificateScopeID++
	if _, ok := managedTLSDomainOnlyChange(current, changedScope); ok {
		t.Fatal("scope change must retain the full reload path")
	}
}
```

- [ ] **Step 2: Run the classifier test and verify RED**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run TestManagedTLSDomainOnlyChange -count=1
```

Expected: compilation fails because `managedTLSDomainOnlyChange` does not exist.

- [ ] **Step 3: Implement the strict classifier**

Create `node/managed_tls_domain_switch.go`:

```go
package node

import (
	"reflect"
	"strings"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func managedTLSDomainOnlyChange(current, next *panel.NodeInfo) (string, bool) {
	if current == nil || next == nil || current.Common == nil || next.Common == nil ||
		current.Common.CertInfo == nil || next.Common.CertInfo == nil ||
		current.Security != panel.Tls || next.Security != panel.Tls ||
		!strings.EqualFold(current.Common.TlsSettings.CertMode, "managed") ||
		!strings.EqualFold(next.Common.TlsSettings.CertMode, "managed") ||
		!strings.EqualFold(current.Common.CertInfo.CertMode, "managed") ||
		!strings.EqualFold(next.Common.CertInfo.CertMode, "managed") ||
		current.Common.TlsSettings.CertificateScopeID == 0 ||
		next.Common.TlsSettings.CertificateScopeID == 0 {
		return "", false
	}
	target := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(next.Common.TlsSettings.PrimaryServerName())), ".")
	currentDomain := current.Common.TlsSettings.PrimaryServerName()
	if !validManagedTLSDomain(currentDomain) || !validManagedTLSDomain(target) ||
		sameManagedTLSDomain(currentDomain, target) {
		return "", false
	}

	left := cloneNodeInfoForManagedTLSDomainComparison(current)
	right := cloneNodeInfoForManagedTLSDomainComparison(next)
	if !reflect.DeepEqual(left, right) {
		return "", false
	}
	return target, true
}

func cloneNodeInfoForManagedTLSDomainComparison(info *panel.NodeInfo) *panel.NodeInfo {
	clone := *info
	common := *info.Common
	clone.Common = &common
	common.TlsSettings.ServerName = ""
	common.TlsSettings.ServerNames = nil
	if info.Common.CertInfo != nil {
		certInfo := *info.Common.CertInfo
		certInfo.CertDomain = ""
		common.CertInfo = &certInfo
	}
	return &clone
}
```

- [ ] **Step 4: Run the classifier and node tests**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLSDomainOnlyChange|TestManagedTLSManager' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the classifier**

```powershell
git add -- node/managed_tls_domain_switch.go node/managed_tls_runtime_activation_test.go
git commit -m "功能：识别托管 TLS 纯域名配置变化" -m "仅忽略 server_name、server_names 与派生 CertDomain，其他字段变化继续全量重载。"
```

### Task 3: Apply the domain-only change without `ReloadCh`

**Files:**
- Modify: `node/task.go`
- Modify: `node/managed_tls_domain_switch.go`
- Test: `node/user_delta_test.go`

- [ ] **Step 1: Write failing monitor-path tests**

Add three tests to `node/user_delta_test.go` using `managedTLSDomainSwitchNode` and `managedTLSDomainTestStore`:

```go
func managedTLSDomainSwitchController(t *testing.T) (*Controller, chan struct{}) {
	t.Helper()
	current := managedTLSDomainSwitchNode("old.example.com")
	certificate := migrationCertificateForActivationTest(2)
	reloadCh := make(chan struct{}, 1)
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: current.Id}
	c := &Controller{
		apiClient: &panel.Client{}, conf: &cfg, store: newOfflineStateStore(t.TempDir()), info: current,
		tag: current.Tag, server: &core.V2Core{ReloadCh: reloadCh},
		userList: []panel.UserInfo{}, aliveMap: map[int]int{}, deviceAliveMap: map[int]int{},
		managedTLS: &managedTLSManager{
			store: &managedTLSDomainTestStore{certificate: certificate}, scopeID: 1,
			domain: "old.example.com", activatedVersion: 2,
			snapshot: managedTLSStatusSnapshot{Managed: true, ScopeID: 1},
		},
	}
	c.managedTLS.setFromLocal(managedTLSReady, certificate, "")
	return c, reloadCh
}

func TestApplyPendingNodeInfoUpdatesManagedTLSDomainWithoutReload(t *testing.T) {
	c, reloadCh := managedTLSDomainSwitchController(t)
	c.pendingNodeInfo = managedTLSDomainSwitchNode("new.example.com")
	if err := c.applyPendingNodeInfo(); err != nil { t.Fatal(err) }
	select { case <-reloadCh: t.Fatal("domain-only change queued a global reload"); default: }
	if got := c.info.Common.TlsSettings.PrimaryServerName(); got != "new.example.com" { t.Fatalf("domain=%q", got) }
	if got := c.managedTLS.Domain(); got != "new.example.com" { t.Fatalf("manager domain=%q", got) }
	if c.pendingNodeInfo != nil { t.Fatal("pending node info was not cleared") }
}

func TestApplyPendingNodeInfoKeepsReloadForOtherConfigChanges(t *testing.T) {
	c, reloadCh := managedTLSDomainSwitchController(t)
	next := managedTLSDomainSwitchNode("new.example.com")
	next.Common.ServerPort++
	c.pendingNodeInfo = next
	if err := c.applyPendingNodeInfo(); err != nil { t.Fatal(err) }
	select { case <-reloadCh: default: t.Fatal("runtime config change did not queue reload") }
}

func TestApplyPendingNodeInfoFailsClosedWhenTargetSANIsMissing(t *testing.T) {
	c, reloadCh := managedTLSDomainSwitchController(t)
	c.pendingNodeInfo = managedTLSDomainSwitchNode("missing.example.com")
	if err := c.applyPendingNodeInfo(); err == nil { t.Fatal("expected target SAN failure") }
	select { case <-reloadCh: t.Fatal("invalid target SAN queued a global reload"); default: }
	if c.info.Common.TlsSettings.PrimaryServerName() != "old.example.com" { t.Fatal("current runtime state changed") }
	if c.pendingNodeInfo == nil { t.Fatal("failed change must remain pending for retry") }
	if _, err := c.store.Load(*c.conf); err == nil { t.Fatal("invalid target domain was persisted") }
}

func TestApplyPendingNodeInfoKeepsCurrentStateWhenPersistenceFails(t *testing.T) {
	c, reloadCh := managedTLSDomainSwitchController(t)
	notDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("occupied"), 0600); err != nil { t.Fatal(err) }
	c.store = newOfflineStateStore(notDirectory)
	c.pendingNodeInfo = managedTLSDomainSwitchNode("new.example.com")
	if err := c.applyPendingNodeInfo(); err == nil { t.Fatal("expected snapshot save failure") }
	select { case <-reloadCh: t.Fatal("persistence failure queued a global reload"); default: }
	if c.info.Common.TlsSettings.PrimaryServerName() != "old.example.com" { t.Fatal("current runtime state changed") }
	if got := c.managedTLS.Domain(); got != "old.example.com" { t.Fatalf("manager domain=%q", got) }
	if c.pendingNodeInfo == nil { t.Fatal("failed change must remain pending for retry") }
}
```

- [ ] **Step 2: Run the monitor tests and verify RED**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run TestApplyPendingNodeInfo -count=1
```

Expected: compilation fails because `applyPendingNodeInfo` does not exist.

- [ ] **Step 3: Implement controller in-place application**

Add to `node/managed_tls_domain_switch.go`:

```go
func (c *Controller) applyManagedTLSDomainChange(next *panel.NodeInfo) (bool, error) {
	target, ok := managedTLSDomainOnlyChange(c.info, next)
	if !ok { return false, nil }
	if c.managedTLS == nil { return true, errors.New("managed TLS manager is nil") }

	c.managedTLSActivationMu.Lock()
	defer c.managedTLSActivationMu.Unlock()
	if err := c.managedTLS.CommitDomain(target, time.Now().UTC(), func() error {
		return c.persistOfflineState(next)
	}); err != nil {
		return true, fmt.Errorf("apply managed TLS domain %s: %w", target, err)
	}
	c.stateMu.Lock()
	c.info = next
	c.stateMu.Unlock()
	log.WithFields(log.Fields{"tag": c.tag, "node_id": next.Id, "scope_id": next.Common.TlsSettings.CertificateScopeID, "domain": target}).Info("Managed TLS domain applied without global reload")
	return true, nil
}
```

Add the required `errors`, `fmt`, `time`, and logrus imports.

- [ ] **Step 4: Refactor pending configuration commit in `nodeInfoMonitor`**

Add to `node/task.go`:

```go
func (c *Controller) applyPendingNodeInfo() error {
	if c.pendingNodeInfo == nil { return nil }
	applied, err := c.applyManagedTLSDomainChange(c.pendingNodeInfo)
	if err != nil { return err }
	if !applied {
		if err := c.persistOfflineState(c.pendingNodeInfo); err != nil { return err }
		if err := c.queueReload(); err != nil { return err }
	}
	c.pendingNodeInfo = nil
	c.recordPanelSuccess("config")
	return nil
}
```

Replace both duplicated persist/queue blocks in `nodeInfoMonitor` with calls to `applyPendingNodeInfo()`. Change the new-config log text from `persist before reload` to `persist before apply`; retain `pendingNodeInfo` on every failure.

- [ ] **Step 5: Run the target tests and verify GREEN**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestApplyPendingNodeInfo|TestNodeInfoMonitorPersistsNewConfigBeforeReload|TestManagedTLSDomainOnlyChange' -count=1
```

Expected: PASS.

- [ ] **Step 6: Run all node tests and commit**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -count=1
git diff --check
```

Expected: PASS and no whitespace errors.

Commit:

```powershell
git add -- node/task.go node/managed_tls_domain_switch.go node/user_delta_test.go
git commit -m "修复：托管 SNI 切换避免全量重载" -m "持久化并原地应用仅域名字段变化；其他节点配置仍保留 ReloadCh 行为。" -m "目标 SAN 校验失败时保持现有运行时并等待重试。"
```

### Task 4: Record the production lesson and complete verification

**Files:**
- Modify: `LESSONS_LEARNED.md`

- [ ] **Step 1: Add the incident record**

Append a dated entry containing:

```markdown
### 2026-08-18 托管 SNI 切换误触发全量 Xray 重载

- 症状：域名迁移门禁与入口探测通过后执行切换，systemd PID 和 NRestarts 不变，但目标端口约 22 秒不可用，日志再次出现 `Xray ... started`。
- 影响链：v2board 域名切换 -> `/api/v2/server/config` ETag 变化 -> `nodeInfoMonitor` -> 全局 `ReloadCh` -> 同机全部控制器和 Core 重建。
- 根因：节点配置监控未区分 managed TLS 的纯 SNI 元数据变化与真正影响入站监听器的配置变化。
- 修复：严格比较忽略域名字段后的 `NodeInfo`；校验当前证书覆盖目标 SAN 后，仅更新证书管理器权威域名和控制器信息。
- 验证：目标测试、全量 Go 测试、`go vet`、竞态测试（环境支持时）、禁止 `ReloadCh` 的专用测试；上线后复核 PID、NRestarts、端口连续性和 TLS 指纹。
- 下次先查：`node/task.go`、`node/managed_tls_domain_switch.go`、`node/managed_tls_manager.go`、`cmd/server.go::reload`。
```

- [ ] **Step 2: Commit the learning record**

```powershell
git add -- LESSONS_LEARNED.md
git commit -m "文档：记录托管 SNI 全量重载故障" -m "补充症状、调用链、根因、修复边界与上线验收点。"
```

- [ ] **Step 3: Run fresh full verification**

Run:

```powershell
gofmt -w node/managed_tls_manager.go node/managed_tls_domain_switch.go node/managed_tls_runtime_activation_test.go node/task.go node/user_delta_test.go
$env:GOEXPERIMENT='jsonv2'
go test ./node -count=1
go test ./... -count=1
go vet ./node ./core
go test -race ./node -count=1
git diff --check
git status --short --branch
git log -8 --oneline
```

Expected: all available checks pass and the worktree is clean. If `-race` cannot run because Windows lacks CGO/`gcc`, record that exact environment gap rather than reporting it as passed.

- [ ] **Step 4: Review the changed call path**

Confirm from the final diff that:

- only managed TLS domain-only changes bypass `ReloadCh`;
- target SAN validation happens before controller state changes;
- failed validation keeps `pendingNodeInfo` for retry and leaves the current runtime untouched;
- normal node changes still persist before queuing the existing global reload;
- no NodeID, port, instance ID, domain, token, or certificate secret is hard-coded.

- [ ] **Step 5: Hand off deployment and live verification**

Do not push or deploy automatically. Report the commits on `dev`, ask the user to deploy the new v2node build once, then verify on the live node:

```bash
systemctl show v2node.service -p MainPID -p NRestarts -p ExecMainStartTimestamp
ss -H -ltnp '( sport = :15014 )'
journalctl -u v2node.service --since '<cutover time>' --no-pager | grep -E 'Managed TLS domain applied|Xray .* started|重启成功'
```

The next managed-domain switch must log `Managed TLS domain applied without global reload`, keep the same PID and `NRestarts`, keep the port continuously listening, and continue serving the expected certificate fingerprint.
