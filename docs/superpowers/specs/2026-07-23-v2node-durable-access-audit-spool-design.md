# v2node 访问审计持久队列与内存保护设计

## 状态

- 日期：2026-07-23
- 结论：已确认设计边界，待实现计划与代码落地
- 适用范围：`v2node` 的普通访问审计事件和连接级 `FlowTraffic` 事件

## 背景与现场证据

生产节点 `hytron-3005-pre-05-24` 在约 8 GiB 内存、无 Swap 的环境中，`v2node` 匿名私有内存增长到约 7.6 GiB，最终被内核 OOM Killer 杀死。受控重启并启用仅监听 `127.0.0.1` 的 pprof 后，现场得到以下证据：

- Trojan 端口约有 1,400 条已建立入站连接，SNTP Eclipse 端口约有 100 多条；Trojan 是主要负载，但连接量本身不足以解释 7.6 GiB。
- `FlowTraffic` 的 bbolt 文件约为 159 MiB。
- 在启用 `FlowTraffic` 后一分钟内，goroutine 从约 11,300 增长到约 12,700。
- 关闭前有 5,205 个 goroutine 堵在 `BoltFlowSpool.Enqueue -> bbolt.DB.BeginRWTx`。
- 堵塞调用来自 `flowTrafficSession.Finish -> ReportFlow -> FlowClient.Report -> BoltFlowSpool.Enqueue`。
- 当前 `BoltFlowSpool.Enqueue` 每写入一条事件都调用 `pruneExpired` 和 `pruneCapacity`；两者会遍历并解码整个 pending bucket。随着本地积压增大，稳态入队退化为 O(N)，连续写入整体退化为 O(N²)。
- 临时关闭 `AccessAudit.FlowTraffic.Enabled` 后，普通 `AccessAudit` 保持开启；bbolt 等待 goroutine 归零，流量任务超时归零，进程内存恢复到数百 MiB。

因此，问题不是日志正文占用数 GiB，而是同步持久化路径的全队列扫描和单写锁竞争阻塞连接结束流程，间接保留大量 goroutine、连接上下文和 Xray 缓冲资源。

## 当前两条数据链

### 普通访问记录

当前链路：

`DefaultDispatcher.routedDispatch -> logSntpUserAccess -> accessaudit.Enqueue -> Client.queue -> Client.loop -> HTTP ingest`

普通访问记录包含用户、来源、目标、节点和网络类型，是用户访问行为的唯一记录。当前实现只使用容量为 `MaxQueueSize` 的内存 channel；队列满时 `Client.Enqueue` 增加 `dropped` 计数并返回 `false`，调用方没有持久化兜底。进程退出、上传端长时间故障或突发流量都可能造成记录丢失。

### 连接级 FlowTraffic

当前链路：

`flowTrafficSession.emit -> ReportFlow -> FlowClient.Report -> BoltFlowSpool.Enqueue -> bbolt -> FlowClient.loop -> HTTP ingest`

这条链记录上传、下载、持续时间、检查点和最终事件。它已有本地持久化，但每条事件同步创建 bbolt 写事务，并在事务内全量扫描历史队列，导致本次内存事故。

## 设计目标

1. 普通访问记录和 `FlowTraffic` 都必须先进入本地持久队列，再由后台上传。
2. 正常流量峰值、上传端变慢或短时不可用时，不静默丢失事件。
3. 数据面不等待远端 HTTP，也不因历史队列大小而出现 O(N) 入队。
4. 本地持久化采用单写入者和批量事务，避免每事件一次事务和 fsync。
5. 待持久化请求数量必须有硬上限，不能通过无界 goroutine 或无界内存队列转移问题。
6. 现有 `/var/lib/v2node/access-audit-spool/flow.db` 原地兼容，保留未上传的历史事件。
7. 达到磁盘容量、保留期限或持久化硬错误时，允许为了节点可用性继续转发，但必须记录明确的缺口计数、时间范围和限频告警；不得静默失败。
8. 保持现有上传事件 JSON、事件 ID、签名方式和服务端接口兼容。

## 非目标

- 不改变用户流量计费、用户在线统计或设备限制语义。
- 不改变 Trojan、SNTP Eclipse、Xray 路由或 TLS 行为。
- 不清空现有 159 MiB `FlowTraffic` 队列。
- 不在本阶段增加管理端页面或新的数据分析功能。
- 不用 `GOMEMLIMIT`、Swap 或扩大机器内存替代根因修复；这些只可作为后续保护栏。

## 方案选择

### 采用：持久化增量统计 + 有界单写入批处理

两类事件分别使用本地持久队列，但复用同一套批量写入和队列统计规则：

- 普通访问记录新增持久队列，默认路径为 `/var/lib/v2node/access-audit-spool/access.db`。
- 普通访问队列新增 `AccessAudit.SpoolPath`、`AccessAudit.MaxSpoolBytes` 和 `AccessAudit.MaxSpoolAge` 配置；默认分别为上述路径、1 GiB 和 7 天。`FlowTraffic` 保留现有 256 MiB、24 小时默认值。
- `FlowTraffic` 继续使用现有 `/var/lib/v2node/access-audit-spool/flow.db`。
- 每类事件只有一个本地批量写入循环，避免大量 goroutine 同时争抢 bbolt 写锁。
- 提交请求携带已复制的紧凑事件值和结果 channel，不携带 Xray link、reader、writer 或请求 context。
- 写入循环每批最多使用 `min(BatchSize, 1000)` 条，或在首条请求到达 10 ms 后提交，在一个 bbolt `Update` 中完成多条写入。
- 提交方只等待本地持久化结果，不等待远端上传；待写请求通过 `MaxQueueSize` 有界控制。
- 正常突发只形成短暂批量等待；本地持久化等待硬上限为 1 秒。队列饱和、事务失败或超过 1 秒进入明确的 hard-failure/gap 统计，不创建新的后台 goroutine。

不采用以下替代方案：

- 只优化 `pruneCapacity`：仍保留每事件事务和同步写锁竞争，无法覆盖高并发连接结束场景。
- 只改为异步内存 channel：能减轻数据面阻塞，但进程崩溃会丢失唯一访问记录，也会把压力转移成无界或满队列问题。
- 永久关闭 `FlowTraffic`：能止血，但丢失已确认需要保留的访问和流量证据。

## 持久队列设计

### 稳态元数据

每个队列在 meta bucket 中持久化：

- `schema_version`
- `pending_events`
- `pending_bytes`
- `oldest_event_at`
- `dropped_events`
- `dropped_bytes`
- `dropped_event_from` / `dropped_event_to`
- `rejected_events`
- `rejected_bytes`
- `rejected_event_from` / `rejected_event_to`
- `persistence_failures`
- `persistence_failure_from` / `persistence_failure_to`

新增、确认、拒绝、过期和容量裁剪都在同一事务内增量修改这些字段。稳态 `EnqueueBatch`、`Ack` 和 `Stats` 不再遍历整个 pending bucket。

### 旧库兼容

打开旧版 `flow.db` 时：

1. 检查 `schema_version` 和增量统计字段。
2. 如果旧库只有 pending 记录而没有可信统计，只在启动时做一次全量重建。
3. 将重建结果和新版本号写入 meta bucket。
4. 后续运行全部走增量统计。

迁移不修改事件键、事件 JSON 或 FIFO 顺序，也不删除现有 pending 记录。迁移失败时不启动 `FlowTraffic` 上传器，保留数据库并产生明确错误，不重建空库覆盖旧数据。

### 批量写入

本地写入循环满足以下规则：

- 单批最多使用 `min(BatchSize, 1000)` 条，首条请求最多等待 10 ms，避免低流量下长期停留在内存。
- 同一批事件在一个事务内分配连续 sequence、编码、写入，并一次性更新 meta 统计。
- 事务成功后再逐一通知提交方；失败时整批返回同一持久化错误。
- 事务函数必须可安全重试，不在事务外提前修改计数。
- 不为每条事件启动独立 goroutine。

### 过期和容量处理

- 过期检查从 FIFO 队首开始，遇到第一条未过期事件即停止，不再全库扫描。
- 容量判断使用持久化的 `pending_bytes`，只在超过上限时从队首删除到恢复上限。
- 每次被裁剪的数量、字节和时间范围写入 gap 统计，并输出限频 warning。
- `Stats` 直接读取 meta；只有旧库迁移或检测到统计不一致时才做修复扫描。

## 普通访问记录持久化

普通 `Client` 从“内存队列直接上传”调整为“本地持久化后上传”：

1. `logSntpUserAccess` 构造紧凑 `Event`。
2. `Client.Enqueue` 把事件提交给本地批量写入循环。
3. bbolt 提交成功后返回成功。
4. 独立上传循环从 `access.db` 按 FIFO 读取一批事件，保持现有 wire 格式和 HMAC 签名发送。
5. HTTP 成功后 `Ack`；超时、网络错误和 5xx 保留原事件并退避重试。
6. 400/413 继续使用拆分定位坏事件；单条确定不可接受时移入 rejected 统计，不阻塞后续队列。

普通访问记录不再以 channel 满作为静默丢弃条件。若本地磁盘写入失败或 1 秒内无法完成，调用返回明确失败，累计 persistence gap，并输出限频错误。这个 hard-failure 分支继续放行业务流量，以符合已确认的节点可用性边界，但日志状态必须明确显示该时间段存在缺口。

## FlowTraffic 持久化

`FlowTraffic` 保留现有事件合同和检查点逻辑，只替换持久化入口：

1. `flowTrafficSession.emit` 生成独立事件值。
2. `FlowClient.Report` 交给单写入批处理，不直接调用 `BoltFlowSpool.Enqueue`。
3. `Finish` 不启动额外 goroutine，也不持有 Xray link；只等待有界的本地持久化结果。
4. 后台上传和重试继续从 `flow.db` 读取。

如果最终事件持久化硬失败，必须记录 session 对应的 gap 时间范围和失败计数。不能用无限等待或无界 goroutine 保证“看起来不丢”。

## 错误处理与可观测性

运行状态至少包含：

- 两类队列各自的 pending events/bytes、oldest event、last success/error、retry count。
- dropped/rejected/persistence-failure 的数量、字节和时间范围。
- 本地写入队列当前深度和最高水位。
- 最近一次旧库重建结果和耗时。

日志要求：

- 同类持久化错误限频输出，避免错误风暴反过来拖垮磁盘。
- 不打印 token、完整用户标识、完整来源 IP、设备 ID 或事件正文。
- 恢复后输出一次恢复日志，包含积压量和持续时间，不包含私人数据。

## 并发与生命周期

- `Configure` 创建并启动两类持久队列和上传器。
- reload 或 shutdown 先停止接收新事件，再提交当前本地批次，最后关闭上传器和 bbolt。
- 关闭过程有明确超时；超时只影响未提交批次，并记录持久化缺口。
- 所有单例替换继续受 `defaultMu` 保护，但等待批量写入或关闭时不得持有 `defaultMu`，避免全局死锁。
- 本地写入循环、远端上传循环和状态读取互相独立；远端 HTTP 卡顿不能阻塞本地持久化。

## 测试策略

实施必须遵循先失败、后修复的测试顺序，至少覆盖：

1. 旧实现回归测试：预置大量/损坏的历史记录时，新增事件不应全量解码旧队列；当前实现应先失败。
2. `EnqueueBatch` 在一个事务内写入多条事件，并正确维护 pending events/bytes。
3. `Ack`、`Reject`、过期和容量裁剪增量更新统计，关闭重开后保持一致。
4. 旧版 `flow.db` 首次打开只重建一次，重开不重复全量扫描。
5. 普通访问记录在上传失败和进程正常重启后仍保留并按 FIFO 补传。
6. `FlowTraffic` 在上传失败时只增加磁盘积压，不产生持续增长的 bbolt 写锁等待 goroutine。
7. 批量写入队列饱和、事务失败和关闭超时都会产生明确 gap/error 状态，不静默丢弃。
8. 400/413 拆分与单条 reject 行为保持兼容。
9. `go test ./common/accessaudit ./conf ./core/app/dispatcher ./node` 通过。

## 上线与回滚

上线前：

1. 保持生产 `AccessAudit.Enabled=true`、`FlowTraffic.Enabled=false`。
2. 备份二进制、systemd unit、`config.json`、`access.db`（如有）和现有 `flow.db`。
3. 记录部署前服务状态、内存、goroutine、队列文件大小和上传错误计数。

上线后：

1. 先在 `FlowTraffic=false` 下验证普通访问记录持久化和补传。
2. 再开启 `FlowTraffic`，不清理旧 `flow.db`。
3. 连续观察至少 15 分钟：bbolt `BeginRWTx` 等待接近零；goroutine 随连接量稳定；RSS/Pss_Anon 不线性增长；普通访问记录和 FlowTraffic 均持续确认；`reportUserTrafficTask` 不超时。
4. 继续保留 localhost-only pprof 直至完成稳定性观察。

回滚时恢复旧二进制和配置，并保持 `FlowTraffic=false`。新版本不得破坏旧 `flow.db` 事件格式，因此回滚后旧程序仍可读取原有 pending 记录；普通访问记录的新 `access.db` 原样保留，待修复版本再次部署后续传。

## 成功标准

- 普通访问记录和 `FlowTraffic` 均可在上传端不可用时持久积压并恢复补传。
- 稳态入队不随历史 pending 数量增长而变慢。
- 生产负载下不再出现成千上万的 `bbolt.BeginRWTx` 等待 goroutine。
- 连接量稳定时 goroutine 和匿名内存不再线性增长。
- 本地容量或磁盘硬故障产生明确、可查询的缺口计数与告警，不静默丢失。
- 现有事件合同、签名、服务端接口和 159 MiB `flow.db` pending 数据保持兼容。
