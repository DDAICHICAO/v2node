# v2node 路由规则零断线热更新设计

## 背景

当前管理端保存路由规则时，v2board 只更新数据库，不会直接向节点发送重启命令。但是，被该规则关联的节点在下一次拉取 `/api/v2/server/config` 时会收到新的响应体和 ETag。v2node 的 `nodeInfoMonitor` 除托管 TLS 域名专用热更新外，会把任何 `NodeInfo` 变化写入进程级 `ReloadCh`。

主循环收到该信号后会关闭同一进程内的全部 Controller 和整个 Xray Core，再顺序重新拉取并启动全部 NodeID。因此，一台机器对接 N 个节点时，一次路由编辑会造成以下问题：

- 所有 NodeID 的监听器和已有连接一起被关闭；
- 多个节点先后发现同一次配置变化时，可能继续排队触发重复全局重载；
- 重建全局 DNS、Router、Outbound、Inbound 和用户状态造成不必要的 CPU 峰值；
- 用户会经历真实连接中断，新连接在重启窗口内也可能失败。

路由配置由 `core.GetCustomConfig(infos)` 根据同机全部 `NodeInfo` 统一生成，包含全局 DNS、Router 和 Outbound，不能只在某一个 Controller 内独立替换一条规则。另一方面，仓库内自定义 Dispatcher 是所有新流量选择 Router 和 Outbound 的统一边界，可以在不维护外部 Xray fork 的前提下承担运行代次切换。

## 已确认的系统约束

- v2board 的路由保存事务完成后，所有相关节点配置在数据库中同时可见；节点配置接口按 NodeID 重新装配路由并计算响应 ETag。
- `cmd/server.go` 中的 `ReloadCh` 和重载流程属于整个 v2node 进程，不属于单个 NodeID。
- `core.GetCustomConfig` 需要同机全部 `NodeInfo`，路由热更新必须由机器级协调器统一编译。
- Xray Router 的规则替换使用写锁，但 `PickRoute` 读取规则时没有对应读锁。不能在生产流量中原地调用 `ReloadRules` 修改正在使用的 Router。
- Xray Outbound Manager 添加重复 tag 会失败，移除 handler 也不等于安全结束已经引用该 handler 的连接。
- Xray 系统拨号器在 Core 启动时捕获 DNS Client。若要更新 DNS 而不重启 Core，必须从首次启动开始注入一个地址稳定、后端可切换的 DNS 代理。
- Dispatcher 的 `handler.Dispatch` 返回不能直接等同于连接结束。旧代次引用必须绑定到实际 Link 生命周期，而不是函数返回时间。

## 目标

- 进程、操作系统和网络本身正常时，纯路由规则变化不关闭任何已有 TCP 或 UDP 会话。
- 路由切换期间的新连接失败数为零；每条新连接只能使用完整可用的旧运行配置或新运行配置。
- 同一进程内 N 个 NodeID 因同一次路由编辑发生变化时，只编译并发布一个新运行代次。
- 热更新期间不停止 Xray Core、Inbound、Controller、心跳、用户同步或流量上报任务。
- 候选配置有任何错误时继续使用上一版配置，保留待应用状态并自动重试，绝不自动降级为全局重载。
- 对旧代次执行连接感知的延迟回收，既不提前关闭长连接，也不永久泄漏已经没有引用的资源。
- 不修改 v2board API、数据库结构或外部 Xray fork。

## 非目标

- 本阶段不把端口、协议、传输、证书作用域、用户权限等其他 NodeInfo 变化改造成零断线热更新。
- 不保证进程退出、系统重启、机器故障、网络故障或人为停止服务时连接不断。
- 不改变路由规则的数据模型、管理端编辑方式或 v2board 的 ETag 语义。
- 不为了回收旧代次而给长连接设置强制关闭期限。
- 不把热更新失败掩盖成“部分规则已生效”；候选只能完整发布或完全不发布。

## 零断线语义

本设计中的零断线包含两个同时成立的条件：

1. 切换前已建立的连接继续使用其创建时取得的运行代次，直到连接自然结束。
2. 切换期间到达的新连接可以在极短的发布临界区等待，但不能因 Router、DNS 或 Outbound 暂时缺失而失败。

运行配置发布具有代次线性化点。Dispatcher 在该点之前取得旧代次，在该点之后取得新代次。已取得代次的连接不会在中途自动迁移，也不会因后续代次发布而失效。

## 变更分类

新增统一的 `NodeInfo` 变更分类，按以下优先级返回一种结果：

1. `unchanged`：配置没有变化。
2. `managed_tls_domain_only`：严格满足现有托管 TLS 域名原地更新条件，继续走现有专用路径。
3. `routes_only`：忽略 `Common.Routes` 后，其余运行字段完全一致。
4. `full_reload`：包含任何其他字段变化，沿用现有进程级重载。

只有同一批次内所有变化节点都属于 `routes_only`，其余节点均为 `unchanged` 时，才进入路由热更新。路由与端口、协议、证书域名或其他字段同时变化时不拆分提交，避免 Controller 权威状态与实际运行时产生半套状态。

## 配置状态分层

每个 Controller 不再用一个对象同时表达“面板最近返回”和“运行时已经采用”，而是维护三个状态：

- `active`：当前运行代次正在使用的 NodeInfo；
- `observed`：最近一次成功从面板取得的 NodeInfo 及内容版本；
- `pending`：已经观察到但尚未成功发布的候选配置。

内容版本使用响应体哈希作为权威比较值，ETag 只用于 HTTP 条件请求。面板 Client 的拉取过程增加每 Controller 互斥，普通轮询和协调器主动刷新必须经过同一个入口，避免 ETag、响应体哈希和缓存对象并发写入。

若一次新响应已经更新了 HTTP ETag，但候选构建失败，`pending` 仍会保留完整 NodeInfo。后续面板返回 `304 Not Modified` 时继续重试该 pending 配置，不能把它误判为没有工作。

## 机器级路由更新协调器

### 职责

新增一个与 `V2Core` 和同机全部 Controller 同生命周期的 `RouteUpdateCoordinator`，负责：

- 接收任意 Controller 报告的 `routes_only` 候选；
- 主动刷新当前进程配置的全部 NodeID；
- 判断整机配置向量是否稳定；
- 串行编译、验证和发布运行代次；
- 合并构建期间继续到达的新配置；
- 管理重试、最后可用快照和运行指标。

协调器只允许一个构建任务执行，事件入口只更新“最新观察版本”和 dirty epoch，不为每个 NodeID 建立一个无界任务。这样 N 个节点同时变化不会形成 N 次构建或 N 次重载。

### 稳定配置向量

任意节点首次报告纯路由变化后执行以下流程：

1. 开启默认约 500 毫秒的稳定窗口，旧运行代次继续正常服务。
2. 以有界并发刷新同机全部 NodeID；同一个 Client 内部仍严格串行。
3. 记录 `NodeID -> body hash` 配置向量以及完整 observed NodeInfo。
4. 经过短暂静默期再次刷新全部 NodeID。
5. 两次向量一致时进入编译；若向量继续变化，则重置稳定窗口并使用最新版重试。

主动全量刷新使同一次数据库事务影响的全部 NodeID 在其常规轮询到达前就被收集。双向量确认避免刷新过程恰好跨过管理员的第二次编辑而发布混合时点配置。持续编辑时不阻塞线上流量，只延迟新规则生效。

若任意 NodeID 拉取失败、缺少有效 observed 配置，或出现 `full_reload` 类型变化，本轮路由候选不得发布。拉取失败保留旧运行代次并退避重试；非路由变化交回现有变更处理逻辑。

### 构建期间的新版本

编译开始时捕获 dirty epoch 和稳定配置向量。候选发布前再次比较：

- epoch 和向量未变化：允许发布；
- 发现更新：销毁尚未发布的候选，直接从最新 observed 状态重新编译；
- 发布线性化点之后才到达更新：形成下一次热切换，但仍不会关闭连接。

## 不可变运行代次

新增 `RouteRuntimeGeneration`，至少包含：

- 单调递增 generation ID；
- 完整、不可变的 Router；
- 该代次使用的 DNS 后端；
- 逻辑 Outbound tag 到物理 handler 的不可变映射；
- 该代次引用的共享 Outbound 资源；
- 活跃 Link 和 DNS 查询引用计数；
- retired 状态、创建时间和诊断信息。

正在使用的 Router 不允许调用 `ReloadRules`、`AddRule` 或 `RemoveRule`。每次变化都在旁路构建一个新的 Router，初始化完成后才可能进入 active 状态。

## Outbound 版本化与复用

面板规则继续使用原有逻辑 tag。编译器为运行时生成不可冲突的物理 tag，并重写 Router、Balancer、`proxySettings.tag`、`dialerProxy` 等内部 tag 引用。物理 tag 包含逻辑 tag 和规范化配置哈希，例如：

```text
proxy-a -> proxy-a@7f3a91c2
```

发布前必须验证所有规则和 Outbound 内部引用都能解析到候选资源。存在重复逻辑 tag、缺失引用、循环依赖、非法 JSON 或 handler 构建错误时，整个候选失败。

Outbound 资源池按“逻辑 tag + 规范化配置哈希”复用完全一致的 handler，并对运行代次和活跃连接分别计数。配置未变化的默认直连、拦截和自定义出口不重复创建；真正变化的出口先构建、注册并启动，之后 Router 才能被发布。

如果候选构建失败，只释放候选新建的资源；被当前或其他代次共享的 handler 不受影响。Xray Outbound Manager 中的物理 handler 只有在所有代次和活跃连接引用都归零后才移除并关闭。

## DNS 热切换

Core 首次启动时注册一个地址稳定的 `RuntimeDNSClient`，Xray 系统拨号器始终持有该代理，不再直接捕获某一版具体 DNS 配置。

每个运行代次旁路构建并验证自己的 DNS 后端：

- 新 Router 使用该代次的 DNS Client；
- DNS Outbound 使用对应代次的 DNS 后端；
- 系统拨号器通过 `RuntimeDNSClient` 获取当前 active DNS 后端；
- 每次查询开始时取得 DNS lease，查询完成后释放；
- 切换前已开始的查询继续使用旧后端，新查询使用新后端，代理指针永不为空。

DNS 后端切换和 active 运行代次发布由同一个 Runtime Manager 临界区完成。即使旧连接在切换后才触发系统级域名解析，也只会使用一个完整可用的 DNS 后端，不会遇到 DNS feature 被移除或尚未注册的空窗。

## Dispatcher 原子发布

自定义 Dispatcher 不再直接持有可变的 `router` 和裸 Outbound Manager 作为路由决策来源，而是通过 `RouteRuntimeManager` 获取代次 lease。

### 获取

`Acquire` 使用短临界区完成以下操作：

1. 读取 active generation；
2. 在允许回收前增加引用；
3. 返回包含 Router、Outbound resolver 和 generation ID 的 lease。

不能使用“先原子读取指针、稍后再增加引用”的两步无锁实现，否则发布线程可能在两步之间把旧代次回收。实现可以使用小粒度读写锁或等价的安全 RCU 获取协议；该临界区不执行网络或构建工作。

### 发布

候选发布顺序为：

1. 编译并严格校验 DNS、Router 和全部 Outbound 引用；
2. 注册并启动候选需要的新物理 handler；
3. 再次确认配置向量和 dirty epoch 未过期；
4. 在 Runtime Manager 写临界区内同时切换 active generation 和 active DNS 后端；
5. 将旧 generation 标记为 retired；
6. 更新所有 Controller 的 active NodeInfo；
7. 异步持久化整机最后可用快照并尝试回收旧代次。

发布临界区内不关闭任何资源。新连接在临界区前取得旧代次，或在临界区后取得新代次，不会看到“新 Router + 缺失 Outbound”的中间状态。

### Link 生命周期

代次 lease 必须覆盖真实连接生命周期，而不是只覆盖 `PickRoute` 或 `handler.Dispatch` 调用：

- Dispatcher 为每条 routed Link 安装独立于 access audit 和用户限制器的生命周期包装；
- 上下行结束、连接上下文取消或 Link 明确关闭时，通过 `sync.Once` 释放 generation lease；
- 嵌套 Dispatch 和内部 detour 通过 context 继承同一 generation，避免旧连接中途跳到新代次；
- 强制 Outbound、默认 Outbound 和正常 Router 命中都必须经过同一 resolver；
- 生命周期包装与现有 `LinkRegistry` 可以协作，但不能依赖用户是否启用审计或是否具有用户标识。

只有 retired generation 的全部 Link 和 DNS 查询引用归零后，才能关闭其不再共享的 Router、DNS 和 Outbound 资源。

## 严格编译与校验

当前 `GetCustomConfig` 的部分分支在路由 JSON 解析或 Outbound Build 失败时会 `continue`，可能静默省略错误规则。热更新路径必须改为严格编译：

- 错误携带 NodeID、规则序号、action 和字段位置，但不记录完整 `action_value`；
- 任意规则无法解析或引用无法闭合时，候选整体失败；
- 编译必须是确定性的，相同配置向量产生相同规范化配置哈希；
- 编译不得修改当前 Router、DNS、Outbound Manager 或 Controller active 状态；
- 候选启动验证产生的副作用必须登记，失败时按逆序清理。

严格编译同时用于最后可用快照恢复。进程首次启动且没有最后可用快照时，若面板配置无效，应明确启动失败，不能静默运行残缺路由。已有最后可用快照时，可保持其配置并持续重试面板候选。

## 最后可用快照

路由运行状态按整机配置向量持久化，避免各 NodeID 分别写入后形成互不一致的离线恢复状态。快照包含：

- schema version；
- generation ID；
- 全部 NodeID 的已验证 NodeInfo；
- 每个 NodeID 的内容版本；
- 整体配置向量哈希；
- 成功发布时间。

快照通过临时文件、刷盘和原子重命名写入，不覆盖现有文件到一半。只有候选已经成功发布后才把它提升为最后可用快照。若发布成功但持久化失败，运行时继续使用新代次并将持久化标记为 degraded，后台重试；不能为了磁盘错误回滚或断开连接。

进程重启本身不属于零断线范围。重启恢复时仍需严格验证快照；面板可用时继续拉取最新 observed 配置并通过相同编译器升级。

## 失败与恢复

### 发布前失败

以下情况均保持 active generation 不变：

- 任一 NodeID 拉取失败；
- 配置向量尚未稳定；
- 路由或 DNS 解析失败；
- Outbound 构建、注册或启动失败；
- Router 初始化或引用闭合校验失败；
- 候选在发布前已经过期。

候选资源按逆序清理，pending 配置保留。重试采用带随机抖动的指数退避，最高约 30 秒。新配置到达时立即打断旧退避并合并到最新版。

### 发布后失败

发布线性化点之后不执行会使 active generation 失效的回滚：

- 快照写入失败：保留新运行代次并重试持久化；
- 旧资源回收失败：保留资源并重试，记录告警；
- 指标或日志写入失败：不得影响流量路径；
- 热更新工作协程 panic：在边界恢复，记录失败并重建协调循环，当前 active generation 保持不变。

热更新失败绝不自动写入 `ReloadCh`。人工紧急开关可以让后续变更恢复旧全局重载语义，但该行为必须由运维明确启用。

## 资源回收策略

严格零断线意味着某条永不结束的连接可能长期保留其旧代次。设计接受这一必要成本：

- 不设置强制关闭期限；
- 相同 Outbound 按配置哈希跨代次共享，减少正常编辑造成的重复资源；
- retired generation 引用归零后立即异步清理；
- 暴露 retired 数量、最老存活时间、活跃 lease 数和资源池大小；
- 超过阈值只告警，不主动断开连接。

若配置被高频修改且每一版都有永不结束的连接，资源占用理论上无法在“绝不主动断线”的前提下被强制封顶。运维需要通过指标定位异常长连接或异常编辑频率，而不是让程序静默破坏承诺。

## 可观测性

增加结构化日志和指标，至少覆盖：

- 热更新触发、合并、构建、发布、失败和重试次数；
- 首次观察到发布完成的延迟；
- 一次批次包含的 NodeID 数量；
- active generation ID、retired generation 数量和最老年龄；
- 活跃 Link lease、DNS lease 和 Outbound 资源数量；
- 候选因版本过期而被废弃的次数；
- 最后可用快照的写入状态。

日志允许记录 NodeID、generation ID、配置向量哈希前缀、规则序号和 action 类型。不得记录完整路由匹配内容、Outbound JSON、代理账号、密码、面板 token、用户标识或订阅凭据。

节点配置轮询、心跳和流量任务在热更新期间持续运行，因此面板 `last_check_at` 不应因路由切换停止刷新。

## 配置与紧急回退

增加默认开启的 `EnableRouteHotReload` 开关：

- 开启：纯路由变化走本设计；失败时保持上一版并重试。
- 关闭：后续变化沿用现有 `ReloadCh` 全局重载，明确接受连接中断。

该开关只作为人工紧急回退手段。程序不能因一次候选失败自动关闭开关，也不能在没有日志和指标的情况下静默退回全局重载。

稳定窗口、刷新并发和最大退避先使用保守内部默认值；除非线上证据表明确有调整需求，不增加过多用户配置面。

## 数据流

```text
任一 Controller 观察到 NodeInfo 变化
                |
                v
          统一变更分类
                |
      +---------+--------------------+
      |                              |
 managed TLS only                routes only
      |                              |
 现有专用热更新              RouteUpdateCoordinator
                                     |
                         刷新全部 NodeID 并确认向量稳定
                                     |
                         严格编译候选运行代次
                                     |
                   +-----------------+-----------------+
                   |                                   |
                 失败                                成功
                   |                                   |
       保持旧代次、保留 pending              注册/启动新 Outbound
             退避重试                                |
                                                       v
                                      原子切换 generation + DNS
                                                       |
                         +-----------------------------+------------------+
                         |                                                |
                 已有连接持有旧 lease                         新连接取得新 lease
                         |                                                |
                         `-------------------+----------------------------'
                                             |
                                  旧引用归零后回收资源
```

## 测试设计

实现必须按测试驱动顺序覆盖以下内容。

### 变更分类与协调

1. 仅 `Common.Routes` 变化返回 `routes_only`。
2. 端口、协议、证书或其他字段同时变化返回 `full_reload`。
3. 现有 managed TLS 域名专用分类不回归。
4. N 个 NodeID 因同一次编辑变化时只产生一次构建和一次发布。
5. 两次配置向量不一致时不发布，并以最新版重新稳定。
6. 构建期间到达新版本时废弃旧候选。
7. ETag 已更新但首次构建失败，后续 `304` 仍能重试 pending。
8. 任一节点拉取失败时保持旧代次且不写入 `ReloadCh`。

### 编译与原子性

9. 非法路由 JSON、重复 tag、缺失引用和 Outbound 启动失败都会拒绝整个候选。
10. 发布前每个 Router tag 都能解析到已启动 handler。
11. 并发 Acquire 与 Publish 时，连接只能取得完整旧代次或完整新代次。
12. 候选失败不会改变 active generation、DNS 后端或 Controller active NodeInfo。
13. forced、default、normal route 和嵌套 detour 都使用正确的代次 resolver。
14. DNS 切换期间查询使用完整旧后端或新后端，不会读取 nil 或已关闭对象。

### 连接连续性

15. 建立持续双向传输的 TCP 会话，反复发布路由时连接不关闭且数据持续可达。
16. 建立持续 UDP 会话，反复发布路由时 Link 不被主动关闭。
17. 在切换临界区持续并发建立新连接，连接建立失败数为零。
18. 已有连接继续使用旧 handler，新连接按新 Router 使用新 handler。
19. `V2Core.Close`、Inbound Close/Start 和进程级 `ReloadCh` 在纯路由测试中均未被调用。

### 生命周期与恢复

20. 长连接存在时旧 generation 和 handler 不会被回收。
21. Link 上下行结束或 context 取消后 lease 只释放一次。
22. 最后一个引用释放后 retired generation 和不再共享的 handler 会被关闭。
23. 相同 Outbound 配置跨多个代次只创建一个共享 handler。
24. 快照写入失败不回滚运行代次，后续重试成功。
25. 工作协程异常不会关闭当前 Core 或连接。
26. 重复大量切换后 goroutine、lease、retired generation 和资源池能够回落。

### 验证命令

实现阶段至少执行：

- 变更分类、协调器、编译器和 Dispatcher 的目标包测试；
- 相关包的 `go test -race`；
- 仓库完整 `GOEXPERIMENT=jsonv2 go test ./... -count=1`；
- `git diff --check`；
- 不启动生产服务或长时间运行的 watcher。

连接连续性集成测试应使用本地可控的 echo/假 Outbound，不依赖公网稳定性。UDP 的验收关注 v2node 不主动关闭 Link；公网或底层网络天然丢包不属于软件零断线证明。

## 上线策略

1. 新二进制部署本身仍需要一次受控进程重启，应安排低流量窗口。
2. 先在一台配置多个 NodeID 的机器启用默认热更新路径。
3. 部署前建立持续 TCP、UDP 和新连接探针，记录基线。
4. 连续执行覆盖 DNS、block、自定义 Outbound 和默认出口的路由编辑。
5. 确认连接失败数为零、Core 和监听器没有重启、一个批次只发布一个代次。
6. 观察 retired generation、CPU、内存、goroutine、心跳和错误指标。
7. 单机稳定后逐步扩大范围；只有出现无法通过保持旧代次规避的实现级故障时，才人工关闭热更新开关。

## 完成标准

以下条件全部满足才能认为本设计实现完成：

- 纯路由变化不再进入全局 `ReloadCh`；
- 同机全部 NodeID 的路由配置以一个稳定向量编译和发布；
- 新旧 Router、DNS 和 Outbound 生命周期不存在缺失窗口或并发数据竞争；
- 已有连接不被主动关闭，切换期间新连接失败数为零；
- 失败候选保留上一版并可在 `304` 后继续重试；
- 旧代次在引用归零后可回收，长连接存在时不会被提前销毁；
- 目标测试、race 测试、完整 Go 测试和差异检查全部通过；
- 上线探针确认无 Core/Inbound 重启、无连接中断、无节点心跳中断。
