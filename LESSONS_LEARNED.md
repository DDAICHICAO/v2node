# LESSONS LEARNED

## 2026-11-06：路由 DNS 合并必须区分省略值与显式关闭

- 症状/链路：复核后台路由回显时发现 DNS 表单提供 script 和关闭开关，但 core/custom.go 合并遗漏 Script，布尔值只合并 true、TTL 只合并非零；显式 false/0 单独配置在严格编译时甚至被当作无效配置。
- 修复：DNS 解析后同时检查原 JSON 字段是否存在；script 进入编译配置，全局 false/0/空字符串可显式覆盖，未提供字段继续继承。旧 applyDNSRouteConfig 与严格 applyDNSRouteConfigChecked 共用合并逻辑；多个全局设置按收到的规则顺序以最后一个显式值为准。
- 验证：在既有 core/custom_test.go 先复现严格/旧路径失败，再验证显式关闭、保留省略字段及脚本到编译配置；core/route_compile_test.go 补充普通/轮询标签前后向引用、内置标签和缺失定义。核心、路由运行时、dispatcher、节点启动/重载定向检查通过。未新增永久测试文件，未构建、发布、部署或真实客户端验收。
- 下次入口：core/custom.go 的 dnsConfigHasValue / mergeDNSConfig，core/route_compile.go 的 applyDNSRouteConfigChecked，以及相应既有测试。后台完整定义与标签引用不能互相替代；DNS script 与域名/回退/并行选项冲突由表单和当前核心校验。详见 docs/log-troubleshooting.md 同日复核章节及相邻 v2board 的 docs/server-routing-forms.md。

## 2026-11-06：SOCKS5 普通用户监听不能直接复用固定账号入口

- 症状/需求：A→B 解锁想减少额外代理加密；用户要求 SOCKS5 同时可作为普通用户节点，不能只做固定账号出口或开放匿名探测端口。
- 根因/链路：当前 Xray SOCKS 服务端没有动态 UserManager，SS none/plain 已移除。新增受管理 SOCKS 监听完成动态认证，再复用核心 CONNECT/UDP 与原 dispatcher，将账号映射到 UserTag，保持限速、设备限制、流量/在线归属和撤销链。
- 改动：core/socks.go 管理凭据、最多 256 个并发认证和 10 秒认证期限、已认证连接及监听生命周期；配置/用户增删入口接入 socks。补路由裸标签引用的两遍解析，保留逻辑→物理标签及轮询组解析，缺失标签按既有严格/兼容启动策略处理。
- 验证/边界：本地 TCP/UDP echo、错误密码/匿名拒绝、UID 计费、撤销既有 TCP/UDP 控制连接、重新授权、监听重建通过；面板生成 10 组出站、标签前向/缺失/轮询组及相邻既有 Go 定向回归通过。临时测试检查后删除，未发布、部署、压测或客户实机验收。
- 下次：api/v2board/node.go / panel.go → core/node_preflight.go / node.go / user.go / socks.go → core/app/dispatcher；路由查 core/route_compile.go。UDP 动态端口和 NAT/防火墙要单独排查，SOCKS5 不加密。详细记录见 docs/log-troubleshooting.md 同日章节，面板操作见相邻 v2board 的 docs/server-routing-forms.md。

## 2026-11-06：面板 CF CIDR 不能直接覆盖进程级官方列表

- 需求/风险：后台需要查看、增删改并下发可信 CF 回源 CIDR；单进程可能服务多个面板或节点，直接写全局官方 store 会造成策略串用，官方定时刷新还会覆盖人工修改。
- 修复链：能力 `cloudflare_origin_cidrs_v1` → `BaseConfig.CloudflareOrigin` → `core/inbound.go` → 私有 WS 传输元数据 → 每入站独立 store。托管清单是该入站的完整权威范围，官方任务只更新未托管入站引用的全局 store；缺省/关闭回退自动模式，PROXY/finalmask 保持原路径，元数据不发往网络。
- 验证/维护：沿用现有 Go 测试文件检查能力、配置解析、来源解析与隔离、默认回退、坏 CIDR/元数据、重载分类；面板 31 项 PHP 隔离检查与 TypeScript 检查通过。两端默认 22 条范围一致，空/宽泛/保留段拒绝；未创建永久测试文件、发布或部署。详见 `docs/log-troubleshooting.md` 同日“接收面板托管”章节。
- 后续入口：`api/v2board/node.go` / `panel.go`、`core/cloudflare_ips.go` / `cloudflare_websocket.go` / `inbound.go` 与相邻面板 `CloudflareOriginPolicy`。该功能使用既有配置重载和离线快照，不把配置已保存等同于所有旧版节点已应用，真实上线需按能力、版本与新连接来源分别验收。

## 2026-11-06：CF 来源识别需要官方回源网段与严格头解析（本地修复）

- 症状/根因：CF WebSocket 节点来源退化为 CF；固定 Xray 升级后默认忽略 XFF。`trustedXForwardedFor` 是头名列表，并不验证来源 CIDR，简单恢复无条件 XFF 会允许直连伪造。
- 链路/修复：`core/inbound.go` → SNTP WS listener → `RemoteAddr` → Xray inbound source → dispatcher/limiter → `node/user.go`。普通 WS 入站自动匹配 CF 官方 socket 来源，仅采用单值合法公网 CF-Connecting-IP，兼容 IPv6、mapped 和 Pseudo IPv4；其他来源或无效头保留实际 socket，不信任任意 XFF。PROXY/finalmask 保留原路径，其他 CDN/本地反代需独立核实。
- 清单：内置官方 IPv4/IPv6，进程级独立任务启动异步更新并每 6 小时刷新；固定 HTTPS、10 秒超时、拒绝重定向、64 KiB 与双族/公网 CIDR 校验、原子快照。失败保留当前清单，重启用内置值。客户端优选 IP 不等于 CF 回源对端，不能拿第三方优选 TXT 当信任依据。
- 验证/范围：旧生成器回归先失败，修复后官方网段/伪造/IPv6/清单失败、TLS WS early data/收发、监听关闭保留已有流，以及真实本地 VLESS 认证→用户/设备在线来源通过。Xray freedom 默认 blackhole 回环目标；本地在线统计用例验证到 limiter，不修改该保护，也不据此宣称生产代理转发通过。没有新建永久测试文件、构建发行包或修改面板/数据库。
- 下次/状态：详见 `docs/log-troubleshooting.md` 同日章节；新增 `core/cloudflare_ips.go`、`core/cloudflare_websocket.go`，生命周期入口 `cmd/server.go`，回归在现有 `core/custom_test.go`。本次提交纳入源码与相关记录，未推送、部署或重启；需更新节点后用新连接验收真实客户端与面板自然上报，旧历史不自动恢复。

## 2026-11-05 晚间：复制同 NodeID 原证书解决持续活跃实例冲突

- 症状/根因：230/231/761/762 两实例持续上报不同自签指纹，面板冻结全局 pin。补文件或重新生成只恢复监听，不自动恢复旧订阅信任；self/file 不承担 managed 的跨机证书分发。
- 修复：核对原机器文件/实际监听/面板既有 pin 后，备份新增机器配置与旧证书，以 SSH/SFTP 复制对应 NodeID 原证书及私钥，文件 0600；临时 Timeout 变化重建目标 Controller，随后逐字节恢复原配置。未改原机器、删节点/作用域、直接 SQL、清观察缓存或重启主进程。
- 验证/边界：21:59 四组自然回报 consistent、两实例/一个指纹，已有 pin 值保留；两机八个监听、PID/NRestarts 不变，目标本机/外部 TLS 1.3 实际证书匹配。761/762 原自签信任下严格域名验证通过，到期 2027-11-03 17:13:45（北京时间）；230/231 原 SAN/校验名不一致仍存在，未修改校验策略或验收客户转发。独立托管作用域 2 保持 v2、2/2、pending 0。
- 管理标签验收：22:05:38 三台目标机器涉及的 14 个 NodeID 在后台实际配置汇总中全部 applied，failed/blocked/unavailable 为 0。追踪 `ConfigStatusReporter` → config-status → `V2nodeConfigStatusService`，不要把 TLS 同步成功等同于配置失败状态已清除。
- 下次入口：`docs/log-troubleshooting.md` 同日“同 NodeID 原证书跨机对齐”章节；`requestCert`、Controller 复用/重建与面板 `V2nodeTlsCertificateReconciler`。分别比较文件、进程实际证书、面板自然共识和客户连接，避免用端口开放替代完整验收。

## 2026-11-05 晚间：托管凭据超时后的重试门禁形成永久等待

- 症状/根因：已签发作用域长期 1/2，新增实例从约 20:30 起 `managed_tls_config_invalid`、version 0、token 未配置且无监听。pending Controller 只启动证书管理器与状态报告，配置轮询未启动；管理器又要求预先存在 token，使 API 自动领取重试不可达。
- 修复：`node/managed_tls_manager.go::validateConfiguration` 仅保留静态结构校验，让正常取证接口处理凭据自动领取与 HMAC。现有 `managed_tls_runtime_activation_test.go` 增加首次领取失败后重试、有效本地证书在凭据失败时继续服务；修改前两项失败，修改后 `go test ./node -run '^TestManagedTLS' -count=1` 通过。修复和测试纳入本次提交，尚未构建、发版或部署，不能把线上凭据补齐当作代码已上线。
- 线上恢复/验证：真实接口领取并校验凭据，标准目录 0700、文件 0600；备份后仅临时调整目标 NodeID 的 Timeout 触发 Controller 重建，再恢复原配置值。约 21:34 监听恢复，21:38 面板自然回报 2/2、pending 0；本机和外部严格公共 CA / SNI 的 PROXY+TLS 1.3 通过，PID/NRestarts 不变。无新签证书、直接 SQL、清缓存或人工 ready；客户代理认证/转发未验收。
- 下次入口：`docs/log-troubleshooting.md` 同日托管凭据章节；`Controller.Start`、`managedTLSManager.Prepare/reconcileOnceLocked`、`Client.managedTLSRequest`、`EnsureManagedTLSCredential`。首次网络超时上游原因仍未知；curl 空 POST 需显式发送零长度 body，业务失败也可能是 HTTP 200。

## 2026-11-05 晚间：file 证书丢失阻塞整组冷启动

- 症状/链路：两台 Debian 13.3 / v5.0.0.92 各有四个 NodeID；file 模式默认 `trojan<NodeID>.cer/.key` 缺失，`Node.Validate` 失败导致全部业务监听缺失，systemd 仍 active。按退避重复失败，不是需要删除共享证书作用域。
- 恢复：用户选择保留全部节点、补齐证书；延续原先自签策略，为目标 NodeID 各自生成 SAN 匹配现有 SNI/校验域名的一年期证书，私钥 0600、证书 0644，先验证域名及公私钥匹配，不覆盖旧文件。一个按定时退避恢复，另一个备份后仅修改配置尾部空白，改变摘要触发恢复，不重启服务。
- 验证：两台共八个端口本机及外部 TCP 成功；各 Trojan 在明确加载本地证书信任后 PROXY+TLS 1.3/SNI 校验通过，进程与重启计数未变。证书重建改变 pin，需核对面板运行态共识与客户端订阅刷新；未据此宣称真实用户转发验收。证书于 2027-11-05 到期，file 模式需要备份和后续更新。
- 下次入口：`docs/log-troubleshooting.md` 同日晚间章节；`node/cert.go`、`node/reconcile.go`、`node/cleanup_control.go::ConfigDigest`、`cmd/reload_retry.go`；面板 `V2nodeTlsCertificateReconciler`。相同字节重写会触发 watcher，但不会跳过相同摘要的退避。独立托管节点首次领取凭据超时约 16 秒与本次 file 缺失分开处理。
- 未闭环：21:09 相关四个 Trojan 的面板 pin 未与本机一致；真实身份补报 `/api/v2/server/status` 返回 `data=true`，唯一查询参数配置检查仍旧。需用户提供原来可用的同 NodeID 机器或面板访问以检查证书共识；不能把接口成功当作 pin 更新成功，刷新旧订阅目前不能保证恢复。不执行 SQL、清冲突观察或关闭校验。

## 2026-11-03：file 证书预检、失败配置重试与独立状态上报（本地源码）

- 症状/链路：新增 TLS 节点缺少 file 模式证书时，候选热加载失败、旧节点继续运行，但修复证书后没有可靠的定时恢复；systemd active 也不能说明新增监听已启动。检查 `cmd/server.go` → `node.New/Validate/Reconcile` → `node/cert.go` → 核心监听，并贯通面板 `/api/v2/server/config-status` → 独立缓存 → 节点管理标签。
- 根因：原 file 分支只检查路径非空，实际文件问题由核心报错；旧定时启动重试只覆盖 `nodes == nil`，热加载失败没有统一待恢复状态。失败在 Controller 启动前发生时，普通运行态上报也不可用。
- 修复：file 模式预检文件存在性、普通文件、PEM 与证书/私钥匹配，保留固定错误码与 NodeID 归属；不自动生成或覆盖人工证书。冷启动在 core.Start 后、业务 Controller 启动前校验，Titan 预检需要核心已初始化。启动与热加载共享 30/60/120/240/300 秒退避，后续封顶 300 秒；相同本地配置的重复通知不能绕过退避，文件摘要改变可立即尝试，成功清空状态。管理清理后只重读最新文件，不复用旧 Nodes 列表。
- 面板状态：独立异步报告器在监听失败时仍可上报，区分 applied/failed/blocked、是否保留旧运行态和下次重试时间。面板按实例加锁合并、过滤过期状态并使用固定中文错误；管理页显示启动失败、配置未生效或同机配置受阻。健康且已应用配置的实例不计入“保留原运行态”。此接口不覆盖带宽、TLS、访问审计或计费缓存，不上传原始错误与本地路径。
- 验证：`go test ./cmd ./node ./api/v2board ./conf -count=1 -timeout 120s` 通过；临时回归覆盖缺文件、坏 PEM、密钥不匹配、目录、有效文件及 self 原行为、退避和报告错误归属。真实本地 VLESS 代理连接在新增 Trojan 缺证书失败期间继续收发；只补齐证书、不改变配置字节即可重新加载成功，旧核心及连接保留，新 Trojan TLS 握手通过。面板隔离缓存的多实例/恢复/过期/字段清洗与 UI 回归、PHP 语法及定向 TypeScript 检查通过。
- 测试陷阱：任何会触发节点版本探测的临时测试程序，都要在 TestMain 中先拦截首个 `version` 参数并退出；否则当前测试可被递归启动。相关旧记录见本文件 2026-07-30 条目。本轮临时测试文件在验证后移出仓库。
- 下次检查/范围：先对照固定证书错误码、`retry_at`、恢复成功日志和实际监听，再查面板 `config_status` 与普通运行态；细节见 `docs/log-troubleshooting.md`。节点重试需要更新 v2node，管理标签需要面板后端和前端一并发布；旧面板返回 404 仅使新状态报告暂不可用。没有改数据库、证书信任策略或 file 自动续签行为；交付范围为本地源码提交，未构建发行包、推送或部署，本地回归不代表 Linux 实机及客户链路验收。

## 2026-10-24：审计队列空闲页清单写放大拖慢授权快照

- 症状/链路：少量在线用户仍有高 I/O 和同步确认长尾；访问/流量审计入队及 Ack → bbolt 每次事务保存 freelist → 同盘授权快照 Sync 等待 → 同步 ACK。授权行数不等于在线人数，文件体积也不等于当前积压。
- 根因：旧节点 access.db 约 2.5 GiB、超过 99% 空间空闲，每个小事务仍重写约 5 MiB freelist；部分 flow.db 另有约 1.52 MiB。旧 Peek 只读优化已经部署，不能重复作为修复方案。
- 修复：共享 `openBoltSpool` 启用 `NoFreelistSync`，正常事件/事务元页刷盘保留；重新打开时重建空闲页信息。原库直接兼容，不清空、不自动压缩、不提前 ACK。物理文件大小不自动回收，重开扫描成本需要上线观察。
- 验证：旧实现的 32 MiB 历史积压夹具先失败，入队/Ack/Reject 事务页面量 221184 字节，修复后 12288 字节（约减少 94.4%，不含固定元页，不等于生产磁盘写入降幅）；两类队列异常进程退出后恢复、旧默认打开/再次升级、FIFO/计数/序号和结构检查通过，`go test ./common/accessaudit -count=1` 通过。
- 范围/下次：只提交源码，由用户构建部署；未验证断电、Linux 运行或生产延迟改善。优先对照进程 write_bytes、审计新增错误、同 revision 的快照/ACK 和真实连接；仍慢再查宿主存储与快照状态锁。维护入口 `docs/audit-spool-storage.md`，关联 `common/accessaudit/spool_core.go`、现有 `spool_core_test.go`、`node/offline_state.go`；相邻 v2board 排查手册保存脱敏现场证据。

## 2026-10-16：默认关闭的秒级许可与可靠回退

- 症状/链路：节点用户同步已有 wakeup，但配置发现与完整状态仍随轮询；用户要求新增秒级通讯可回退。链路为后台提交后修订 → 既有鉴权 WSS → `node_realtime_v1` 许可 → 原配置接口及独立网卡采样。
- 实现：`api/v2board/realtime.go`、`wakeup.go` 协议能力和共享写入超时；`node/realtime.go` 每连接独立许可，15 秒失效、单飞配置拉取、合并/抖动/退避、独立采样器。无许可/旧后台不发新遥测，原配置轮询和 60 秒计费持续。
- 一致性：读协程及时回复 ping 并在合并信号前记录最高 dirty 修订号；不能因同步 HTTP 较慢堵住心跳，也不能先 ACK 再落盘。关闭连接取消并回收新增任务；配置提示与周期监控共用锁，等待锁可取消，提示不触发更新/解锁探测任务。
- 验证：Go 专项覆盖默认关闭/许可过期/停用、配置单飞/退避/取消、慢同步心跳及原用户/配置行为；本地实际 WebSocket 上 ACK/pong/遥测各 30 条完整接收。Windows 无 CGO，race 尚未执行成功；未构建 Linux 二进制、未部署、未验收真实网卡和生产秒级时延。
- 回退/下次：使用相邻 v2board `docs/node-realtime-rollout.md` 与 `scripts/node-realtime-release.py`。关开关令许可到期，版本回退必须现场保存旧二进制 SHA-256/权限/版本并验证服务路径，不恢复过时离线用户授权快照。原配置重载语义没有改变，秒级通知不等于无中断。
- 交付约束：用户要求本轮仅提交源码，v2node 构建发版和线上更新由用户自行操作；不能把 Git 提交/推送报告为已发布或已部署。

## 2026-10-08：self Trojan 只换 SNI 不重建整个代理核心（源码完成）

- 症状/根因：后台批量替换 SNI 使 self Trojan 进入 `nodeInfoFullReload`，整进程所有节点和核心关闭重建；已有免重载分支仅覆盖 managed TLS。self 证书文件复用，允许任意 SNI 的普通 Trojan TLS 不使用域名字段构造入站，纯名称变化无需重建。
- 改动/链路：`node/node_info_change.go` 新增 `nodeInfoSelfTrojanSNIOnly`；只在当前和新配置均为 Trojan/普通 TLS/self、无托管作用域、证书/私钥路径非空、允许陌生 SNI时，忽略 Common.ServerName、TlsSettings.ServerName/ServerNames 与 CertInfo.CertDomain 的差异，严格比较其余所有字段。
- 新行为：`node/task.go::applyPendingNodeInfo` 在同一 stateMu 写锁内先落离线快照，再更新 info 与 active version；成功清 pending，不向 ReloadCh 发信号，日志 `Self Trojan SNI applied without global reload`。保存失败保留旧状态和 pending；重复观察同一配置不重载，随后真正运行字段变更仍可重载。不改用户状态、证书文件、订阅验签或 managed SAN 验证。
- 验证：在已有 `node/node_info_change_test.go` 先复现错误重载，再通过域名/空值/IP、失败重试、快照及版本一致、重复观察、后续端口重载和 19 种保护边界。`GOEXPERIMENT=jsonv2 go test ./node ./core -run '^Test(SelfTrojanSNIChange|ClassifyNodeInfoChange|ApplyPendingNodeInfo|NodeInfoMonitorPersists|ManagedTLSDomainOnlyChange|CommitRouteNodeInfo|RouteHotReloadDisabled|RouteHotReloadKeepsExistingAndNewConnectionsOnline|SwapPreparedInbound)' -count=1` 通过；含现有 managed/route/inbound 定向回归。Go 格式、差异检查通过，Windows CGO=0 未做 race；无新增测试文件。
- 边界/下次查：仅优化 self Trojan，file/dns/http/managed/REALITY 或拒绝未知 SNI不进入新分支；路由、端口、协议、证书路径或规则同时变化仍走原流程。先查分类条件、pending/version、离线快照写入错误和免重载日志，再看全局 reload 日志。节点不解码客户端 allow_insecure，本次不依赖该客户端开关判断服务器行为。
- 文件/部署：修改上述三个 Go 文件及本记录，后台手册在 v2board `docs/uniproxy-node-troubleshooting-runbook.md` 同日补充；保留其他未提交修改，未提交、构建发行包、部署、改证书或重启线上服务。必须更新节点二进制后才生效，线上批量替换及原故障网络未验收；不持久化完整 IP、用户凭据或证书指纹。

## 2026-10-08：Trojan self 证书与当前 SNI 不同，仍可握手不代表客户端验证了证书

- 症状/链路：用户发现任意 SNI 仍可连接；面板 `v2_server_v2node.tls_settings` → 订阅 `skip-cert-verify` → `api/v2board/node.go::CertInfo` → `core/inbound.go` → 实际 TLS 握手。
- 现场：HK Trojan #114/#208/#352 的 `cert_mode=self`、`reject_unknown_sni=0`，快照 `RejectUnknownSni=false`；面板 `allow_insecure=1`，部署机订阅生成函数输出 `skip-cert-verify: true`。配置 SNI 为 `vercel.com`，实际证书 CN/SAN 仍是 `live.bilibili.com`；`node/cert.go::requestCert()` 在 self 模式复用已存在的证书/私钥，修改 SNI 不会自动重签。
- 验证：入站 10011/12001/16001 均要求 PROXY Protocol；先发送基于本地 socket 的 PROXY v1 头，再以配置 SNI、`sni-check.invalid`、无 SNI测试 TLS，9/9 成功并返回对应磁盘证书。系统默认信任库严格验签 3/3 失败，错误 18 `self-signed certificate`。未测试用户认证或实际代理流量。
- 处理/边界：此次只读核实，没有换证、改配置、重启或提交。若后续恢复正常验签，先处理可信链及验证名称匹配；不能将 TLS 成功误读成证书已受信任、Trojan 无需密码或线路不会阻断。
- 同日批量替换 SNI 诊断：优化前分类器只为 managed 域名变化免重载，self 的 server_name/CertDomain 变化仍走全局 reload；成功时进程不退出，日志“重启成功”指进程内重载。诊断阶段未改代码或触发线上替换，未确认线上二进制与源码一致；随后经用户授权完成 self Trojan 纯 SNI 免重载源码优化，边界和回归见上一条，尚未部署。用 PID/启动时间与相关日志区分两类重启。
- 下次查：`/etc/v2node/offline-state/node-*.json` 的 TLS 指定字段、实际 `.cer`、`api/v2board/node.go`、`core/inbound.go`、`node/cert.go`，以及后台实际订阅配置。完整方法见 v2board `docs/uniproxy-node-troubleshooting-runbook.md` 同日章节；不输出含用户凭据的整份快照，不保存完整 IP、口令或证书指纹。原故障网络的间歇断连及公网中转尚未复测。

## 2026-10-07：账户到期必须撤销同用户的全部设备凭据

- 症状：网站套餐过期数小时后，后台仍有设备心跳；用户未确认重新连接仍可用，目标节点运行快照尚未取得。
- 影响链：v2board `ServerService::getAvailableUsers/expandUsersWithDeviceUuids` → UniProxy 全量响应/缓存 → `api/v2board.UserInfo.ExpiredAt` → `node/user_expiry.go` / `node/user_delta.go::removeExpiredUsers` → `Controller.applyUserList` → `V2Core.DelUsers` → `LinkRegistry.DeactivateUser`。
- 根因：后端全量设备展开 helper 漏掉 `expired_at`，节点解码为 `0`；旧过滤仅按单条 UUID 的时间移除，带时间的主 UUID 到期后，无时间的设备仍留下。增量 upsert 会保留设备到期时间，空增量不会补旧字段。0 倍率只是面板不入账计费流量，与时间授权无关。
- 修改：面板已改为克隆用户并替换设备 UUID，完整继承到期字段。节点先收集已到期的正整数 UID，再撤销该 UID 全部记录，复用已有定时器和连接回收链；保留真正不限时用户及不同 UID 隔离，不把无效 UID 合并删除。续费 upsert 整体替换所有凭据后再判到期，避免沿用旧时间。
- 验证：临时混合凭据回归先失败后通过；PHP 8 个场景全部通过，真实 JSON/msgpack 响应到节点解码/到期过滤 16 个组合通过。`GOEXPERIMENT=jsonv2 go test ./node ./core ./core/app/dispatcher -run 'Test(ExpiryRegressionTemp|RemoveExpiredUsers|NextUserExpiry|LocalExpiry|ApplyUserList|DeactivateUsers|LinkRegistry|CommitUserState|SyncUserState)' -count=1` 通过；Go 格式、两仓库 diff 和后端 PHP 语法检查通过。`ExpiryRegressionTemp` 为本次临时测试，结束前删除，不保留新增测试文件或合成响应。
- 后续：部署后端并刷新常驻 Worker，更新实际 v2node 二进制；定向刷新相关每节点/共享缓存，并让节点获取完整授权状态。若所有凭据都缺时间或主 UUID 已被旧逻辑删除，节点兜底无法恢复未知时间，必须同步，不能把所有 0 时间用户当过期。现场只汇总凭据数量/到期时间并检查实际版本、时钟、到期删除及数据面停用日志。
- 范围/入口：本次修复按后续授权单独提交；未修改数据库、计费倍率或离线保活政策，未新增周期全量任务，未打包或部署。完整部署边界见 v2board `docs/uniproxy-node-troubleshooting-runbook.md` 同日章节；未保存真实用户身份、UUID、IP、凭据或私密快照。

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

## 2026-08-19：托管 TLS Cloudflare TXT 已创建但本地传播预检超时

- 症状：作用域停在 v0 并显示 `managed_tls_obtain_failed`，节点日志先显示 Cloudflare `new record`，随后每 12 秒等待传播，两分钟后清理 challenge。
- 影响链：v2board 作用域租约 -> v2node managed TLS issuer -> Cloudflare DNS API -> lego 本地传播预检 -> Let’s Encrypt DNS-01。
- 根因：TXT 写入成功，但节点通过 `/etc/resolv.conf` 的 systemd-resolved stub 执行本地传播预检时无法完成，导致尚未进入 ACME 最终验证就超时。
- 修复：Cloudflare managed TLS 使用公共递归 DNS，TXT 写入后固定等待 15 秒并跳过节点本地传播预检；最终权限校验仍由 Let’s Encrypt 完成；Obtain 原始错误写入节点日志。
- 验证：运行 managed TLS 定向单元测试；部署后重新签发，确认不再等待本地传播两分钟并生成 v1 证书。
- 下次先查：先区分 Cloudflare TXT 创建失败、本地传播预检失败和 Let’s Encrypt 最终验证失败；不要只根据面板通用错误码判断 Token 无效。
- 相关文件：`node/managed_tls_issuer.go`、`node/managed_tls_issuer_test.go`。

## 2026-08-20：同机多 NodeID 的纯路由更新必须作为一个不可变运行代次发布

### 症状与影响链

- 管理员保存一条被多个节点引用的路由规则后，同一 v2node 进程内的相关 NodeID 会先后拉到新的 `/api/v2/server/config`；旧 `nodeInfoMonitor` 把每次变化都写入进程级 `ReloadCh`。
- `cmd/server.go` 收到任意一次 reload 后会关闭全部 Controller 和整个 Xray Core，再重新拉取、编译和启动。于是 N 个 NodeID 既可能重复触发昂贵重载，也会让既有 TCP/UDP 连接和监听器真实中断。
- 完整链路为：v2board 路由规则保存 -> 各 NodeID 配置正文和 ETag 变化 -> v2node retained pending -> 整机稳定向量协调 -> 严格编译候选 Router/DNS/Outbound -> 原子发布 generation -> 旧连接释放 lease 后回收旧资源。

### 根因、修复与失败边界

- 路由、DNS 和自定义 Outbound 原来只存在于 Core 启动配置中，没有独立的运行代次和引用生命周期；仅做 debounce 仍然必须重启 Core，无法满足零断线。
- 新实现先严格区分 unchanged、managed TLS only、routes only 和 full reload。纯路由变化由单 worker 在 500ms 稳定窗口内并发刷新该机器全部 NodeID，两次配置版本向量一致且发布前 epoch 未变化时才整体应用一次。
- 每个新连接只取得一个不可变 generation lease；normal、forced、default、DNS 和嵌套 detour 都使用同代 Router 与 Outbound。已建立连接继续持有旧代次，最后一个 lease 释放后才按 Remove -> Close 顺序回收不再共享的资源。
- 候选编译、Outbound 启动、二次稳定、面板刷新或 Apply 任一步失败时保持旧代次，保留 observed pending，并以可被新 Notify 打断的指数退避重试；不能静默改走全局 reload。
- 协调器的最大退避必须在抖动之后再次封顶；同一 pending 版本只 Notify 一次，避免短轮询不断重置 500ms 稳定窗口；目标刷新、状态读取、Apply、Controller commit 和快照保存等外部边界要把 panic 转为可重试失败，不能让后台热更新异常退出整个进程。
- Apply 成功后必须先原子保存整机 `route-runtime.json`，成功后才能分别更新 Controller active 状态和单节点快照；否则首次发布中途退出会留下半旧半新的恢复向量。整机快照失败只重试持久化和状态提交，不重复发布 generation；离线启动只有在整机 NodeID/APIHost 身份完全匹配且严格编译通过时才覆盖旧 NodeInfo，用户、在线状态和同步序列仍来自单节点快照。
- 防御性 NodeInfo 深拷贝必须保留 `NetworkSettings` 的 nil/raw 表示；把 nil 转为 JSON `null` 会让本来只变 Routes 的配置被误判为 full reload。
- `Controller.info` 在热更新后不再是启动期常量；流量上报、证书状态、在线状态间隔和其他运行任务必须通过 `stateMu` 读锁取得不可变指针快照，否则 Linux race 检查会暴露发布写入与旧读取路径的竞争。

### 验证、上线与下次检查

- 单元和压力回归覆盖：严格编译冲突/循环/依赖传播、并发 Acquire/Publish、旧连接 lease 延迟回收、共享 Outbound 引用、候选失败不推进、N 节点只发布一次、向量变化重新稳定、304 后 retained pending 重试、Notify 打断退避、快照失败不重复发布，以及纯路由 pending 不写 `ReloadCh`。
- Windows 本机完整 Go 回归使用 `GOEXPERIMENT=jsonv2`；`go test -race` 需要 CGO，当前环境没有 `gcc`，上线前必须在带 CGO 工具链的 Linux CI 或构建机补跑，并用持续 TCP/UDP 和并发新连接探针验证。
- 生产先灰度一台配置多个 NodeID 的机器。检查日志只出现一次 route generation 发布，不出现 `收到重启信号`、Controller task stopped、端口消失或 Xray Core restarted；同时观察 active/retired generation、lease、Outbound pool、CPU、内存和 goroutine 是否回落。
- 若变化还进入全局 reload，先查 `node/task.go` 的统一分类和协调器开关；若候选一直失败，查配置向量、严格编译错误和 retained pending，不要先删除快照或重启；若旧资源不回收，查 dispatcher Link 的双向结束/context 取消和 lease release-once。
- 相关文件：`api/v2board/node.go`、`node/node_info_change.go`、`node/route_update_coordinator.go`、`node/route_runtime_state.go`、`node/task.go`、`core/route_compile.go`、`core/route_runtime.go`、`core/route_outbound_pool.go`、`core/app/dispatcher/route_runtime.go`、`core/app/dispatcher/route_link.go`。
- 设计与实施入口：`docs/superpowers/specs/2026-08-20-v2node-zero-disconnect-route-hot-reload-design.md`、`docs/superpowers/plans/2026-08-20-v2node-zero-disconnect-route-hot-reload.md`。实现提交依次为 `9b52acb`、`231c1d7`、`774db97`、`a75370e`、`282879b`、`cd18a4b`、`b8f4796`、`1afb5d2` 和 `903b9f3`。

## 2026-10-14 Titan V3 WSS 与原会话恢复

- 目标/链路：api/v2board config/capability → core/sntp_eclipse_v3.go TLS/WSS → common/sntpv3 稳定 Conn → 原 Titan V2 cipher / Dispatcher / limiter / 明文计费。
- 修复：独立 resume key、challenge/epoch/进度 proof、原密文重传去重；Ready 丢失可持钥恢复，目标启动 sync.Once；新连接及恢复查授权，恢复 proof 验证后再按当前物理来源执行 limiter 策略，原连接仍在正式 LinkRegistry 内。证书原子替换，身份或其他配置变化拒绝热替换。
- 资源/易错点：共享发送/接收各 64 MiB 有效 payload，而非每会话预留；默认 256 会话、64 待认证、物理 max_sessions+64。正常末尾响应直到读完仍计入额度；Abort/Close 释放。TLS 的 lazy PROXY 解析不能清除 net/http 设置的握手 deadline；Serve 退出必须清 ready 并关闭逻辑会话。
- 验证：共享恢复包十轮 race；真实客户端/节点的 TLS/WSS、双 IP 2.76 MB 回传、生产 Dispatcher 目标只一次且计费无重复、换新证书和新旧连接、错 SNI、Ready 丢失及正式 V2Core.DelUsers 立即关闭逻辑流验证通过。core/api/node 常规回归与 Linux 二进制编译通过。旧 bbolt 1.3.2 在 Go1.26.2 race 的 checkptr 中崩溃；仅该依赖关闭 checkptr 后继续 node 并发检查，未为本任务升级它。
- 后续/边界：原进程是活 socket 所有者；同密钥的独立进程不能恢复原 socket，重启也不能。地区实网、物理路由归属仍需发版灰度。临时测试已备份并在结束前清理，见 v2board docs/titan-v3-retirement.md 的命令与发布顺序。
- 并发补查：窗口过期快照与新 wire 接入并发时，必须确认 finishIf 真正关闭后再退出收发循环；确定性调度测试先失败、修复后通过，双端源码同步。测试调度 hook 仅临时验证，未进入提交。

## 2026-10-14：自研 V1/V2 托管证书后台预备

- 症状：证书申请流程与节点启动耦合，无法在保留现有 V1/V2 TCP 服务的同时可靠预备 V3 证书。
- 影响链路：后台独立作用域 → 节点配置/能力上报 → `Controller.Start` → `managedTLSManager` → 证书文件与同步状态。
- 修复：上报 `managed_tls_native_prepare_v1`；V1/V2 托管模式即使 tls=0 也启动证书管理器，先启动原运行时，再后台签发/续期。证书变更只更新证书版本状态，不替换原生监听器；V3 的 TLS 证书阻塞门禁不变。
- 验证：`GOEXPERIMENT=jsonv2 go test ./node ./api/v2board -run 'TestNativeCertBatch|TestManagedTLS' -count=1` 通过；临时用例覆盖 V1/V2/tls=0/tls=1、非托管及 V3 隔离、轮换不替换监听器、签发失败保持既有运行时；临时测试文件完成后删除。
- 下次检查：能力是否由所有活跃实例上报、证书版本是否大于零、作用域同步覆盖率是否全覆盖；创建作用域不等于签发成功。
- 发布约束：先更新节点二进制，再更新后台批量入口；客户端无需更新。线上整批签发由用户发版后再执行。关联：`node/controller.go`、`api/v2board/panel.go`、v2board `docs/titan-v3-retirement.md`。

## 2026-10-14 - 旧 Titan 传输方案退役与下一代命名

- 用户纠正：旧 V3 已全面下线，不再修补或保留为运行选项；后续以 V2 衍生的新方案仍命名 V3。本文更早的 TLS/WSS/恢复实现条目仅是已废弃历史，不可据此重新启用。
- 清理链路：后台模式/批量编辑/下发 → v2node 模式解析、监听与运行上报 → 三端共用核心与 IPC 诊断。删除旧 TLS/WSS 与恢复源码、专用参数、能力声明、就绪判断和过期实施/发布指南；未知版本显式拒绝或过滤，避免误降级。
- 保留范围：V1/V2 原生协议、并发帧读写锁与半关闭、网络变化后的入口状态重置、TTL0 缓存清理、通用能力查询、独立证书运维功能；保留其他未提交修改，不改生产或数据库。
- 新设计边界：以 V2 为基线讨论握手/长度保护、抗探测行为和有界连接池；无抗封锁保证。产品名称可复用 V3，但新能力标识与线协议契约不能沿用旧值。新协议尚未实现。
- 验证与下一步：本次轻量检查的完整结果见 v2board docs/titan-v3-retirement.md；新设计见 docs/superpowers/specs/2026-10-14-titan-v3-native-design.md。源码清理不等于包内二进制已替换，需按实际发布流程重打三端和节点产物；不以编译/单测替代真实网络验收。


## 2026-10-14 - Titan native-1 双协议入口

- 目标/链路：后台 V3 配置 → core/sntp_eclipse_native.go 同端口 V2/V3 → 原授权/limiter/Dispatcher。旧 V2 首包、密钥和缓存配置保留。
- 实现：common/titannative/wire.go 与客户端 transport 同文件；长度/内容 AEAD 分离，50 字节无 read-ahead 分流，授权后防重放，待认证/活动/用户与进程预算分离，失败静默关闭。runtime 来自真实监听，含模式/公钥哈希/端口及聚合计数。
- 修正：混合入口先授权再写重放缓存；20 分钟覆盖允许未来 10 分钟 hello，缓存满不逐请求全量扫描；认证完成释放握手配额。PROXY 包装保留 CloseWrite。新入口无逐会话身份/目标诊断日志，Dispatcher 运维策略保留。
- 验证：两端 V1/V2/V3 TCP/UDP 回环及 1024 条混合持有 TCP、2 UDP/2 拒绝，预算回零；原生记录异常/节点预算/无效探测测试通过。Go 相关包检查；race 因本机无 CGO/C 编译器未完成。
- 边界/下一处：后台 docs/titan-v3-native-wire.md 与 docs/titan-v3-native-release.md；未部署、未实际出站/计费/运营商验收，不承诺抗封锁或进程重启续接。临时测试移出仓库。

## 2026-10-14：自研协议入口地址丢失与 PROXY 目的地址传播

- 症状：线上自研访问记录的 entry_ip 为空，在线心跳也缺少入口信息；不能以字段和接口存在代替实际采集验证。
- 影响链/根因：V1/V2/native-1 共用 sntpEclipseSession.dispatchContext，但 session.Inbound 漏设 Local；PROXY v1/v2 解析只保留来源 IP。dispatcher 审计、FlowTraffic 和 limiter 在线入口均依赖 Local。
- 修复：从真实 TCP 连接设置 Local；仅在 acceptProxyProtocol 启用且首部提供有效目的地址时使用该地址。UNKNOWN、LOCAL、无首部及 unspecified 目的地址回退真实 socket；无法确定的本地地址保持空。UDP 业务仍携带真实外层 TCP 的入口。来源 IP 与目标访问地址保持各自语义，不用 machine_ip 猜测回填。
- 相邻修复：PROXY v2 LOCAL 必须消费完整长度字段指定的数据，避免污染后续握手；v1 读取限制 108 字节，并校验地址族、目的 IP 和端口。共享来源解析入口继续供 Mieru 使用，既有回归通过。
- 验证：临时回归先复现直连/转发 Local 缺失和 LOCAL 残留，再验证 PROXY v1/v2、IPv4/IPv6/mapped、TLV、禁用转发、缺失/非法/截断首部及 TCP/UDP 上下文。GOEXPERIMENT=jsonv2 go test ./core ./core/app/dispatcher ./common/accessaudit ./limiter ./api/v2board ./node -count=1 -timeout 60s 六包通过；git diff --check 通过。临时用例移至仓库外，不提交新测试文件。
- 下一处/边界：需重编并发布节点后，以新连接 entry_ip → aliveDevices → 后台实时入口、审计 → ingest → ClickHouse 历史逐段验收；本次未部署、未改 SQL，也未处理同机 Trojan 空字段的独立原因。“客户端上报入口”由 /user/app/entry/report 独立提供，不由本修复填充。
- 相关：core/sntp_eclipse.go、core/sntp_eclipse_native.go、core/app/dispatcher/default.go、core/app/dispatcher/flow_traffic.go、limiter/limiter.go、api/v2board/user.go；面板 docs/uniproxy-node-troubleshooting-runbook.md 的同日入口 IP 排查条目。修复与本条同一提交，可用 git log -- core/sntp_eclipse.go 定位。


## 2026-10-15：重启准备并发与局部配置切换

- **症状**：更新程序、增删节点或改配置后业务长时间不可用；重载期间所有节点一起停服。
- **链路**：`cmd/server.go -> node.New/prepareController -> loadBootstrapState -> 面板配置/用户/在线状态 -> core -> Controller.Start`；文件修改经 `Conf.Watch -> reload`。
- **根因**：多节点及各接口串行准备，用户重复全量下载；旧重载先关闭所有节点；文件监控固定等待 5 秒且不支持原子替换。
- **修复**：节点最多四并发，配置后用户/在线状态并行；最近 24 小时内、凭据绑定且已全量验证的快照先在线追平增量，失效/未追平要求成功全量，旧快照首次升级仍全量；无变化增量检查点每分钟最多一次。先准备和校验候选，再局部增删/替换；共享核心或路由变化整体切换，监听失败恢复旧运行态。目录监听加末次事件后 250ms 防抖。
- **授权边界**：401/403 不用快照绕过；保留节点也提交在线授权变化，停止旧节点后再次继承较新用户序号，回滚保留最新撤销；托管 TLS 待激活控制器先持久化授权。实测发现核心 TCP 直连可绕过管道关闭，因此连接登记增加真实 TCP 流关闭入口，撤销/移除节点时关闭；不关闭共享 UDP 套接字。
- **回滚边界**：Xray 监听绑定失败会先注册 tag，必须清理本次失败 handler 才能恢复旧监听。受影响节点连接不保留；完整程序更新不提供跨进程连接迁移。
- **验证**：临时回归先失败再通过；实际 VLESS TCP 连续传输下修改其他节点端口、增删节点；端口占用局部/整体回滚后代理恢复；保留节点用户撤销真实断开；较新运行状态继承、旧/未来/未绑定快照、无效/不完整增量、非法配置、原子替换及待激活 TLS 均覆盖。执行 `GOEXPERIMENT=jsonv2 go test ./node ./cmd ./conf ./core ./api/v2board ./common/task ./core/app/dispatcher -count=1 -timeout 60s`。
- **下次检查**：优先看 `stage/elapsed_ms` 耗时；确认状态目录跨更新持久化、首次全量后有 `fast_sync_ready`；分开准备时间与共享核心切换时间，生产使用真实代理请求验收。Windows 缺 C 编译器，race 检查未执行；Linux/systemd 与生产秒数待部署验证。
- **相关文件**：`node/bootstrap.go`、`node/reconcile.go`、`node/offline_state.go`、`node/task.go`、`cmd/server.go`、`conf/watch.go`、`core/node_preflight.go`、`core/inbound.go`、`core/app/dispatcher/linkmanager.go`；操作说明见 `docs/panel-offline-mode.md`。

## 2026-10-17：已删除节点阻断整机路由更新

- 症状：面板删除节点后，本机配置仍残留其 NodeID；修改其他节点的路由时，整机刷新因已删除节点返回错误而中止，有效节点继续使用旧规则。启动和配置重载也可能被残留节点阻断。
- 影响链：v2board `ServerController::__construct` -> `/api/v2/server/config` -> `api/v2board/node.go:GetNodeInfo` -> `Controller.RefreshObserved` / `loadBootstrapState` -> `routeUpdateCoordinator.refreshVector` / `node.New` -> 监听清理、路由发布、离线快照和重载回滚。
- 根因：面板用 HTTP 200 的 `status=fail,message=server is not exist` 表示不存在，旧节点端按普通配置解析成协议为空；协调器将所有请求错误视为整批失败，离线启动还可能回退旧授权。
- 修复：精确识别节点不存在的业务响应；鉴权拒绝、普通 404、5xx 和超时不归类为删除。默认热更新路径停止该节点监听/连接/任务并排除其路由，其余节点继续发布。启动跳过删除项并按有效控制器启动，避免索引错位；回滚不恢复已删除节点。保存凭据绑定的删除标记并禁止后台旧快照覆盖；全节点删除允许空路由快照；整机快照匹配时排除有删除标记的配置项。
- 验证：临时回归先复现“残留节点阻断启动”和“删除节点阻断路由更新”，修复后通过；覆盖错误分类、旧快照不能复活、同 ID 在线恢复、凭据变更、全部删除、重载回滚、临时网络错误及关闭失败重试。本地真实 VLESS 测试从配置监控触发删除，确认被删监听和已连接会话关闭、另一节点持有连接与新连接仍能传输，共享核心未替换。测试专用 loopback 出站显式放行回环；不修改业务默认出站限制。
- 下次检查：先核对 config 接口原始业务响应，再检查 `Skipping deleted panel node`、`Deleted panel node stopped; remaining nodes continue serving`、`Route runtime generation published`，以及状态目录权限与目标业务端口。不要把所有 HTTP 404 或请求失败当作节点删除。
- 检查命令：设置 `GOEXPERIMENT=jsonv2` 后执行 `go test ./api/v2board ./node ./cmd ./core ./common/task -count=1 -timeout 60s`，全部通过；9 个节点回归场景及 9 类 API 响应验证通过，补测确认残留删除项不会让重启丢弃最新整机路由快照。`gofmt -l`、`git diff --check` 通过。本机无 gcc/clang，未运行 race 检查；Linux 和生产链路待部署验收。
- 边界：本次未部署、未执行生产 SQL，未修改规则内容和本机配置文件；关闭路由热更新仍沿用配置重载，无法承诺其他连接连续性。同 ID 恢复须在线配置重载/重启并获取完整授权。已有其他工作区修改保持不变。临时测试验收后删除。
- 相关文件：`api/v2board/node.go`、`node/bootstrap.go`、`node/node.go`、`node/controller.go`、`node/task.go`、`node/reconcile.go`、`node/offline_state.go`、`node/route_update_coordinator.go`、`node/route_runtime_state.go`；运维入口见 `docs/panel-offline-mode.md` 的“面板已删除节点”。提交：见本条记录所属提交。


## 2026-10-17：独立管理通道清理已删除节点配置

- 症状：业务控制器在节点删除后停止，挂在控制器内的更新任务无法再收到命令；全部节点失效或启动失败时尤其明显。
- 链路：实际 `--config` → 面板/凭据/配置实例身份 → 30 秒管理清单 → 定向任务 → 主库复核 → 配置备份/原子替换 → 单节点停用 → 持久回执。
- 修复：`node/cleanup_control.go` 独立于业务控制器；`cmd/server.go` 启动前建立管理循环，启动失败继续重试；清理由主循环串行执行，与更新任务共用锁。
- 安全边界：`node/cleanup_config.go` 使用 JSON RawMessage 保留未知字段/大整数，拒绝重复键/歧义字段/非 JSON 与摘要冲突；只删匹配 ApiHost、ApiKey、NodeID 的配置。
- 运行态：`node/cleanup_runtime.go` 停用指定控制器并应用剩余路由，不重启正常节点；watch=false 仍生效，配置监听只忽略自己完成的写入。
- 恢复：本地 0600 `.orphan-control.json` 记录执行意图及结果，清理最后一项后仍可跨重启补报；凭据不可外传，确认后删除，备份保留。
- 验证：Go 指定包回归、临时 HTTP/配置/全部失效启动测试及真实本地 VLESS 已有/新连接连续性验证通过；临时测试已删除。Windows 本地不能替代 Linux 挂载与正式部署验收。
- 下次检查：管理接口认证、任务摘要/期限、本地目录可写性、待回执日志；面板发布入口见 v2board 的 `docs/node-orphan-cleanup-runbook.md`。


## 2026-10-17：审计队列额外写盘与管理重试的诊断边界

- 症状：更新前后日志出现 audit persistence queue full、面板连接重置与独立管理通道重试，用户曾遇到不可用并手动重启。当前运行不能反证重启前无故障。
- 已核对链路：systemd/MainPID/版本构建元数据 → 实际监听与 socket 双向字节增量 → 配置/快照 → 独立心跳 → accessaudit 非阻塞接收/持久队列。线上 .82 对应 254e672，首次约 11 秒启动，人工重启后约 1 秒；当前业务有流量、心跳 HTTP 200，但审计队列满仍复发并伴随 I/O 等待。
- 可复现代码问题：boltSpool.peek 无条件 db.Update；即使未清理过期数据、未记录缺口，bbolt 仍提交并重写 freelist/fsync，与落盘写入器争用。该额外负担不能直接证明历史不可用的全部因果。
- 修复：无过期队首且没有待保存缺口时，在同一只读事务中检查和读取；需要维护才执行原写入路径，保留 fsync、缺口、过期、FIFO 和上传确认。管理错误增加受限原因码/HTTP 状态，不泄露 URL/凭据/响应；恢复提示对默认 warning 级别可见，不把管理告警当业务启动失败。
- 验证：临时回归先证明空/非空队列各 20 次读取产生 20 次提交，修复后均为 0；过期清理/缺口重启恢复、403/404/503/异常响应/超时分类与脱敏通过。GOEXPERIMENT=jsonv2 go test ./common/accessaudit ./node ./cmd ./core/app/dispatcher -count=1 -timeout 60s 通过。临时测试完成后删除。
- 下一处：发布后比较队列深度/缺口增长、磁盘 I/O 等待、真实代理请求；管理失败看 reason/http_status，业务启动看独立 err 和监听。spool 文件大小含空闲页，不能当作 pending_bytes；不删除队列、不禁用同步刷盘。
- 边界：本次未改线上二进制/配置、未重启、未推送或发布；不宣称仅此补丁即可消除慢盘/过载，也未完成原用户客户端故障复现。文件：common/accessaudit/spool_core.go、node/cleanup_control.go；运维说明：v2board/docs/node-orphan-cleanup-runbook.md。提交号用本节所属提交定位。

## 2026-10-21：Xray v26.10.21 CA 私钥事件的 v2node 影响核查

- 问题：上游紧急版移除 emSign ECC Root CA - C3、emSign ECC Root CA - G3、emSign Root CA - C1，并用修补后的 Go 拦截 8 个问题证书公钥；这属于 TLS 服务端身份信任风险，不能仅凭 Xray 版本早于被撤版版本判定安全。
- 已核对链路：go.mod 的 replace → 实际缓存中的 wyx2685/xray-core 6f1e299a2bce → Linux TLS getCertPool/SystemCertPool → Go crypto/x509；同时检查面板 Resty HTTPS/WSS、更新包 http.DefaultClient、Xray TLS 出站和 DoH/DoQ，以及 CI/Docker 编译版本。
- 修复前：定制核心为 2026-05-04 提交，没有此次被移除的内置 x509roots/fallback 包；但默认信任系统 CA。release.yml 和 Dockerfile 固定 Go 1.26.1，缺少 containsDistrustedSPKIHash 拦截。因此若运行二进制仍使用未修补 Go 且有效信任库仍信任问题证书链，存在同类中间人风险；需要攻击者具备流量拦截或重定向条件，不等于已发生入侵或数据泄漏。
- 修复：go.mod 最低版本提高到 Go 1.26.10；release.yml 改为从 go.mod 读取编译版本，与现有 CodeQL 配置共用来源；Docker 构建镜像升级到 golang:1.26.10-alpine。保留定制核心和依赖版本，后续新产物使用已修补的证书校验器；不会自动修补线上旧程序。
- 发布后续：仍需重新编译、发布和替换运行二进制，并更新实际运行环境的 CA 库；当前 Xray 会缓存根证书池，不能只更新宿主机包就宣称进程已生效。Docker 还要检查容器内 CA。不要直接以官方 xray 程序覆盖 v2node。
- 验证：读取 Xray release/PR #7212、golang/crypto 提交 9f2b28d4331a，以及 Go 1.26.1/1.26.10/1.27.3 官方源码；再次确认 Go 1.26.10 的证书链过滤及 8 个 SPKI 拦截值、官方 Linux amd64/arm64 发行包与 Docker 对应镜像可用。go mod edit -json 解析、构建版本一致性检查与 git diff --check 通过。按仓库约束未运行构建、漏洞利用或生产检查，编译兼容性和线上验收待发版验证。核查基线 v2node 8255517。
- 下一处：从具体节点运行进程定位二进制，用 go version -m 检查实际编译版本/模块；检查有效 CA 信任源、SSL_CERT_FILE/SSL_CERT_DIR 和自定义信任/固定证书配置，再以证书拒绝验证及真实面板/代理请求验收。仅升级机器上的 Go，不会修补已运行的旧程序。
- 相关：go.mod:238、.github/workflows/release.yml:58、Dockerfile:2、api/v2board/panel.go:70、api/v2board/wakeup.go:105、node/update.go:235；依赖 transport/internet/tls/config_other.go:18。上游：https://github.com/XTLS/Xray-core/releases/tag/v26.10.21 ；https://github.com/XTLS/Xray-core/pull/7212 。
- 范围：本次仅修改 go.mod、Dockerfile、.github/workflows/release.yml 和本条记录；保留其他未提交记录。用户授权本地提交，未推送、发布、部署或重启；线上逐节点暴露状态与是否遭利用均未确认。提交号用本节所属提交定位。


## 2026-11-02：Linux TCP 内核漏洞与默认 MPTCP 监听缓解

- 症状/背景：上游作者建议升级以降低 Linux 漏洞利用风险；核对上游 main 的 5935d2e、dde9f23 与 v0.4.7，以及 Go Security Team 对 CVE-2026-98432 的公告。漏洞位于 Linux IPv4 TCP 内核路径，已公开的远程执行链使用 MPTCP 监听；不能据此断言普通 TCP 无风险或 SNTP 节点已被入侵。
- 影响链：go.mod / release.yml / Dockerfile 的编译默认 → Xray transport/internet/system_listener.go → 代理 TCP 入站；另查 core/sntp_eclipse.go:Start、core/mieru.go:mieruListenerFactory.Listen。当前 Go 1.26.10 仍继承 Go 1.24+ 的默认 MPTCP 监听；旧 Xray 只在 TcpMptcp=true 时显式开启，false/未配置不会显式关闭。Titan 和 Mieru 入口也未显式关闭。
- 本地修补：go.mod 增加 godebug multipathtcp=0，使后续编译的主程序及依赖默认关闭 MPTCP；保留 Go 1.26.10、定制 Xray/REALITY、go.sum、CI 和 Docker 版本。运行环境 GODEBUG 和代码 SetMultipathTCP(true) 可覆盖编译默认，发布验收必须核对有效配置。
- 上游变更：11 月 2 日将 Go 升到 1.27.3，Xray fork 固定 dcfae7c4776b，REALITY fork 固定 25ea3c0fa255（含 X25519 兼容），加入主模块 MPTCP 默认并整理依赖。新 Xray 默认监听器显式关闭 MPTCP。我们当前定制核心仍为 6f1e299a2bce，不能直接替换整个依赖图或用官方二进制覆盖 SNTP。
- 兼容边界：已发现本地 core/app/dispatcher/fakednssniffer.go 使用 errors.Error.AtError()，而上游新核心 dcfae7c4776b 已删除该方法；完整内核升级至少需要适配这一调用，并验证用户撤销、限速/流量审计、DNS/路由热更新、REALITY、Titan 和 Mieru。此处仅确认一个静态接口差异，不宣称已穷尽全部兼容问题。
- 验证：实际 go.mod 经 Go 1.26.10 的 go mod edit -json 解析；复制实际 go.mod 到仓库外临时最小主模块，通过 go list -find -mod=readonly -f '{{.DefaultGODEBUG}}' 对照，修补前为空（继承工具链默认），修补后为 multipathtcp=0；临时目录已自动删除。完整项目 go list 的依赖解析未在检查窗口内结束，已停止；隔离检查仅证明编译默认，不等于完整编译或 Linux 实际监听验证。
- 线上处理顺序：先查运行内核/发行版补丁状态、实际运行二进制的 go version -m、systemd Environment 与自定义 socket 配置；对旧程序可在维护窗口以 GODEBUG=multipathtcp=0 合并原有环境参数并重建监听作为缓解；随后发布自定义修补产物，并升级到发行版已修复 CVE-2026-98432 的内核，启动修补内核后验收。容器共用宿主内核，网络命名空间和其他程序也要核对；不要把仅更新 Go、仅升级 v2node 或关闭 MPTCP 说成修复了内核漏洞。
- 下一处：core/sntp_eclipse.go、core/mieru.go、core/inbound.go、go.mod、script/install.sh 的 systemd 段；依赖 transport/internet/system_listener.go；公告 https://groups.google.com/g/golang-announce/c/PCoQcdxoScw/m/aD-MgXEvAAAJ ，上游 https://github.com/wyx2685/v2node/pull/52 。
- 范围：仅改本地主模块默认值并追加本记录，保留原有未提交记录；用户授权本地提交；未完整构建、推送、发布、连接生产节点、执行 sysctl、修改服务或重启。生产暴露条件与利用情况待逐机核查；提交号用本节所属提交定位。


## 2026-11-02：完整核心升级、Mieru 移除与定制运行链兼容

- 症状/目标：作者更新到 Go 1.27.3 和新 Xray/REALITY fork；SNTP 旧核心直接更换依赖无法编译，且用户要求保留自有协议、用户同步、流量/访问审计和 DNS/路由热更新。用户明确同意移除 Mieru，并批准本地完整编译、回归和 Linux amd64/arm64 交叉编译。
- 链路：面板 GetNodeInfo / 用户增量 -> core.AddNode / AddUsers / DelUsers -> 定制 dispatcher、限速与连接登记 -> 版本化 DNS/路由出站 -> 计费计数 / FlowTraffic 包装 -> bbolt 队列 -> 审计 HTTP 接收端。保留这条链和自有协议 wire，不用上游完整源码覆盖本地实现。
- 固定版本：作者 main dde9f237fad986dabe662db18025fb9ae349b5bf；require Xray v1.260930.0，实际 replace wyx2685/xray-core v0.0.0-20261102040409-dcfae7c4776b，核心运行版本 26.10.31；REALITY 全版本 replace wyx2685/REALITY v0.0.0-20261102081311-25ea3c0fa255，保留 standalone X25519；Go 1.27.3，继续 godebug multipathtcp=0。Docker builder 同步，CI 已从 go.mod 取版本。模块图 tidy，bbolt 仍为 1.3.2，不更换审计格式。
- 接口根因与修复：新核心移除 errors.Error.AtError、旧 singquic 传输、Shadowsocks NONE 枚举及 proxySettings。分别保留原错误、注册 Hysteria/TUIC 传输、拒绝 none/plain，并在路由首次校验和物理 tag 重写前统一迁移 legacy proxySettings 到 sockopt.dialerProxy。校验字段类型、依赖缺失、循环和冲突；transportLayer:false 的非 TCP/REALITY/header/socket 等配置过去被忽略，不能迁移后悄悄启用，现要求明确迁移，失败沿现有降级/回滚路径处理。
- SS2022 根因与修复：旧占位用户把随机十六进制文本再 base64，解码长度为 64 字节，新服务端严格校验会报 bad key；占位用户改为 AES128 16 字节、AES256 32 字节随机密钥，真实用户密钥格式不变。AES128/AES256 多用户实测通过；上游 Chacha20 多用户服务端不支持，不能误报为已兼容。
- AnyTLS 根因与修复：GetUser 只读延迟约 200ms 的认证快照。Add 后立即 Del，快照为空导致漏清 pending add；Del 后立即 Add，快照仍存在导致漏清 pending remove。异步管理器的增删均提交到待处理队列，覆盖相反操作，不逐用户等待 200ms。两个方向均先以临时测试复现失败，再验证修复通过；既有批量失败回滚/UID 提交路径继续回归。
- 审计/热更新根因与修复：原始 socket splice 可绕过 link 包装，导致流量审计或代次租约失真；有 route lease 或 flow session 时设置 CanSpliceCopy=3，保持受跟踪复制。临时用例先复现绕过状态再通过。生产绑定了路由管理器，因此部分直连会失去 splice 优化；尚未吞吐压测，不能承诺性能不变。
- 真实验证：VMess、VLESS、Trojan、经典 SS、SS2022 AES128/AES256、Hysteria2、AnyTLS 以真实本地客户端完成 TCP 代理转发；TUIC 用 QUIC v5 认证/流中继客户端验证入站。Xray 的 TUIC outbound 在升级前后均未完成，不能用它的 TODO 报错判定节点入站坏了。
- REALITY 验证：自签名本地 TLS1.3/X25519 目标无会话票据，临时 fixture 仅预置已知的空 post-handshake profile，实际加密握手和认证不 mock。新 freedom 默认拒绝标准协议访问私网，测试配置只精确允许本地 echo 端口；真实 VLESS 链式转发、热切换前后旧/新连接、撤销真实断连、旧出站回收和审计事件落盘/上报均通过。native-1 真实加密握手、TCP echo、双向计数和撤销断连通过。现有 route_hot_reload_integration_test 的 echo 直连不经过代理，不能单独据其名字宣称已测全链路。
- 核心检查：GOEXPERIMENT=jsonv2、CGO_ENABLED=0，go test -json -p 8 -mod=readonly ./... -count=1 -timeout 90s；现有 259 个顶层用例、含子测试 367 个通过（9 个测试包；14 个无测试文件包）。仓库外 overlay 补测，不保留新增测试文件。构建 Windows amd64 与 Linux amd64/arm64；用 go version -m、ELF machine 和 SHA256 核对工具链、fork、默认 MPTCP 和架构，产物为本地验证版本，不是已发布 release。
- 工具排障：旧 govulncheck v1.7.0 由 Go 1.26 编译，分析 Go 1.27 泛型方法/标准库时错误；临时用 1.27.3 重编扫描器后语法加载通过，但官方 vuln.go.dev 漏洞库连接 EOF，扫描未完成。go mod verify 仅验证模块完整性，不能替代漏洞扫描。没有据此声称无漏洞。
- 下次核查：先看 docs/core-upgrade.md 的固定版本和配置边界，再看 core/route_compile.go、core/user.go、core/app/dispatcher/default.go 与 core/inbound.go；发布需走自有 release，核对安装/节点自更新实际下载源、运行二进制、服务环境和真实代理请求。Linux 内核仍需发行版修补并启动修补内核，容器共用宿主内核；程序升级和关闭 MPTCP 只是应用侧缓解。
- 范围/剩余：只改 v2node 本地源码与说明，保留本记录追加前文件的完整字节前缀和已有约 282 行未提交记录。未改 v2board/客户端/数据库，未推送、发布、生产部署或重启。Linux 实机、race、全协议 UDP/三端矩阵、吞吐和长期稳定性待后续验收；早先 MPTCP 默认修复已经单独提交 c6fce73，此次完整升级随本条记录进行本地提交，提交号以本节所属提交定位。运行/发布指南见 docs/core-upgrade.md，实施计划见 docs/superpowers/plans/2026-11-02-core-upgrade.md。

## 2026-11-05：Shadowsocks 多出口轮询兼容单出口配置

- 症状/链路：用户需要多台出口分担总带宽，且已有单台 JSON 必须原样使用。面板 action_value → GetNodeInfo → core 路由编译 → 版本化出站与 router → 路由热更新/快照。固定核心要求每个 SS 出站只有一个 server，直接提交多个原先会构建失败。
- 改动/新行为：仅多项 Shadowsocks settings.servers 自动拆为独立成员并生成 roundRobin balancer；单台及 address 简写保留旧解析、protobuf、物理标签与直接路由。完整组先校验再注册，重复同名同配置去重，冲突拒绝；继承传输和单出口依赖，无效成员整组拒绝，错误不包含原始配置/密码。组作为 socket 代理的引用明确拒绝，既有单出口依赖不变。
- 代次/回滚：成员和组按配置版本标识，当前代次只选择自己的物理成员。启动/热更新校验候选组及成员，旧连接保留旧出站，无效更新保留旧代次；快照继续保存原始 action_value，不改面板 API 或数据库。兼容启动仍可能跳过无效组并标记降级，不把 active 当成负载组健康。
- 验证：先复现多台 JSON 编译失败，再验证单台/简写与旧 Build 的 protobuf、物理标签及路由目标一致；本地双 SS AEAD 加密转发各分配一半新连接，替换成员后新连接使用新组、原加密连接持续收发；同时覆盖依赖、冲突、route/route_ip/default_out、null/无效成员、错误脱敏、兼容整组移除、多台切回单台、旧成员回收及原始多台配置快照往返。测试均补入既有测试文件，无新增测试文件。
- 边界/下一处：等权按连接轮询，不自动探活/剔除/重试，不按字节均摊，也不叠加单连接带宽。回退旧二进制前先恢复单台配置；生产多台配置只有升级后才能使用。本轮未改 v2board/客户端/数据库；源码按本条所属提交定位，未推送、发布、部署或连接生产；Linux、UDP、吞吐及长期稳定性待上线前后验收。配置示例及命令见 docs/route-load-balancing.md；实现入口 core/route_balance.go、core/route_compile.go、core/route_runtime.go；保留本文件及 docs/log-troubleshooting.md 既有未提交内容。

## 2026-11-05：带流量的 SS 冷启动会阻塞初始用户加载

- 症状：版本升级/回退后 active 但业务未完整就绪，只有首个 SS 端口监听，CPU 高，后续节点未完成启动；v5.0.0.91 与 v5.0.0.90 均观察到。
- 链路/根因：`Controller.startRuntime` → `AddNode` 先发布监听 → `AddUsers/addManagedUsers` 逐用户安装；正在接入的 SS 认证持 Validator 读锁全量扫描，启动 Add 等写锁并受持续请求拖慢。pprof 主协程卡于 `Validator.Add`，连接大量排队于 `GetBehaviorSeed/GetWithCache`。两个节点各有数千用户；未观察到多成员出站，本次不能归为多出口解析错误。首次采样的审计 bbolt 页扫描很快进入下一阶段，不是持续卡点。
- 应急/验证：已备份远端配置；临时只拦目标业务 TCP/UDP 入站，重启后数秒完成全部节点初始化，随后撤销 IPv4/IPv6 临时规则。两个入站均通过 AES-128-GCM 加密固定 IP HTTP 请求；主节点额外返回 Google HTTP 204。第二节点域名探测超时，固定 IP 成功，DNS 原因尚未定位。最终恢复原配置且关闭临时 loopback 诊断端口，线上保持用户已回退版本。
- 源码修复：`core/node.go:AddNodeWithUsers` 先在离线 handler 加载完整凭据，再发布监听；`node/controller.go` 使用一次激活。失败清理 handler、UID 映射与连接登记；拒绝同 tag 覆盖；旧单出口 JSON、旧 AddNode 及后续增量同步保持兼容。
- 回归：既有 `core/inbound_reload_test.go` 验证 2048 用户在 AddHandler 前全部可查、重复启动不伤旧用户、端口冲突不残留 handler/UID。先运行新测试观察缺少原子激活能力的失败，再修复；`go test ./core ./node -count=1 -timeout=90s` 和 core/dispatcher/node 的路由与热更新专项通过。未新增测试文件。
- 下一处/边界：详见 `docs/log-troubleshooting.md` 同日“SS 在带流量重启时卡在用户加载”；入口 `core/node.go`、`core/inbound_reload.go`、`core/user.go`、`node/controller.go`，诊断配置 `conf.Conf.PprofPort`。源码修复随本条记录提交，尚未构建、部署或发布；已恢复线上服务不等于永久修复已发布。未改生产规则、数据库、审计记录或其他机器，保留既有未提交改动。


## 2026-11-14：DNS 配置、缓存和解析上下文必须同时按节点隔离

- 症状/根因：同进程双节点分别下发不同地区 DNS，同域名仍命中第一节点。链路为 NodeInfo 聚合 → compileRouteRuntime 的共享 config → contextless 出站/DNS 查询。只拆配置对象无法修复实际出站，不能把正式/探测入站分开或把节点状态写进共享出站。
- 修复：NodeDNSConfigs 独立编译并进入配置哈希；每代按节点建立 DNS backend/缓存，探测别名指向其所属节点，二次转发重写标签不改变原始入口的 DNS 归属；RouteRuntimeLease.Context 绑定带租约的解析器。固定核心源码随仓库维护 7 个 context 补丁，覆盖 DNS 截获、路由、targetStrategy、Freedom 和 UDP endpoint 解析，保留原解析与安全策略。
- 生命周期：新连接用新代，老连接保留旧代；异步 DNS 查询单独持租约，候选创建失败不替换旧代。IsOwnLink 仍聚合所有在用 backend，防止 DNS upstream 再被截获。缺少自定义 DNS 的节点不继承其他节点规则。
- 验证：先在旧实现观察 DNS/TCP 双节点同域名失败，再验证真实 AES-128-GCM SS 双入口、共享自定义出站、UDP 新域名、并发热更新、探测别名、旧查询持代、失败候选回滚及全量 Go 回归。测试用 loopback 目标受到核心默认私网规则阻挡时，仅在测试出站显式允许该范围，不能修改生产策略。Windows 无 CGO/gcc，未跑 race detector。
- 交付边界：本地提交由用户发布；需更新二进制并重启进程，之后路由规则仍可热更新。没有上线部署或宣称线上 JP/US 已验收；流媒体地区/播放与 AI 实际对话须经新版本真实入口复测。
- 下次入口：`docs/log-troubleshooting.md` 2026-11-14 章节、`third_party/xray-core/SNTP_PATCHES.md`、`core/route_dns.go`、`core/route_compile.go`、`core/app/dispatcher/route_runtime.go`、已有 `route_runtime_dns_test.go`/`runtime_dns_test.go`。升级核心时必须迁移 7 个补丁，不能直接恢复远端 replace。
