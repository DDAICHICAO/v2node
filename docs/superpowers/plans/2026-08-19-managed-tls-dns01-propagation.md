# Managed TLS DNS-01 Propagation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Cloudflare 托管证书签发不再被节点本地 DNS 传播预检卡死，并在失败时留下真实 ACME 原因。

**Architecture:** 在 `node/managed_tls_issuer.go` 中增加 Cloudflare 专用 DNS-01 policy，显式使用公共递归 DNS，并在 TXT 写入后等待 15 秒后跳过本地传播预检，最终验证仍由 Let’s Encrypt 完成。现有面板错误码保持不变，底层 Obtain 错误仅写入节点日志。

**Tech Stack:** Go 1.23、go-acme/lego v4、logrus、Go `testing`、Git。

---

## 文件结构

- 新建 `node/managed_tls_issuer_test.go`：验证 Cloudflare policy、非 Cloudflare 默认行为和稳定错误码。
- 修改 `node/managed_tls_issuer.go`：定义 policy、生成 lego DNS-01 options、记录 Obtain 原始错误。
- 修改 `LESSONS_LEARNED.md`：记录 NodeID 304 的 DNS-01 本地传播预检故障与以后优先检查点。

### Task 1: 用失败测试固定 Cloudflare DNS-01 policy

**Files:**
- Create: `node/managed_tls_issuer_test.go`
- Modify: `node/managed_tls_issuer.go`

- [ ] **Step 1: 写 Cloudflare policy 的失败测试**

在 `node/managed_tls_issuer_test.go` 中写入：

```go
package node

import (
	"slices"
	"testing"
	"time"
)

func TestManagedTLSDNS01PolicyForCloudflare(t *testing.T) {
	policy, ok := managedTLSDNS01PolicyForProvider(" CLOUDFLARE ")
	if !ok {
		t.Fatal("expected cloudflare managed TLS DNS-01 policy")
	}
	if policy.PropagationWait != 15*time.Second || !policy.SkipPropagationCheck {
		t.Fatalf("unexpected propagation policy: %#v", policy)
	}
	for _, nameserver := range []string{"1.1.1.1:53", "8.8.8.8:53"} {
		if !slices.Contains(policy.RecursiveNameservers, nameserver) {
			t.Fatalf("missing recursive nameserver %q: %#v", nameserver, policy)
		}
	}
}

func TestManagedTLSDNS01PolicyLeavesOtherProvidersUntouched(t *testing.T) {
	if _, ok := managedTLSDNS01PolicyForProvider("route53"); ok {
		t.Fatal("non-cloudflare providers must retain lego defaults")
	}
}
```

- [ ] **Step 2: 运行测试并确认按预期失败**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLSDNS01Policy' -count=1
```

Expected: FAIL，编译错误包含 `undefined: managedTLSDNS01PolicyForProvider`。

- [ ] **Step 3: 写最小 policy 和 option 转换实现**

在 `node/managed_tls_issuer.go` 中加入 `dns01`、`time` 和 `logrus` import，并定义：

```go
var managedTLSCloudflareRecursiveNameservers = []string{
	"1.1.1.1:53",
	"8.8.8.8:53",
}

type managedTLSDNS01Policy struct {
	RecursiveNameservers []string
	PropagationWait      time.Duration
	SkipPropagationCheck bool
}

func managedTLSDNS01PolicyForProvider(provider string) (managedTLSDNS01Policy, bool) {
	if !strings.EqualFold(strings.TrimSpace(provider), "cloudflare") {
		return managedTLSDNS01Policy{}, false
	}

	return managedTLSDNS01Policy{
		RecursiveNameservers: append([]string(nil), managedTLSCloudflareRecursiveNameservers...),
		PropagationWait:      15 * time.Second,
		SkipPropagationCheck: true,
	}, true
}

func managedTLSDNS01ChallengeOptions(provider string) []dns01.ChallengeOption {
	policy, ok := managedTLSDNS01PolicyForProvider(provider)
	if !ok {
		return nil
	}

	return []dns01.ChallengeOption{
		dns01.AddRecursiveNameservers(policy.RecursiveNameservers),
		dns01.PropagationWait(policy.PropagationWait, policy.SkipPropagationCheck),
	}
}
```

- [ ] **Step 4: 将 policy 接入 managed TLS provider**

把：

```go
if err := client.Challenge.SetDNS01Provider(providerInstance); err != nil {
```

改为：

```go
if err := client.Challenge.SetDNS01Provider(
	providerInstance,
	managedTLSDNS01ChallengeOptions(provider)...,
); err != nil {
```

- [ ] **Step 5: 运行测试并确认通过**

Run:

```powershell
gofmt -w node/managed_tls_issuer.go node/managed_tls_issuer_test.go
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLSDNS01Policy' -count=1
```

Expected: PASS。

### Task 2: 保留机器错误码并记录 Obtain 原因

**Files:**
- Modify: `node/managed_tls_issuer.go`
- Test: `node/managed_tls_issuer_test.go`

- [ ] **Step 1: 写错误映射的失败测试**

在测试文件追加：

```go
func TestManagedTLSObtainFailureKeepsStableMachineCode(t *testing.T) {
	err := managedTLSObtainFailure(3, errors.New("authoritative TXT missing"))
	if err == nil || err.Error() != "managed_tls_obtain_failed" {
		t.Fatalf("unexpected mapped error: %v", err)
	}
}
```

并在 import 中加入 `errors`。

- [ ] **Step 2: 运行测试并确认按预期失败**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLSObtainFailure' -count=1
```

Expected: FAIL，编译错误包含 `undefined: managedTLSObtainFailure`。

- [ ] **Step 3: 实现日志与稳定错误码映射**

在 `node/managed_tls_issuer.go` 中加入：

```go
func managedTLSObtainFailure(scopeID uint64, cause error) error {
	log.WithError(cause).
		WithField("scope_id", scopeID).
		Warn("Managed TLS certificate obtain failed")

	return fmt.Errorf("managed_tls_obtain_failed")
}
```

把 `Certificate.Obtain` 的错误分支改为：

```go
resource, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: domains, Bundle: true})
if err != nil {
	return nil, managedTLSObtainFailure(request.ScopeID, err)
}
```

日志只接收 lego 的 error，不记录 `request.DNSEnv`。

- [ ] **Step 4: 运行测试并确认通过**

Run:

```powershell
gofmt -w node/managed_tls_issuer.go node/managed_tls_issuer_test.go
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLS(ObtainFailure|DNS01Policy)' -count=1
```

Expected: PASS；测试日志可以出现脱敏的模拟错误，但返回值必须仍是 `managed_tls_obtain_failed`。

### Task 3: 记录经验并完成验证

**Files:**
- Modify: `LESSONS_LEARNED.md`
- Verify: `node/managed_tls_issuer.go`
- Verify: `node/managed_tls_issuer_test.go`

- [ ] **Step 1: 更新故障经验记录**

在 `LESSONS_LEARNED.md` 追加一节，必须包含：

```markdown
## 托管 TLS Cloudflare TXT 已创建但本地传播预检超时

- 症状：作用域停在 v0 并显示 `managed_tls_obtain_failed`，节点日志先显示 Cloudflare `new record`，随后每 12 秒等待传播，两分钟后清理 challenge。
- 影响链：v2board 作用域租约 -> v2node managed TLS issuer -> Cloudflare DNS API -> lego 本地传播预检 -> Let’s Encrypt DNS-01。
- 根因：TXT 写入成功，但节点通过 `/etc/resolv.conf` 的 systemd-resolved stub 执行本地传播预检时无法完成，导致尚未进入 ACME 最终验证就超时。
- 修复：Cloudflare managed TLS 使用公共递归 DNS，TXT 写入后固定等待 15 秒并跳过节点本地传播预检；最终权限校验仍由 Let’s Encrypt 完成；Obtain 原始错误写入节点日志。
- 验证：运行 managed TLS 定向单元测试；部署后重新签发，确认不再等待本地传播两分钟并生成 v1 证书。
- 下次先查：先区分 Cloudflare TXT 创建失败、本地传播预检失败和 Let’s Encrypt 最终验证失败；不要只根据面板通用错误码判断 Token 无效。
- 相关文件：`node/managed_tls_issuer.go`、`node/managed_tls_issuer_test.go`。
```

- [ ] **Step 2: 运行定向与完整 node 测试**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestManagedTLS' -count=1
go test ./node -count=1
```

Expected: 两条命令均 PASS。

- [ ] **Step 3: 完成静态复查**

Run:

```powershell
gofmt -w node/managed_tls_issuer.go node/managed_tls_issuer_test.go
git diff --check
git diff -- node/managed_tls_issuer.go node/managed_tls_issuer_test.go LESSONS_LEARNED.md
```

Expected: `gofmt` 无额外变化，`git diff --check` 无输出；差异只包含本计划范围。

- [ ] **Step 4: 中文提交实现**

Run:

```powershell
git add -- node/managed_tls_issuer.go node/managed_tls_issuer_test.go LESSONS_LEARNED.md docs/superpowers/plans/2026-08-19-managed-tls-dns01-propagation.md
git commit -m "修复：稳定托管证书 DNS 校验" -m "Cloudflare TXT 写入后使用确定性等待并跳过异常的节点本地传播预检，最终验证仍由 Let’s Encrypt 完成。`n`n同时保留面板机器错误码，并在节点日志记录底层 ACME 失败原因。"
```

- [ ] **Step 5: 部署后验收**

用户部署新 v2node 后，在证书作用域 #3 选择“重新签发证书”。节点日志预期在 Cloudflare `new record` 后等待约 15 秒，随后进入 Let’s Encrypt 验证；成功时面板显示 v1 或更高版本且成员同步为 `1/1`。
