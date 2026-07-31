# v2node 用户同步唤醒连接稳定性 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让用户同步追赶的临时失败在现有 WebSocket 内重试，避免节点重连风暴，并在同步成功后可靠确认最高 revision。

**Architecture:** 保留现有 v2node WebSocket 协议和外层拨号/fallback 状态机。拨号成功后立即进入读循环，读循环把同步错误转成定时重试，只把读写类连接错误返回给外层重连。

**Tech Stack:** Go 1.26、gorilla/websocket、Go testing、`GOEXPERIMENT=jsonv2`

---

### Task 1: 用回归测试锁定连接内重试

**Files:**
- Modify: `node/user_delta_test.go`

- [ ] **Step 1: 增加可控的假唤醒连接**

在测试文件中增加实现 `ReadMessage`、`SendApplied`、`SendPong` 和 `Close` 的假连接，使用 channel 驱动服务端消息并记录 ACK、pong 与关闭次数。

- [ ] **Step 2: 写同步失败后保持连接的测试**

测试先放入 revision 42。`syncFn` 第一次返回普通临时错误，第二次返回 seq 99；两次同步之间向连接发送 ping。断言先收到 pong，随后收到 revision 42/seq 99 的 ACK，且取消 context 前关闭次数为 0。

- [ ] **Step 3: 写 ACK 失败属于连接错误的测试**

让同步成功但 `SendApplied` 返回写错误，断言 `serveConnection` 返回错误而不是继续在同一连接内重试。

- [ ] **Step 4: 运行红灯**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run 'TestUserSyncRuntimeKeepsConnectionWhileCatchUpRetries|TestUserSyncRuntimeReconnectsAfterAckWriteFailure' -count=1
```

Expected: FAIL，因为 `serveConnection` 仍要求具体连接类型，并且同步错误会退出循环。

### Task 2: 实现原连接内追赶重试

**Files:**
- Modify: `node/user_sync_runtime.go`
- Test: `node/user_delta_test.go`

- [ ] **Step 1: 抽象运行时需要的最小连接接口**

定义只包含 `ReadMessage`、`SendPong` 和 `Close` 的内部接口，让真实 `panel.UserSyncWakeupConnection` 与测试假连接共同满足。

- [ ] **Step 2: 拨号后立即进入读循环**

删除 `runWakeup()` 在 `serveConnection()` 前同步并退出的路径。设置 ACK 函数后直接进入读循环；读循环曾达到 `push` 时才重置外层重连退避。

- [ ] **Step 3: 首次追赶和 dirty 合并使用同一定时器**

进入读循环时设置 `catching_up`，按 `MergeMS` 启动首次同步。收到 dirty 时保留最高 revision 并重置合并窗口。

- [ ] **Step 4: 区分同步错误与连接错误**

同步错误保持 `catching_up` 并重排定时器；`UserSyncRetryError` 使用 `retryDelay()`，其他同步错误使用 `fallbackInterval()`。ACK 通道缺失或写失败包装为内部连接错误并返回。

- [ ] **Step 5: 保留已经生效的同步退避**

同步错误已安排重试后，新 dirty 只更新最高 revision，不把 Retry-After 缩短为 merge 窗口。增加 500ms Retry-After 期间收到更高 revision 的回归测试。

- [ ] **Step 6: 运行绿灯和相关包测试**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
gofmt -w node/user_sync_runtime.go node/user_delta_test.go
go test ./node -run 'TestUserSyncRuntime' -count=1
go test ./node ./api/v2board -count=1
```

Expected: PASS。

### Task 3: 记录故障边界并完成验证

**Files:**
- Modify: `LESSONS_LEARNED.md`
- Create: `docs/superpowers/specs/2026-07-31-user-sync-wakeup-stability-design.md`
- Create: `docs/superpowers/plans/2026-07-31-user-sync-wakeup-stability.md`

- [ ] **Step 1: 追加经验记录**

记录症状、调用链根因、修复、验证和生产扩容检查点，不写入真实 IP、令牌或节点私密标识。

- [ ] **Step 2: 运行完整轻量验证**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node ./api/v2board -count=1
go vet ./node ./api/v2board
git diff --check
```

Expected: 全部退出码为 0。

- [ ] **Step 3: 审查差异**

逐段检查 `git diff`，确认没有改变面板协议、用户数据提交顺序、生产配置和 legacy polling。

- [ ] **Step 4: 中文提交**

```powershell
git add node/user_sync_runtime.go node/user_delta_test.go LESSONS_LEARNED.md docs/superpowers/specs/2026-07-31-user-sync-wakeup-stability-design.md docs/superpowers/plans/2026-07-31-user-sync-wakeup-stability.md
git commit -m "可靠性：避免用户同步追赶触发重连风暴" -m "同步临时失败改为在现有 WebSocket 内重试，保持心跳和最高 revision；仅在读写连接失败时进入 fallback 与重连。补充回归测试、设计说明和故障经验。"
```

Expected: 创建一个本地提交，不推送。
