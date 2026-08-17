# v2node 失效用户连接立即回收设计

## 状态

- 日期：2026-08-17
- 用户选择：用户到期、流量耗尽、封禁或暂停生效后，立即硬断全部现有转发连接，不设置宽限期
- 适用范围：v2node 用户授权生命周期、Xray dispatcher 连接登记与用户删除
- 当前阶段：设计已确认，尚未实施

## 问题与根因

当前正常删除链已经存在：v2board 流量入账跨过额度后生成 `user_delete`，v2node 本地到期调度也会按 `expired_at` 移除用户；两条路径最终都进入 `Controller.applyUserList -> V2Core.DelUsers -> LinkManager.CloseAll`。

问题不在于完全缺少删除，而在于连接登记和用户删除没有共享同一个原子生命周期边界：

1. dispatcher 的两个入口使用 `sync.Map.Load` 后创建 `LinkManager` 并 `Store`。同一用户首次并发建连时，多个 goroutine 可同时创建 manager，后写者覆盖前者；被覆盖 manager 上的连接无法再由 `DelUsers` 找到。
2. `LinkManager.CloseAll` 只清空调用时已经登记的连接，没有 closed 或 tombstone 状态。并发 `AddLink` 可以在清空后重新加入连接。
3. `DelUsers` 在关闭后删除全局 manager entry。已经通过认证和 limiter 的在途请求仍可能在删除窗口重新创建 entry，之后没有第二次删除事件；持续有流量的连接可以长期存活。
4. 旧的 `FlowTraffic` 同步 bbolt 入队曾阻塞连接收尾，但当前源码已使用有界非阻塞接收。本设计保持该边界，不让审计落盘重新进入硬断路径。

## 成功语义

从用户失效开始处理后，系统必须满足以下合同：

- 所有已登记的用户转发连接立即收到 close/interrupt。
- `DeactivateUser` 返回后，不存在任何仍可继续转发的旧连接。
- 已完成认证但尚未登记的在途请求不能创建或复活连接槽位。
- 后续新请求不能因为 dispatcher 自动建 manager 而绕过失效状态。
- 用户重新授权时只能建立全新的连接生命周期；旧连接的延迟回调不能影响新连接。
- 关闭逻辑不得等待 FlowTraffic、普通访问审计、bbolt 或远端上报。

“立即”指 v2node 应用用户删除或本地到期事件后立即执行本地硬断，不包含面板流量上报、`traffic:update` 入账和用户同步事件到达之前的控制面延迟。

## 方案比较

### 采用：权威 `LinkRegistry`

由 dispatcher 持有一个明确的 `LinkRegistry`。只有用户授权路径可以激活用户槽位；代理请求只能向已经激活的槽位登记连接，不能自行创建槽位。用户删除通过同一 registry 原子移除槽位并摘出全部连接。

该方案同时关闭首次创建覆盖、`CloseAll`/`AddLink` 并发和删除后 entry 复活三个竞态，且无需永久保存 tombstone。

### 不采用：只替换为 `LoadOrStore`

`LoadOrStore` 只能保证首次创建得到一个 manager，不能阻止 `CloseAll` 之后的 `AddLink`，也不能阻止 `DelUsers.Delete` 之后的在途请求重新创建 entry。

### 不采用：在现有 manager 上增加 closed/generation 补丁

closed 和 generation 可以做成正确实现，但在原始 `sync.Map` 周围还要处理墓碑清理、重新授权和并发删除，状态分散且更难证明。把创建权限收口到 registry 更小、更清晰。

### 暂不采用：把每个用户的 cancel context 贯穿 Xray 入站

用户级 context 可以进一步关闭物理入站 carrier，但需要侵入更多 Xray/MUX 生命周期和协议实现。当前故障目标是立即关闭用户逻辑代理流与下游转发连接；权威 registry 已能解决转发机 TCP 和连接资源滞留。

## 组件与接口

### `LinkRegistry`

`LinkRegistry` 放在 `core/app/dispatcher`，内部使用 `sync.RWMutex` 保护 `map[string]*LinkManager`。

核心接口：

- `ActivateUser(user string)`：为已授权用户创建槽位；用户已激活时幂等返回，不替换现有 manager，不中断正常连接。
- `RegisterLink(user string, writer buf.Writer, reader buf.Reader, source string) (*ManagedWriter, bool)`：只向已激活槽位登记；未激活时在锁外立即关闭 writer、interrupt reader，并返回拒绝。
- `DeactivateUser(user string) int`：原子删除用户槽位、摘出全部连接，释放 registry 锁后关闭连接，返回实际关闭数量。
- `CloseUserIP(user, ip string) int`：只查询现有槽位并关闭匹配来源 IP，不创建新槽位。

锁顺序固定为 `LinkRegistry.mu -> LinkManager.mu`：

1. `RegisterLink` 持 registry 读锁完成槽位查找和 manager 登记。
2. `DeactivateUser` 必须取得 registry 写锁，因此会等待已进入的登记完成；随后删除槽位并摘出全部连接。
3. 删除完成后新的登记只能看到“用户不存在”并被立即关闭。
4. 实际 I/O close/interrupt 在 registry 锁外执行，避免慢 socket 阻塞其他用户建连。
5. `ManagedWriter.Close` 只访问所属旧 manager。用户重新授权后使用新 manager，旧连接的延迟关闭不会删除新生命周期里的连接。

### dispatcher

`DefaultDispatcher` 不再暴露或直接操作 `LinkManagers sync.Map`。两条现有登记路径统一调用 `LinkRegistry.RegisterLink`，拒绝时沿用现有关闭上下行和返回错误的方式。

dispatcher 不具备 `ActivateUser` 权限，也不得在用户不存在时隐式创建槽位。这是防止失效用户复活的核心约束。

### 用户授权与删除

`V2Core.AddUsers` 在凭据加入成功时激活对应 registry 槽位。批量新增部分失败时，既有凭据回滚和 registry 激活状态必须一起回滚；正常重复同步不得替换已激活 manager。

`V2Core.DelUsers` 按以下顺序处理整批用户：

1. 先对整批用户逐一调用 `DeactivateUser`，从数据面立即封死全部用户并硬断现有连接；不得先等待某个认证清理完成再关闭下一个用户。
2. 从 Xray 或自定义入站删除认证用户；“未找到”按幂等成功处理，其他清理错误记录为告警但不重新开放数据面。
3. 清理 V2Core 的 UID 映射和流量计数器。
4. 记录本次实际关闭数量；日志只保留 node tag、UID、数量和通用原因，不记录原始 UUID、IP、token 或目标地址。

`V2Core.DelUsers` 成功返回后，现有 `Controller.applyUserList` 继续通过 `limiter.UpdateUser` 清理 limiter 用户状态，再更新并持久化用户快照。即使认证清理失败，V2Core 也必须保持 fail-closed 并允许这条既有提交链继续前进。

registry 是数据面最终准入门。认证用户清理失败时不得重新开放旧连接，也不得阻止本地用户快照提交失效状态，否则进程重启可能从旧离线快照重新授权该用户。残留认证记录只存在于当前内核内存中，registry 和 limiter 已删除后不能建立转发，节点重建时会自然清除。重新授权必须显式经过 AddUsers/ActivateUser；若底层仍存在相同用户凭据，AddUsers 应把它作为幂等已安装状态处理或先替换后激活，不能由代理请求自行恢复。

## TCP、UDP、MUX 与审计边界

- TCP：关闭被管理 writer 并 interrupt 对端 reader，使上下行复制循环退出并关闭下游转发 TCP。
- UDP：关闭该用户对应的逻辑 link 和读写循环，失效后不得继续建立转发。
- MUX：关闭该用户当前所有逻辑代理流和下游连接；物理入站 carrier 可以暂时存在，但新的子流会在 limiter 或 registry 处被拒绝，不能再创建转发机连接。
- SNTP Eclipse、Mieru 和标准 Xray 用户删除都必须经过同一 registry 数据面门闩；各协议自己的认证用户表继续按现有方式清理。
- FlowTraffic 强制关闭时仍只生成一次 final。`FlowClient.Report` 保持 `TrySubmit` 非阻塞语义，队列满或关闭只记录 gap，不延迟 socket 回收。

## 并发正确性

设计需要建立三个 happens-before 边界：

1. `ActivateUser` 完成后，后续登记可以看到唯一有效 manager。
2. 已经取得 registry 读锁的登记必须在 `DeactivateUser` 取得写锁前完成；这些连接会被同一次摘取覆盖。
3. `DeactivateUser` 删除 map entry 并释放写锁后，所有后续登记只能拒绝，直到明确的 `ActivateUser` 创建新 manager。

不依赖 sleep、最终一致性轮询、重复删除事件或 Xray idle timeout 来达到回收正确性。

## 错误处理与可观测性

- `ActivateUser`、`DeactivateUser` 和 `CloseUserIP` 必须幂等。
- 关闭单条连接失败不能阻止继续关闭同用户其余连接；关闭结果按实际摘取数量统计。
- registry 不可用或用户未激活时，dispatcher fail-closed，并立即释放当前 link。
- 用户删除日志记录 `tag`、`uid`、`closed` 和 `source=panel_sync|local_expiry`；无法可靠区分到期、耗尽、封禁或暂停时使用通用 `user_invalidated`，不猜测具体业务原因。
- 对删除后在途登记的拒绝使用限频 debug/warn 或内部计数，避免恶意重连制造日志洪泛。
- 本次不新增 v2board 数据库字段、管理端页面或跨服务状态协议。

## 测试设计

遵守先失败、后实现的顺序，并把回归加入现有测试文件，不留下临时测试文件。

### registry 单元合同

1. 未激活用户登记连接：立即关闭 writer、interrupt reader，registry 保持为空。
2. 激活用户后登记多个连接：全部进入同一个 manager。
3. 停用用户：全部连接关闭，返回准确数量，entry 被删除。
4. 重复停用：不 panic，关闭数量为零。
5. 停用完成后登记：立即拒绝，不能复活 entry。
6. 删除后重新激活：新连接可登记；旧 manager 的延迟 `ManagedWriter.Close` 不影响新连接。

### 确定性并发回归

1. 使用 channel/barrier 控制多个 `RegisterLink` 与 `DeactivateUser` 同时发生，不用 `time.Sleep` 猜竞态。
2. `DeactivateUser` 返回后断言全部测试端点已关闭、entry 不存在、后续登记全部拒绝。
3. 同一用户高并发首次登记只能使用授权路径创建的唯一 manager。
4. 在底层 `RemoveUser` 被阻塞期间发起新登记，断言 registry 已先关闭并拒绝。
5. 重复执行并在 Linux CI 使用 `go test -race`，验证没有数据竞争和语义漏关。

### 集成回归

1. 本地 `expired_at` 调度删除用户时，现有连接立即关闭。
2. 面板 `user_delete` 增量同步时，现有连接立即关闭。
3. 删除后 upsert/重新授权可以建立新连接，旧连接不能恢复。
4. 底层 `RemoveUser` 返回非“未找到”错误时，用户快照仍提交失效状态，registry/limiter 保持关闭，旧凭据不能建立转发。
5. 两条 dispatcher 连接登记路径都经过 registry。
6. TCP、UDP/MUX 逻辑 link、blocked IP 定向关闭和普通自然关闭保持正确。
7. FlowTraffic final 只发送一次，且阻塞的审计 spool 不延迟用户硬断。
8. 现有流量计数、设备限制、在线状态和用户同步测试保持通过。

目标验证命令：

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core/app/dispatcher ./core ./limiter ./node ./common/accessaudit -count=1
go vet ./core/app/dispatcher ./core ./limiter ./node ./common/accessaudit
git diff --check
```

Linux CI 或构建机补充：

```bash
GOEXPERIMENT=jsonv2 go test -race ./core/app/dispatcher ./core ./node
```

按仓库规则不默认运行完整构建、开发服务或长期 watcher。

## 灰度与生产验证

1. 核对生产二进制包含本修复提交和既有非阻塞审计提交，不用配置变更代替版本确认。
2. 选择一台可回滚节点和一个测试用户，建立到受控目标的长 TCP 连接并记录 v2node、转发机连接基线。
3. 分别测试本地到期和面板流量耗尽/`user_delete`；从节点应用事件开始计时，约一秒内转发机对应 `ESTABLISHED` 应归零。
4. 确认旧连接无法继续传输，删除后的新建请求被拒绝，重新授权后新连接恢复。
5. 连续观察至少 15 分钟：TCP state、goroutine、RSS、FD、FlowTraffic pending/gap、panic、OOM 和服务重启次数均无异常增长。
6. 回滚只替换二进制并重启服务；本修复不改变配置、spool、面板 API 或数据库格式。

## 成功标准

- 用户删除或本地到期处理完成后，测试用户的全部下游转发连接立即关闭。
- 删除与并发登记的确定性测试证明 entry 不会复活，后续请求全部 fail-closed。
- 重新授权创建独立新生命周期，旧连接回调不影响新连接。
- 高并发测试和 Linux race 检查通过，没有新增锁死、无界 goroutine 或连接登记热点。
- FlowTraffic 和普通访问审计不进入连接关闭等待路径。
- 生产灰度观察期内，转发机 TCP、v2node RSS/goroutine/FD 能随连接关闭回落。

## 非目标

- 不缩短 v2board 流量上报、分钟入账或用户同步控制面延迟。
- 不新增流量额度本地实时计算。
- 不保证强制关闭多用户共享的物理传输 carrier；保证失效用户的逻辑代理流和下游转发连接立即关闭且不能新建。
- 不改动 v2board 数据库、API 协议、管理端或客户端。
- 不借本次修复重构无关的 limiter、审计、路由或证书逻辑。
