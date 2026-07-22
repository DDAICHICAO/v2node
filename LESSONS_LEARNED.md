# LESSONS LEARNED

## 2026-07-14：面板失联不应拖垮节点数据面

### 症状

官网面板或其通讯链路故障时，运行中的 v2node 可能因周期任务超时触发 reload；reload 或进程重启后又必须在线拉取节点配置和用户，最终导致节点整体离线，已连接用户也无法继续正常使用。

### 受影响链路

`cmd/server.go` → `node.New` → `loadBootstrapState` → `Controller.Start` → `nodeInfoMonitor` / `reportUserTrafficTask` → `common/task.Task.ExecuteWithTimeout` → reload。

面板接口还包括节点配置、用户增量/全量、用户在线统计、设备在线统计、流量和运行状态上报。任何一类失败都不应直接关闭代理内核或清空已生效授权。

### 根因

1. 启动链把面板配置、用户和在线统计作为硬依赖，没有可跨进程恢复的最后成功状态。
2. 周期任务超时默认向核心发送 reload；reload 会先关闭旧节点和核心，再重新访问面板。
3. 用户在线统计接口曾把网络错误和 HTTP 错误伪装成成功的空 map，可能用空状态覆盖上次有效限制。
4. 部分上报接口只检查传输错误，不检查 HTTP 4xx/5xx，导致离线状态和恢复判断不可靠。

### 修复

- 在 `StatePath` 下按 `ApiHost + NodeID` 保存带版本和身份校验的最后成功快照，使用 `0700` 目录、`0600` 文件和临时文件原子替换。
- 启动时在线数据优先；单项请求失败时只从同一有效快照回退。首次无快照仍明确失败，避免空授权启动。
- 运行中同步失败时保留当前用户、在线限制、limiter 和代理核心；完整成功后才刷新快照。
- 新节点配置必须先保存成可启动快照，再发送 reload；保存失败会保留待处理配置并重试。
- 面板同步和上报任务超时不再 reload；只有显式设置 `ReloadOnTimeout` 的证书恢复任务保留原行为。
- 24/72 小时只是限频告警，没有快照过期或停服分支。

### 验证

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./conf ./common/task ./api/v2board ./node -count=1
go test ./... -count=1
git diff --check
```

回归测试覆盖：空用户快照、损坏/未知版本、跨节点拒绝、失败写入保护、在线优先/离线回退、面板 503 不改用户、在线统计不应用半套状态、新配置落盘后才 reload、24/72 小时告警限频，以及任务超时策略。

### 下次优先检查

1. `journalctl -u v2node` 中是否出现 `Panel unavailable`、`keeping current runtime`、`offline snapshot` 或 `recovered`。
2. `StatePath` 是否位于持久化磁盘，目录/文件权限是否为 `700/600`，磁盘是否只读或已满。
3. `api/v2board/node.go` 和 `api/v2board/user.go` 是否把 HTTP 4xx/5xx 向上返回，避免空数据伪成功。
4. `node/task.go` 的失败路径是否调用 `applyUserList`、`UpdateAliveState` 或发送 reload。
5. `common/task/task.go` 新增任务时是否明确决定 `ReloadOnTimeout`，面板类任务默认应为 `false`。

### 相关文件、命令与提交

- 设计：`docs/superpowers/specs/2026-07-14-v2node-offline-snapshot-design.md`
- 运维：`docs/panel-offline-mode.md`
- 状态存储：`conf/conf.go`、`node/offline_state.go`、`node/bootstrap.go`
- 运行保活：`node/controller.go`、`node/task.go`、`node/user.go`、`node/offline_status.go`
- 面板错误传播：`api/v2board/node.go`、`api/v2board/user.go`
- 超时隔离：`common/task/task.go`
- 关键提交：`3e55be3`、`46294d7`、`f2611ef`、`1cdcaf2`
- 排查命令：`systemctl status v2node --no-pager`、`journalctl -u v2node --since '30 min ago' --no-pager`

## 2026-07-22：连接次数不能替代用户双向流量画像

### 症状

后台只能按连接事件猜测用户访问用途，无法解释某个自然日为什么上传或下载突然升高，也无法与倍率后的计费量核对。

### 受影响链路

Xray dispatcher 最终路由 -> 每连接双向字节增量 -> `common/accessaudit` bbolt spool -> HMAC 签名 ingest -> ClickHouse 原始/小时/每日聚合 -> v2board MySQL 权威对账与用途分类 -> 管理端画像和单日诊断。

### 根因

连接开始事件没有传输字节、持续时间、检查点或完成状态。连接多不等于流量大，连接方向也不能从请求次数推导；长连接如果只等关闭再上报，还会让正在发生的高流量长时间不可见。

### 修复

- 在最终路由选定后包装双向连接，客户端写入计为上传，客户端读取计为下载，不替换既有全局计费 `TrafficCounter`。
- 短连接关闭时发送 final；长连接每 5 分钟发送增量 checkpoint，异常退出仍尝试 final 后重新抛出 panic。
- 使用稳定 `event_id`、单调 sequence 和持久 bbolt 队列；临时失败保留重试，400/413 批次二分并只隔离坏的单事件。
- spool 默认上限 256 MiB、最长 24 小时，达到边界时记录 dropped 时间窗口；状态上报暴露 pending、oldest、dropped、rejected、重试和最近成功/错误。
- 同一进程配置多个 NodeID 时，运行状态额外上报不随 NodeID 变化的 `machine_instance_id`；面板据此只采用同一机器最新的一份全局 flow spool 状态，避免重复累计。
- 节点与 ingest 都拒绝任一方向超过 1 PiB 的单条增量，防止异常计数器污染后续小时、每日聚合和用户画像。
- ClickHouse 负责用途归因，MySQL `v2_stat_user*` 继续负责账单权威；两者通过 coverage/gap 明确展示差异。

### 验证

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./common/accessaudit ./conf ./core/app/dispatcher ./node
go vet ./common/accessaudit ./core/app/dispatcher ./node
git diff --check
```

定向测试覆盖签名批次、重试保留、坏事件隔离、队列持久化与容量/年龄裁剪、方向计数、checkpoint/final、MUX 连接生命周期和 panic final。Windows 当前 CGO 关闭，因此 `go test -race` 不能作为本机有效门禁。

### 下次优先检查

1. `flow_traffic_config_reported` 与 `flow_traffic_enabled` 是否符合面板配置。
2. `pending_events/pending_bytes` 是否回落，`oldest_event_at` 是否持续变旧。
3. `dropped_*` 或 `rejected_*` 是否增长并与用户工单日期重叠。
4. `last_error_code` 是否为 400/413、401/403、502 或网络错误；`/health=200` 不能证明 ClickHouse 写入正常。
5. ClickHouse raw 最新事件、小时/每日聚合延迟和 MySQL/ClickHouse coverage 是否同时恢复。

### 相关文件、命令与提交

- 事件与队列：`common/accessaudit/flow_event.go`、`spool.go`、`flow_client.go`。
- 方向采集：`core/app/dispatcher/flow_traffic.go`、`default.go`。
- 配置与状态：`conf/access_audit.go`、`api/v2board/status.go`、`node/user.go`。
- 运维：v2board `docs/access-traffic-profile-diagnostics-runbook.md`。
- 关键提交：`2cac267`、`4c140e7`、`fd03fb0`、`65b44ec`。
