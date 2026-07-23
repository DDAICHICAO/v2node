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

## 2026-07-19：高流量 Trojan 节点的 Go 匿名堆增长必须用 pprof 定位

### 症状

一台 2 GiB、无 Swap 的节点运行 `v2node v5.0.0.52`，进程 RSS 达到约 1.3 GiB，并在约 10 至 17 小时后反复被内核 OOM killer 杀死。重启会暂时恢复，但服务已经连续出现 8 次 OOM。

### 受影响链路

Trojan 入站 -> Xray dispatcher -> 每连接 pipe/buffer -> 用户流量统计与 LinkManager -> SNTP 本地访问流水和 access-audit 队列 -> Go runtime GC。

同进程中的 SNTP Eclipse 入站流量明显较低，不应在没有协议分流证据时先归因给 Eclipse。

### 现场证据与结论

- `smaps_rollup` 显示约 1.25 GiB 为 `Private_Dirty` / `Pss_Anon`，文件映射仅约 23 MiB；这是进程匿名堆，不是 Linux page cache、journald 或 socket kernel memory。
- 最大匿名映射约 1.4 GiB，符合 Go heap arena；短窗口内匿名 RSS 在连接数横向波动时仍可继续增长。
- 该主机没有 `GOMEMLIMIT`、`GOGC` 或 systemd `MemoryMax`，也没有 Swap。Go runtime 看不到部署预算，流量峰值和高水位 buffer/pool 会把进程推到宿主机 OOM 边界。
- 线上每个节点加载约 9,261 个用户；近 30 分钟约 96% 的访问事件来自 Trojan，Eclipse 只占约 4%。access-audit 队列有 10,000 条硬上限且现场无上报失败，因此它不是 1.3 GiB 的单独解释。
- v5.0.0.52 部署前的旧进程在约 1 小时内也达到 669 MiB 峰值，所以不能把问题直接归因于离线快照功能；快照文件每节点约 1.2 MiB，只构成固定的小量副本。
- 当前 `PprofPort=0`。在没有 heap profile 的情况下，只能确认“Trojan/Xray 共享数据面触发的 Go 堆高水位或对象滞留 + 无内存预算”这一层，不能严谨宣称具体是哪一个分配栈泄漏。

### 验证

- `free -h`、`vmstat 1 5`、`/proc/<pid>/status`、`/proc/<pid>/smaps_rollup`、`pmap -x`。
- `systemctl show v2node.service` 的 `MemoryCurrent`、`MemoryPeak`、`NRestarts`、`MemoryMax`。
- `journalctl -k -b` 的 OOM 时间线，以及 systemd 每次退出时约 1.6 GiB 的 memory peak。
- `ss -ntp`、FD/线程计数、按入站 tag 聚合的访问事件量。
- 只读检查线上配置、离线快照摘要和本地 `v2node` / Xray dispatcher、buffer、access-audit 实现。

### 下次优先检查

1. 在维护窗口把 `PprofPort` 设为仅本机监听端口并受控重启，分别采集 `/debug/pprof/heap`、`allocs` 和 `goroutine`；比较 5 至 15 分钟差分，而不是只看一次快照。
2. 用 heap profile 判断主要保留者是 Xray `buf`/pipe、TLS/Trojan 会话、LinkManager、limiter map 还是 access-audit payload，再做单一变量修复。
3. 修复验证后再为 2 GiB 主机设置留足系统余量的 `GOMEMLIMIT`；不要用过低限制把 OOM 换成 GC thrashing。
4. Swap 和 systemd 限制只能作为故障护栏，不能替代 heap profile 和根因修复。
5. 若再次看到高磁盘占用，仍需把访问日志洪泛与本次匿名内存增长分开诊断。
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

## 2026-07-23：审计 spool 入队不得随历史积压做全量扫描

### 症状

一台高连接 Trojan 节点在连接数没有数量级变化时，v2node 匿名内存持续增长到数 GiB，goroutine 达到上万并最终触发 OOM。临时关闭 FlowTraffic 后，普通访问审计仍开启，内存和 goroutine 恢复到正常范围。

### 受影响链路

`flowTrafficSession.Finish` -> `ReportFlow` -> `FlowClient.Report` -> `BoltFlowSpool.Enqueue` -> bbolt 写事务；普通访问记录原链路为 `logSntpUserAccess` -> 易失内存 channel -> HTTP。

### 根因

旧 `BoltFlowSpool.Enqueue` 每写一条都在单个 bbolt 写事务中执行 `pruneExpired` 和 `pruneCapacity`，两者会遍历并解码整个 pending bucket。积压达到百余 MiB 后，单次入队退化为 O(N)，连续事件整体接近 O(N²)；大量连接结束 goroutine 堵在 `bbolt.DB.BeginRWTx`，连带保留连接上下文和缓冲区。普通访问 channel 在队列满或远端失败时还会丢弃唯一访问日志。

### 修复

- bbolt meta 持久化 pending/dropped/rejected/persistence failure 和迁移统计；稳态 `EnqueueBatch`、`Ack`、`Stats` 不再全库扫描。
- 首次打开旧 `flow.db` 时只扫描一次重建增量统计，保留原 key、JSON、FIFO 和待上传事件。
- 普通访问记录写入独立 `access.db`；两类事件各用一个有界单写者，最多 1000 条或 10 ms 合并成一个事务。
- 远端错误保留待上传事件；400/413 只隔离坏的单条。容量/期限裁剪和本地持久化硬失败都产生明确 gap。
- 本地 spool 打开或迁移失败不阻止代理服务启动；运行状态上报错误、积压、缺口、队列水位和迁移结果。

### 首轮灰度追加发现：等待超时不等于日志缺口

首台灰度开启 FlowTraffic 后，bbolt `BeginRWTx` 等待保持为 0，内存和 goroutine 未再出现原先的锁堆积，但普通访问与 Flow 均出现 `local persistence failed: access audit persistence timeout`。当时服务未重启、CPU 不高，队列写入循环仍在运行，说明这是调用方超过 1 秒没有等到事务结果，而不是已经证明事件未落盘。

原实现把“进入有界队列前超时”和“已经进入队列、等待结果超时”合并为同一个错误。后者返回后，原批次仍可能成功提交，因此立即累计 gap 会制造假缺口；若批次随后真实失败，又可能因为调用方已经返回而漏记真实缺口。

修正后的边界是：

- 进入有界队列前超时：事件未被接收，记录 persistence gap。
- 事件已接收但 1 秒内结果待定：继续由原单写循环提交，只增加 `persist_timeouts` 并输出独立的限频延迟 warning，不立即记 gap。
- bbolt 事务最终失败：批处理器通过整批失败回调逐条记录真实 gap，不依赖调用方是否仍在等待，也不重复计数。
- 批次最终成功：由成功回调唤醒上传器并清除此前真实失败的日志状态。

灰度遇到任何 persistence failure 时应停止扩容并暂时关闭 FlowTraffic，但保持普通访问审计开启；修复后仍需在同一台节点重新验证至少 15 分钟，再进入第二批节点。

### 验证

~~~powershell
$env:GOEXPERIMENT='jsonv2'
go test ./common/accessaudit ./conf ./core/app/dispatcher ./node -count=1
go vet ./common/accessaudit ./core/app/dispatcher ./node
git diff --check
~~~

定向测试覆盖旧库仅迁移一次、增量计数、批量单写、已接收结果待定、队列未接收超时、整批事务失败只回报一次、远端失败重启补传、400/413 二分、启动磁盘失败保持代理可用以及状态映射。Windows 缺少 gcc 时不能把 `go test -race` 记为已通过，应在 Linux 验证环境补跑。

首台生产灰度先在 FlowTraffic 关闭时确认普通访问审计工作，再开启 FlowTraffic 连续观察 15 分钟。观察窗口内连接量约为 1700–2300，Pss_Anon 约为 216–276 MiB，MemoryPeak 约为 404 MiB；bbolt `BeginRWTx` 等待、`NRestarts`、真实 persistence failure、`reportUserTrafficTask` 超时、OOM、panic 和 fatal 均为 0。普通访问与 Flow 各出现 11 次按分钟限频的“结果待定”提示，未被误计为日志缺口；内存和 goroutine 随连接量回落，未复现原先持续增长到数 GiB 的现象。首台保留两类审计开启，扩大灰度前仍需取得明确的第二批主机清单和访问授权。

### 下次优先检查

1. Pprof 中 `go.etcd.io/bbolt.(*DB).BeginRWTx` 等待栈是否增长。
2. 连接数相近时 goroutine、MemoryCurrent 和 Pss_Anon 是否连续线性增长。
3. 两类队列的 pending、oldest、dropped、rejected、persistence failure 和写入队列高水位。
4. `access.db` / `flow.db` 文件大小、磁盘余量、NRestarts、OOM 和 `reportUserTrafficTask` 超时。
5. 若旧库迁移失败，先保留原文件并检查首个损坏 key；禁止新建空库覆盖。

### 相关文件、命令与提交

- 队列与批处理：`common/accessaudit/spool_core.go`、`spool.go`、`access_spool.go`、`persist_batcher.go`。
- 客户端：`common/accessaudit/client.go`、`flow_client.go`。
- 配置与状态：`conf/access_audit.go`、`api/v2board/status.go`、`node/user.go`。
- 关键提交：`a9186bf`、`58197bf`、`0818e3a`、`ab18068`、`5bad530`。
