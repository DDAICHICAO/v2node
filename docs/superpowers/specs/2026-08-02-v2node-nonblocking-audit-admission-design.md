# v2node 审计非阻塞接收设计

## 状态

- 日期：2026-08-02
- 用户选择：采用永久修复，不以关闭审计功能作为长期方案
- 适用范围：普通访问审计与连接级 `FlowTraffic` 的本地持久化入口

## 问题与根因

当前普通访问链路在真正出站前执行：

`DefaultDispatcher.routedDispatch -> logSntpUserAccess -> Client.Enqueue -> persistBatcher.Submit -> 等待 bbolt 批次结果 -> handler.Dispatch`

事件虽然已经进入有界单写入队列，调用方仍会等待 bbolt 事务结果，默认上限为一秒。磁盘 I/O 或 bbolt 写事务变慢时，等待会直接增加到代理请求首包延迟。`FlowClient.Report` 使用相同提交语义，主要拖慢连接检查点与结束阶段。

本次修复不改变 ClickHouse 上传、事件格式、签名、spool 文件格式、流量计费、TLS、Trojan 或路由行为，只解除数据面对本地事务完成结果的同步等待。

## 方案比较

### 采用：有界队列非阻塞接收

- 为 `persistBatcher` 增加明确的非阻塞接收入口。
- 请求只要成功放入既有有界队列就立即返回，不等待 `EnqueueBatch` 和 fsync。
- 队列已满或 batcher 已关闭时立即返回明确错误，不等待 `PersistTimeout`。
- 后台仍由现有单写入器按最多 1000 条或 10 ms 窗口批量写入 bbolt。
- bbolt 最终失败继续通过现有整批 `OnFailure` 回调记录缺口；成功继续唤醒上传器。

该方案直接消除已确认的一秒数据面等待，同时保留有界内存、单写入、批处理和明确缺口统计。

### 不采用：继续缩短同步等待超时

缩短超时只能缩短固定延迟，磁盘再次变慢时仍会把持久化压力传回代理请求，而且不同配置会产生不同的用户延迟。

### 暂不采用：独立 WAL 或审计进程

独立进程可以在严格持久化和数据面隔离之间提供更强保证，但需要新的进程通信、部署、升级和故障恢复协议，超出本次最小修复范围。

## 组件与接口

### `persistBatcher`

保留现有 `Submit` 的“等待事务结果”语义，避免隐式改变潜在严格持久化调用方；新增非阻塞接收方法供代理数据面使用。

非阻塞方法满足以下合同：

1. batcher 可用且队列有容量：复制事件值进入队列，更新队列高水位，立即返回成功。
2. 队列已满：立即返回明确的队列满错误，不创建 goroutine，不等待定时器。
3. batcher 已关闭或 spool 不可用：立即返回关闭错误。
4. 异步请求不要求调用方持有结果 channel；批处理器只向需要同步结果的请求回传事务结果。

### 普通访问审计

`Client.Enqueue` 在事件校验通过后使用非阻塞接收方法：

- 接收成功返回 `true`，`handler.Dispatch` 立即继续。
- 队列满或关闭返回 `false`，通过现有内存 gap 聚合记录 `PersistenceFailures`、字节数和时间范围，并输出限频失败日志。
- 后台事务失败由 `OnFailure` 对整批事件记录一次，不依赖调用方等待。

### `FlowTraffic`

`FlowClient.Report` 使用同一非阻塞接收入口：

- 接收成功立即返回 `nil`。
- 队列满或关闭立即返回错误并记录 gap。
- 检查点、连接结束和 panic 收尾逻辑保持不变，不新增每事件 goroutine。

## 一致性与故障边界

- 已写入 bbolt 的事件继续具备现有进程重启补传能力。
- 已被内存队列接收但尚未完成批次事务的事件，在进程崩溃或机器断电时可能丢失；正常窗口约为当前 10 ms 批次等待加实际事务耗时。
- 正常 shutdown/reload 继续调用 `persistBatcher.Close` 排空队列；两秒关闭超时边界保持不变。
- 队列满、关闭和真实事务失败都必须产生明确 gap，不能静默丢弃。
- 不用无界 channel、每事件 goroutine 或无限重试来掩盖背压。

## 测试设计

按先失败、后修复顺序覆盖：

1. spool 被阻塞时，非阻塞接收在事务完成前返回成功。
2. 单写入器被阻塞且有界队列已满时，新事件立即返回队列满错误，不等待一秒。
3. 普通访问 `Client.Enqueue` 在 spool 被阻塞时立即返回，解除 `handler.Dispatch` 前的事务等待。
4. `FlowClient.Report` 在 spool 被阻塞时立即返回。
5. 已接收事件的后台事务最终失败时，整批 gap 仍只记录一次。
6. 队列满的未接收事件记录明确 gap；成功接收不误记失败。
7. 同步 `Submit` 的既有行为、批量上限、关闭排空和上传协议保持通过。

目标验证命令：

```text
go test ./common/accessaudit ./core/app/dispatcher
go test -race ./common/accessaudit
go vet ./common/accessaudit ./core/app/dispatcher
git diff --check
```

不默认运行完整构建或启动服务。

## 上线与回滚

1. 先在本地完成针对性测试和 race 检查，不直接修改生产配置。
2. 部署前备份当前二进制、systemd unit、`config.json`、`access.db` 和 `flow.db`，不清空现有 spool。
3. 部署后分别确认进程版本、监听端口、连接数、审计 pending/gap、两类 persistence 日志和 FullTClash HTTP 延迟。
4. 至少连续观察 15 分钟；“已部署”“延迟恢复”“审计持续落盘/补传”分别报告。
5. 若出现回归，恢复旧二进制；spool 格式未改变，旧版本仍可读取已有数据。必要时临时关闭 `FlowTraffic` 止血，但保留普通访问审计。

## 成功标准

- 被阻塞的本地 bbolt 事务不再给普通代理请求增加固定一秒等待。
- 高负载下不会新增无界 goroutine，也不会恢复 bbolt 多写入者竞争。
- 队列满、关闭和事务失败均有可查询的 gap 统计与限频日志。
- 普通访问与 FlowTraffic 的事件格式、上传接口、签名和 spool 数据兼容。
- 线上连续观察期内 HTTP 延迟恢复且审计事件仍能持续落盘和补传。
