# LESSONS LEARNED

## 2026-08-17：用户失效时的连接回收必须和新连接登记使用同一生命周期门闩

### 症状与影响链

用户到期或流量耗尽后，面板/节点用户列表已经删除该用户，但高并发转发机仍可能保留少量长期 `ESTABLISHED` 连接，TCP 数量和连接相关内存不能随用户删除完全回落。完整链路为：v2board `traffic:update` / 用户到期 -> `user_delete` 或 v2node 本地到期调度 -> `Controller.applyUserList` -> `V2Core.DelUsers` -> `LinkManager.CloseAll`，同时代理请求走 `DefaultDispatcher` -> limiter -> `LinkManagers` -> `AddLink`。

### 修复前代码结论与根因

- 到期用户有本地最早到期时间调度；流量耗尽由 v2board 入账后生成 `user_delete`。正常删除路径确实调用 `DelUsers` 和 `CloseAll`，因此不是“完全没有连接回收”。
- dispatcher 的两个连接登记入口使用 `sync.Map.Load` 后自行创建并 `Store`，不是原子的单实例登记。同一用户首次并发建连时可产生多个 `LinkManager`，后写入者覆盖前者，被覆盖 manager 上的连接不再能由全局表找到。
- `LinkManager.CloseAll` 只清空当时已有的 links，没有 closed/tombstone 状态；与删除并发的 `AddLink` 可以在清空后重新加入连接。
- `DelUsers` 在关闭后删除全局 manager entry。已经完成认证和 limiter 检查的在途请求仍可能在删除窗口重新登记 manager，之后没有第二次用户删除事件，这些连接只能依赖远端关闭或 Xray idle policy；持续有流量的连接可以长期存活。
- 仅把 `Load`/`Store` 改成 `LoadOrStore` 只能消除首次创建覆盖，不能修复 `CloseAll` 与 `AddLink` 的删除竞态。正确边界需要按用户序列化 get-or-create / close，并让 close 在同一互斥区设置 generation 或 tombstone；`AddLink` 必须拒绝并立即中断已关闭 generation，用户重新授权时再显式开启新 generation。

### 验证、线上判别与下次检查

- 诊断基线 `dev@9365469` 的定向回归 `GOEXPERIMENT=jsonv2 go test ./node -run 'Test(RemoveExpiredUsers|NextUserExpiry|LocalExpiry|ApplyUserList)' -count=1`、FlowTraffic dispatcher 测试、access-audit 非阻塞测试及 `go vet ./node ./core/app/dispatcher ./common/accessaudit` 均通过；当时的测试没有覆盖用户删除与新 dispatch 并发发生时的 active-link 回收。
- 当前 FlowTraffic 已使用 `TrySubmit`，旧的 bbolt 同步入队阻塞连接清理问题不应直接套用到当前源码；生产二进制仍需单独核对版本和运行状态。
- 线上先按 TCP state 和用户维度判别：持续 `ESTABLISHED` 且删除后仍有字节变化才符合本竞态；大量 `TIME_WAIT` 是另一类内核状态，`CLOSE_WAIT`、conntrack 增长、转发机 NAT 内存或旧二进制也要分开检查。
- 修复回归至少覆盖：同用户首次并发建连只产生一个 generation；`CloseAll` 与 `AddLink` 并发时新 link 被拒绝/中断；全局 delete 与在途 dispatch 并发时不能复活 entry；用户重新授权后可创建新 generation；TCP/UDP 和两条 dispatcher 登记入口保持一致。
- 相关文件：`core/app/dispatcher/default.go`、`core/app/dispatcher/linkmanager.go`、`core/user.go`、`node/task.go`、`node/user_expiry.go`、`limiter/limiter.go`；面板链路位于 v2board `app/Console/Commands/TrafficUpdate.php` 与 `app/Services/NodeUserSyncService.php`。
- 诊断阶段没有取得线上主机样本证明某台转发机的现象全部由此竞态造成；代码修复完成后仍需单独核对生产二进制并做灰度观察。

### 已实施修复与验证

- dispatcher 已改为权威 `LinkRegistry`：只有 `AddUsers` 能激活用户槽位，两条代理登记路径只能加入已激活槽位；`DeactivateUser` 原子删除槽位并在 registry 锁外关闭 writer、interrupt 原始 reader。
- `DelUsers` 对整批用户先关闭数据面，再执行 Xray、自定义入站和 Mieru 的认证清理。认证清理失败只产生脱敏告警，不会重新开放 registry，也不会阻止 limiter 更新和离线用户快照提交。
- 标准协议重复添加时先通过 `UserManager.GetUser` 识别相同已安装凭据；批量新增失败只回滚本次新装凭据，任何失败批次都不会提前激活连接槽。
- 回归覆盖未激活拒绝、停用后拒绝复活、同用户并发登记唯一 manager、登记/停用锁边界、按 IP 定向关闭、重新授权 generation 隔离、批量新增回滚和认证清理失败时保持 fail-closed。FlowTraffic 继续使用非阻塞 `TrySubmit`，不进入连接关闭等待路径。
- 本地执行 `GOEXPERIMENT=jsonv2 go test ./core/app/dispatcher ./core ./limiter ./node ./common/accessaudit -count=1`、节点到期/快照定向回归、`go vet ./core/app/dispatcher ./core ./limiter ./node ./common/accessaudit`、旧 `LinkManagers` 源码扫描和 `git diff --check`，结果均通过。
- 实现提交为 `33f2205`（连接生命周期注册表）、`c8943d5`（dispatcher 接入）和 `faf67d8`（授权/失效链）。当前 Windows 主机为 `CGO_ENABLED=0` 且没有 `gcc`，未执行 Linux `go test -race`；生产二进制版本核对和转发机 TCP/RSS/FD 灰度也尚未执行，不能据此声称线上已经修复。

## 2026-08-12：同机多 NodeID 的受管证书必须隔离本地身份

### 症状与链路

一个 NodeID 可同时运行多实例，逐实例 ACME 会反复申请；一台机器也可运行多个 NodeID，因此不能把“机器上某个节点生成的证书”作为无条件共享证书。完整链路为 `conf.NodeConfig.TlsCertificateToken` -> v2board HMAC 证书接口 -> `managedTLSManager` -> scope lease / Lego DNS-01 -> NodeID 本地 store -> `Controller.startRuntime` -> `/api/v2/server/status`。

### 根因与边界

- 共享边界应是 v2board 明确维护的 certificate scope，不是机器、实例或相同域名。
- 每个 `Nodes[]` 条目对应一个 NodeID credential；同机多 NodeID 不能复制同一个 token。
- 同 scope 的 NodeID 只共享证书内容和版本，不共享本地目录、Controller 生命周期或 API 身份。
- DNS provider 使用进程环境变量，必须把整个 Lego Obtain 放在进程级互斥区并在错误、取消和 panic 路径恢复环境。

### 修复

- 增加独立 HMAC client、固定 canonical、时间窗和 nonce；受管接口不携带普通 panel query token，也不允许客户端选择 scope。
- 证书安装在 `/etc/v2node/certificates/{NodeID}/current`，保留 current/previous 并校验 scope metadata、精确 SAN、私钥、有效期和 serverAuth。
- manager 先下载，缺证书才竞争 scope lease；发布后重新 GET 确认 canonical 指纹再安装。已有有效本地证书时 panel 故障进入 degraded，不中断运行。
- managed 尚未 ready 时只让该 Controller 等待并独立上报状态，不阻塞同进程其他 NodeID；ready 后只启动一次 runtime。受管更新不写 `ReloadCh`，旧 `renewCertTask` 排除 managed。
- 安装脚本只允许单 NodeID 携带 `--tls-certificate-token`；多 NodeID 必须在各自 `Nodes[]` 中配置对应 token。

### 验证与下次检查

- 验证：`GOEXPERIMENT=jsonv2 go test ./api/v2board ./node`、`bash -n script/install.sh`、安装脚本单/多 NodeID 函数测试、`git diff --check`。
- 下次先查：managed 状态字段的 scope/version/status/token fingerprint、各 NodeID `metadata.json`、进程 `MainPID/NRestarts`、面板 scope lease holder 和失败冷却；日志不得记录 token、DNS 凭据、PEM、私钥或完整签名。
- 相关文件：`api/v2board/managed_tls.go`, `node/managed_tls_store.go`, `node/managed_tls_issuer.go`, `node/managed_tls_manager.go`, `node/controller.go`, `node/user.go`, `script/install.sh`。
- 关键提交：`9e5ad14`, `b03e8d4`, `b7d9cf6`, `2551a68`, `367e653`。

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

## 2026-07-28：同一 UUID 的 IP 扩散限制必须在连接入站前判定

### 症状与影响链

复制同一客户端配置后，不同机器会携带相同认证 UUID。原设备限制只按 UUID 去重，因此大量不同来源 IP 仍可通过。受影响链为：面板 `uuid_ip_fanout_guard` 配置与 `fanout_exempt` 用户字段 -> v2node 用户同步 -> `Limiter.CheckLimit` -> 在线状态 -> 本地事件队列 -> 面板 `uuidIpFanoutEvents`，以及 `deviceAliveList` 返回的跨节点决定。

### 根因与修复

- 原限流器没有“同一 UID + UUID 在滚动窗口内的唯一 IP 数”状态。
- 新增并发安全 tracker，并在写入 `UserOnlineIP` 之前检查；阈值 10 表示前 10 个不同 IP 允许，第 11 个新 IP 才审计或拒绝，已在窗口内的 IP 不受影响。
- 本地 `reject` 只拒绝超额新 IP，不保存被拒绝 IP；`audit` 有 256 个 IP 的有界证据窗口，事件队列最多 2000 条。
- 面板全局决定使用归一化 IP 的 SHA-256 允许集合和单调 revision；旧 revision、已到期决定、非 reject 模式都不会生效。
- 用户/UUID 白名单由面板折算为 `fanout_exempt`，CIDR 白名单由节点本地解析；上报失败只回队有界事件，不阻断代理连接。
- 管理员从面板清除状态时，全局决定会在下一次 `deviceAliveList` 同步后移除，但节点本地窗口按自身滚动 TTL 自然到期，不会主动踢现有连接。

### 验证与下次检查

- 验证命令：`$env:GOEXPERIMENT='jsonv2'; go test ./api/v2board ./limiter ./core/app/dispatcher ./core`。`node` 包测试逻辑输出 `ok`，但当前 Windows 环境清理临时 `node.test.exe` 时可能报 Access denied；`-race` 需要启用 CGO，应在 Linux 构建环境补跑。
- 下次先查：节点配置是否收到 enabled/mode/window/threshold/cooldown/CIDR；用户是否收到 `fanout_exempt`；`CheckLimit` 是否在在线状态写入前返回 `uuid_ip_fanout_exceeded`；事件失败后队列是否保持有界；全局 revision 是否前进且离线快照没有回退。
- 相关文件：`limiter/uuid_ip_fanout.go`、`limiter/limiter.go`、`api/v2board/node.go`、`api/v2board/user.go`、`node/controller.go`、`node/task.go`、`node/user.go`、`node/offline_state.go`、`core/app/dispatcher/default.go`。
- 相关提交：`d65377b`、`97fbb69`。

## 2026-07-30：Windows 测试版本探测不能递归执行测试二进制

- 症状：执行 `go test ./api/v2board ./limiter ./node` 时三个包的断言均显示 `ok`，但 `node` 包会持续派生 `node.test.exe`，最终因临时测试文件仍被占用而报 `Access is denied`，并造成短时本机进程与 CPU 压力。
- 影响链：节点测试 -> `node.localVersion()` -> `common/version.FromCommand()` -> 以 `version` 参数执行当前 `.test.exe` -> `TestMain` -> 再次运行测试。
- 根因：`TestMain` 只有同时存在 `V2NODE_TEST_VERSION_HELPER=1` 时才处理 `version` 参数；未设置该环境变量的测试路径会把版本探测子进程当完整测试再次执行。
- 修复：测试二进制只要收到首个 `version` 参数就输出测试版本并退出。该改动仅影响测试护栏，不改变正式 v2node 的版本探测与节点运行行为。
- 验证：设置 `GOEXPERIMENT=jsonv2` 后运行 `go test -count=1 ./api/v2board ./limiter ./node`，三个包全部通过，退出码为 0，且 `node.test` 残留进程数为 0。
- 下次先查：Windows 上若 Go 测试断言显示 `ok` 但退出清理失败，先检查 `node.test.exe` 数量与 `TestMain` 的命令参数分支，不要先归因于 Go 临时目录或杀毒软件。
- 相关文件与提交：`node/user_delta_test.go`、`node/update.go`、`common/version/version.go`；预约功能合入 `dev` 的节点提交为 `a1e14de`，测试护栏提交为 `6e637cb`。以上为本地分支状态，尚未推送或发布。

## 2026-07-31：用户同步追赶失败不能转换为 WebSocket 重连风暴

### 症状

用户同步唤醒灰度扩大后，面板 WebSocket 监听队列和连接数快速波动，节点连接反复建立和关闭，新节点长时间没有稳定 `user_sync_applied`。缩回原小批灰度并完整重启面板 Worker 后恢复。

### 影响链

面板 `user_sync_dirty` -> v2node `runWakeup` / `serveConnection` -> `syncUserState` -> 增量分页或强制全量同步 -> `UserSyncRetryError` / 临时 HTTP 错误 -> 关闭 WebSocket -> fallback 短轮询 -> 重连。

### 根因

- 拨号成功后，旧状态机先执行 `activatePush` 再开始读 WebSocket；初始追赶只要暂时失败，刚建立的连接就会被关闭。
- 读循环收到 dirty 后，`flushPending` 的任何同步错误都会直接退出读循环。正常的增量分页、全量同步熔断和面板短时繁忙因此都被错误地当成连接故障。
- 若只改成定时重试但不保护 Retry-After，新 dirty 又会把退避缩短到 merge 窗口，在高变更期重新制造追赶压力。

### 修复

- 拨号成功后立即进入读循环，首次追赶和 dirty 合并共用同步定时器。
- 普通同步错误和 `UserSyncRetryError` 保留当前连接；等待期间继续响应 ping、合并最高 revision，并按 fallback 间隔或 Retry-After 重试。
- 已生效的 Retry-After 不会被后续 dirty 缩短；dirty 只抬高待确认 revision。
- 只有拨号、读取、pong 写入、ACK 通道或 ACK 写入失败才退出连接并进入外层 fallback/重连。
- 用户状态提交成功且 ACK 写成功后才清除最高待确认 revision。

### 验证

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./node -run '^TestUserSyncRuntime' -count=1
go test ./node ./api/v2board -count=1
go vet ./node ./api/v2board
git diff --check
```

回归测试覆盖：同一连接内失败后重试、等待期间 pong、成功后 ACK 最高 revision、ACK 写失败触发重连，以及新 dirty 不缩短 Retry-After。Windows 当前 `CGO_ENABLED=0` 且没有 gcc，`go test -race` 需在 Linux 构建环境补跑。

### 下次优先检查

1. 区分“拨号成功”“进入 push”“ACK 前进”和“生产已验证”，不要只看二进制版本或 capability。
2. 同时采样 WebSocket established/listen backlog、节点 `mode`、`applied_revision`、Nginx upstream timeout、Webman worker 和 CPU。
3. 扩容按原 3 个节点、再加 3 个、再到 10/20 个分批；每批必须等 ACK 与连接数稳定后再继续。
4. 若连接稳定但追赶仍慢，继续检查 `force_full`、增量 `has_more`、全量同步熔断和面板用户缓存，不要重新把同步错误改成断线。

### 相关文件

- 运行时：`node/user_sync_runtime.go`
- 用户同步：`node/task.go`
- WebSocket 协议：`api/v2board/wakeup.go`
- 回归测试：`node/user_delta_test.go`
- 设计与计划：`docs/superpowers/specs/2026-07-31-user-sync-wakeup-stability-design.md`、`docs/superpowers/plans/2026-07-31-user-sync-wakeup-stability.md`

### v5.0.0.60 灰度补充：单次 HTTP deadline 不是运行时退出

- 症状：原 3 个灰度节点升级到 `v5.0.0.60` 后，小用户范围节点能够稳定进入 push；全量用户响应约 19.5 秒的节点仍按约 15 秒周期反复断开和重连。
- 根因：面板请求超过 v2node 默认 15 秒 HTTP timeout 时，Resty 返回 `context.DeadlineExceeded`。`serveConnection()` 用 `isContextError(err)` 同时判断父运行时退出和单次请求超时，因而把可重试的请求 deadline 错判为必须关闭 WebSocket。
- 修复：追赶失败后只在传入的父 `ctx.Err()` 非空时退出运行时；父上下文仍有效时，单次请求的 `context.Canceled` / `context.DeadlineExceeded` 与其他同步错误一样保留当前连接并按 fallback 间隔重试。读取、pong、ACK 等真实传输错误仍立即交给外层重连。
- 验证：新增回归先稳定失败于 `connection exited after request deadline`，修复后与普通追赶失败、ACK 写失败测试共同通过；`GOEXPERIMENT=jsonv2 go test ./...`、`go vet ./...` 和 `git diff --check` 通过。
- 下次先查：若只有大权限范围节点反复重连，先比较 `/UniProxy/user?force_full=1` 的服务端耗时与 v2node `DefaultNodeTimeout`，并区分“请求 deadline”与“父运行时已取消”；不要仅凭错误类型包含 `context deadline exceeded` 就断开 WebSocket。

## 2026-08-02：审计落盘结果等待会给新代理请求增加固定一秒延迟

### 症状

HK 高连接 Trojan 节点的 TCP/入口延迟仍在正常范围，但 FullTClash 的 HTTP 延迟从数百毫秒升到约 2.1–2.3 秒；同批非 Trojan 节点约为 1.3 秒，Trojan 额外增加接近 1 秒。节点日志每分钟同时出现 `SNTP access audit local persistence delayed` 和 `SNTP flow audit local persistence delayed`，错误为 `access audit persistence result pending`。

### 影响链与根因

`Trojan 认证成功` -> `DefaultDispatcher.routedDispatch` -> `logSntpUserAccess` -> `accessaudit.Enqueue` -> `Client.Enqueue` -> `persistBatcher.Submit` -> 等待 bbolt 批次结果最多 1 秒 -> `handler.Dispatch`。

普通访问审计位于真正出站转发之前。事件进入有界队列后，`persistBatcher.Submit` 仍同步等待 `request.result`；当 bbolt 单写者、上传 ACK 和磁盘同步不能在默认 `PersistTimeout=1s` 内返回时，请求虽然最终继续并且没有形成 persistence gap，但新代理请求已经被固定阻塞约 1 秒。FlowTraffic 的最终事件也使用同一等待模型，主要影响连接收尾。

现场的 `access.db` 约 35 MiB、`flow.db` 约 154 MiB，进程启动后累计物理写入约 294 GiB；短采样出现 10%–32% I/O wait 和阻塞任务。日志中有数千次限频后的 `persistence delayed`，但 `persistence failed`、OOM、panic、fatal 和流量上报任务超时均为 0。这说明本次是“同步等待落盘结果拖慢数据面”，不是已经确认的审计数据丢失。

### 证书排除

- 证书文件和节点配置早于本次延迟出现，延迟前最近的运行时变化是 v2node 二进制升级与重启。
- 按真实入口补齐 PROXY Protocol 后，在节点回环连续执行 20 次 TLS 握手，中位数约 2.86 ms。
- 节点直出 Cloudflare、Google 的 DNS/TCP/TLS/首字节多数在 2–60 ms；当前自签证书没有 OCSP URI 会产生 `ignoring invalid OCSP` 告警，但没有形成秒级 TLS 握手等待。

### 修复与止血边界

- 代码修复应把“事件已被有界队列接收”和“bbolt 最终提交结果”拆开：数据面在队列成功接收后立即继续，真实事务失败由现有整批 `OnFailure` 回调记录 gap；队列未接收、已关闭或已满时仍要明确计数，不能静默丢弃。
- 生产 A/B 止血可先备份配置，只关闭 `AccessAudit.FlowTraffic.Enabled` 并重启，保留普通访问审计，观察普通访问 `persistence delayed` 是否消失以及 HTTP 延迟是否恢复。未取得变更授权前不要执行。
- 不应把删除 spool、覆盖空库、关闭全部访问审计或更换证书作为首选修复；这些操作要么破坏证据，要么与已验证的阻塞点无关。

### 验证与下次优先检查

1. 同时采样 FullTClash TCP/HTTP 延迟、`journalctl -u v2node` 的两类 persistence warning、`access.db` / `flow.db` 大小和 `vmstat 1` 的 I/O wait。
2. 在修复版本中加入回归：让 `EnqueueBatch` 超过 1 秒才完成，断言 `logSntpUserAccess` 所在数据面不会等待事务结果，同时最终成功不记 gap、最终失败只记一次 gap。
3. 生产验证分开报告“配置已下发”“服务已重启”“延迟已恢复”“审计仍可补传”，至少连续观察 15 分钟。

ClickHouse 写入逻辑优化后的追踪再次证明，单次测速恢复不能作为修复完成依据：节点没有重启，日志入口健康检查约 0.52 秒且长连接发送队列为空，但两类 `persistence delayed` 在 45 分钟内仍每分钟出现；10 秒采样中进程物理写入约 32 MiB，折合约 3.75 MiB/s，I/O wait 多次达到 17%–23%。因此远端消费变快只能让个别批次或磁盘空档暂时低延迟，不能解除数据面同步等待本地事务结果的结构性问题。

相关文件：`core/app/dispatcher/default.go`、`common/accessaudit/client.go`、`common/accessaudit/flow_client.go`、`common/accessaudit/persist_batcher.go`、`conf/access_audit.go`。

### 已实施修复与本地验证

- `persistBatcher` 保留原有等待事务结果的 `Submit`，新增 `TrySubmit`：队列有容量时只完成有界接收就返回；队列已满返回 `ErrPersistenceQueueFull`；异步请求不创建结果 channel，后台单写入器仍按原批次执行 `EnqueueBatch`，并在真实事务失败时触发整批 `OnFailure`。
- 普通访问 `Client.Enqueue` 和 `FlowClient.Report` 已切换到 `TrySubmit`。因此 `logSntpUserAccess` 位于 `handler.Dispatch` 前不再意味着代理请求等待 bbolt；队列满或关闭时仍立即放行业务，并通过现有内存 gap 聚合记录失败数量、字节数和时间范围。
- 回归测试先在旧客户端实现上稳定失败：普通访问和 FlowTraffic 都出现 `waited for local transaction`，满队列出现 `waited instead of failing fast`；切换到非阻塞接收后这些测试通过。现有上传、补传、批次拆分和后台事务失败测试改为显式等待 spool 状态，不再依赖调用方同步等待落盘。
- `GOEXPERIMENT=jsonv2 go test ./... -count=1` 与 `GOEXPERIMENT=jsonv2 go vet ./common/accessaudit ./core/app/dispatcher` 通过。全仓回归发现并修正了 `node` 状态上报测试的旧同步假设：测试现在等待两类事件进入持久化 spool 后再校验 pending 字段。Windows 本机未安装 `gcc`，`go test -race ./common/accessaudit` 因 `-race requires cgo` / `C compiler "gcc" not found` 未能执行，部署前应在带 CGO 工具链的 Linux CI 或构建机补跑。
- 对应提交：`0de78ac`（批处理非阻塞接收）和 `6b95752`（两类客户端迁移）。当前只完成代码与本地验证，尚未部署生产；上线后仍需按前述步骤连续观察至少 15 分钟，并分别确认延迟恢复、审计落盘和补传状态。

## 2026-08-13：多 NodeID 机器的受管证书凭证必须按面板和 NodeID 隔离

### 症状与影响链

同一台机器可运行多个 NodeID，同一 NodeID 也可部署到多台实例。旧流程要求把后台只显示一次的 `TlsCertificateToken` 手工写入每台实例；漏配时节点继续使用旧 `http` / `dns` ACME 路径，证书失败又可能触发整个多节点进程被 systemd 反复拉起。

影响链：后台创建证书作用域 -> NodeID 凭证生成 -> 每个实例手工配置 -> 节点切换 `cert_mode=managed` -> HMAC 拉取共享证书。手工步骤既容易漏掉新实例，也容易把同机其他 NodeID 的凭证误用过来。

### 根因与修复

- 证书共享边界是作用域，访问凭证边界是 NodeID，机器不是任何一者；因此不能选“这台机器上的某个节点凭证”作为全机共享凭证。
- 新节点能力 `managed_tls_auto_credential_v1` 通过既有面板 `ApiKey + NodeID + instance_id` 身份，在配置进入 `managed` 后领取本 NodeID 的凭证。
- 本地凭证按规范化 `ApiHost + NodeID` 生成独立文件名，保存于 `/etc/v2node/credentials`；目录权限 `0700`、文件权限 `0600`，使用同目录临时文件和原子替换。相同 NodeID 的多个实例领取同一个面板凭证，不同 NodeID 或不同面板绝不复用。
- 显式 `TlsCertificateToken` 保持最高优先级，兼容旧部署。自动凭证收到受管证书接口 `401` 时只刷新并重试一次；并发请求发现其他协程已换新后直接复用，避免形成领取风暴。
- 首次领取失败后，即使节点配置正文未变化，后续配置轮询仍会重试；状态快照动态读取当前凭证指纹，避免领取成功后继续上报旧的“未配置”状态。

### 验证、下次检查与相关文件

- 临时回归覆盖：同机 NodeID 隔离、不同面板隔离、落盘重载、显式 Token 优先、401 单次刷新、配置正文未变化时继续领取。
- `GOEXPERIMENT=jsonv2 go test ./... -count=1`、`go vet ./api/v2board ./node`、`git diff --check` 通过；临时测试文件已按仓库约定删除。
- 生产验证先看 `/etc/v2node/credentials/node-<NodeID>-<ApiHostHash>.json` 的权限和 NodeID，再看状态上报指纹、受管证书版本以及旧 ACME 日志是否停止。日志和文档不得记录明文凭证。
- 相关文件：`api/v2board/managed_tls_credential.go`、`api/v2board/managed_tls.go`、`api/v2board/node.go`、`api/v2board/panel.go`、`node/managed_tls_manager.go`。
- 对应实现提交：`e0321fe`。

## 2026-08-18：托管 SNI 切换不能误触发全量 Xray 重载

### 症状与影响链

- 托管域名迁移门禁和真实入口探测均已通过，但执行入口切换后目标端口约 22 秒不可用。
- systemd 的主 PID 和 `NRestarts` 都没有变化，服务日志却出现所有节点任务取消、目标端口消失以及 `Xray ... started` 再次出现。
- 完整影响链为：v2board 切换作用域主域名 -> `/api/v2/server/config` ETag 变化 -> v2node `nodeInfoMonitor` 收到新 `NodeInfo` -> 全局 `ReloadCh` -> 同进程所有 Controller 和 Xray Core 重建。

### 根因与修复

- 原配置监控只区分“配置是否变化”，没有区分 managed TLS 的纯 SNI 元数据变化与真正影响入站监听器的运行时变化。
- TLS 入站构造在 managed 模式下只使用已激活的证书/私钥路径和 `RejectUnknownSNI`，不会用 `TlsSettings.ServerName` 或 `ServerNames` 构造监听器；双 SAN 证书已经激活后，切换权威域名无需重建入站。
- 新路径只接受严格的纯域名变化：当前和新配置都必须是同一 managed TLS 节点；忽略 `ServerName`、`ServerNames` 和派生 `CertDomain` 后，其余字段必须完全一致。
- `managedTLSManager` 在串行边界内先验证当前本地证书的 scope、目标 SAN 和有效期，再保存新离线快照，最后提交权威域名。SAN 校验或快照保存失败时不改变管理器域名、控制器信息或当前运行时，并保留 pending 等待重试。
- 端口、协议、NodeID、tag、证书作用域、证书模式、`RejectUnknownSNI` 或任何其他字段同时变化时，仍沿用原有的持久化后全量重载路径。

### 验证与下次优先检查

- 回归测试覆盖：纯域名变化不写 `ReloadCh`；其他配置变化仍写 `ReloadCh`；目标 SAN 缺失不落盘；快照保存失败不推进管理器或控制器状态；管理器在证书协调与域名提交之间使用同一串行边界。
- `GOEXPERIMENT=jsonv2 go test ./... -count=1` 与 `go vet ./node ./core` 已通过。Windows 本机没有 `gcc`，`go test -race ./node` 因无法启用 cgo 未执行成功，需在带 CGO 工具链的 Linux CI 或构建机补跑。
- 本地检查入口：`node/task.go` 的 `applyPendingNodeInfo`、`node/managed_tls_domain_switch.go` 的严格分类和原地应用、`node/managed_tls_manager.go` 的 `CommitDomain`、`cmd/server.go` 的全局 reload。
- 上线验证不能只看 PID 和 `NRestarts`；还要连续检查目标端口、日志中是否再次出现 `Xray ... started`，并确认出现 `Managed TLS domain applied without global reload`。
- 相关设计与计划：`docs/superpowers/specs/2026-08-18-managed-tls-runtime-activation-design.md`、`docs/superpowers/plans/2026-08-18-managed-tls-domain-switch-in-place.md`。
- 对应实现提交：`b13cd1f`、`c20566d`、`336aec6`。
