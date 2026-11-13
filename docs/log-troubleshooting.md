# v2node 日志排查速查

## 2026-11-06：接收面板托管的 CF 回源清单（本地实现，未发布）

- 新能力 `cloudflare_origin_cidrs_v1`；`api/v2board` 在普通配置请求中声明，解码 `base_config.cloudflare_origin = {managed:true,cidrs:[...]}`。未下发、关闭托管或旧面板仍使用 `core/cloudflare_ips.go` 的默认/官方自动快照；不要用版本号代替能力判断。
- 面板设置入口为“系统设置 → 节点 → CF 回源网段”，保存开关和完整 CIDR 文本到原有设置表，无需新建表。开关默认关闭；普通节点轮询取得新内容后按原有配置分类触发监听重载，关闭托管也走相同路径。不是即时无损热更新，不保证已有连接重新判定来源。
- `core/inbound.go` 将策略交给私有 `sntp-cf-websocket`；仅在已核实的普通 WS 路径中，将经过校验的列表编码到该私有传输的保留元数据槽。原网络设置不能自行注入该槽；该内容不作为 HTTP 请求/响应头发送。监听器建立每个入站独立快照，同进程不同节点/不同面板互不修改，官方更新任务不会覆盖托管列表。其他未托管入站仍引用自动更新的全局官方快照。
- 校验：1～256 条规范公网 CIDR，IPv4 最短 /8、IPv6 最短 /19；拒绝非法格式、未清除主机位、映射前缀、私有/保留地址及包含保留地址的大网段，空清单不能降级为信任全部。只允许单地址族也可去重；具体来源头安全规则和 PROXY/finalmask 等边界保持原逻辑。非法下发导致候选配置验证失败，不替换为不受限信任。
- 离线/回退：托管策略在原有 Common.BaseConfig 中保存，随既有离线节点快照恢复；没有新增独立缓存文件或全局面板覆盖表。恢复自动模式时采用当时全局官方快照，重启用内置清单再异步刷新。
- 本地回归：沿用 `core/custom_test.go` 验证自定义可信对端来源、默认范围被替换、多个入站隔离、官方更新不覆盖、关闭/缺省回退、元数据注入被清除与坏策略拒绝；`api/v2board/node_config_test.go` 检查真实 Client 的能力与解码，`node/node_info_change_test.go` 检查该字段变化触发既有重载。面板另有 31 项隔离 PHP 校验检查、13 项下发/默认检查和 TypeScript 检查。`go test ./core ./api/v2board ./node ./cmd -count=1 -timeout 120s` 通过；未发版、部署、重启或修改线上面板设置。

## 2026-11-06：CF WebSocket 来源地址恢复（本地源码，未部署）

- 症状/原因：CF 回源节点的在线 IP 变为边缘 IP，并出现 `sockopt.trustedXForwardedFor` 未配置警告。2026-11-02 升级至固定 Xray `dcfae7c4776b` 后，WebSocket 不再默认读取 XFF。该字段配置的是头名而非可信 CIDR；直接填入 CF 头不能防止公网伪造。
- 本地修复：`core/inbound.go` 生成普通 WebSocket 入站后选择 SNTP 的 `sntp-cf-websocket` 监听。`core/cloudflare_websocket.go` 保留原 Host/path、TLS、early data、heartbeat 与流封装，先按真实 socket 对端匹配 CF 官方 IPv4/IPv6 网段，再采用单值、合法公网 `CF-Connecting-IP`；Pseudo IPv4 覆盖模式下仅接受合法 `CF-Connecting-IPv6`。直连、非 CF、缺失/重复/非法头均保留 socket 来源，不回退到任意 XFF。
- 清单机制：`core/cloudflare_ips.go` 内置官方 15 个 IPv4、7 个 IPv6 网段；`cmd/server.go` 在进程启动时异步更新，此后每 6 小时从固定 HTTPS 官方 `/client/v4/ips` 获取，10 秒超时、拒绝重定向、64 KiB 响应上限、双地址族与网段校验、原子替换。失败保留内存中上次成功清单；重启先使用内置清单，不依赖 API 可用性、不新增磁盘缓存或第三方来源设置。新增同类节点自动使用，不需要逐个填写优选 IP。
- 业务链：连接 `RemoteAddr` → Xray `session.Inbound.Source` → SNTP dispatcher/limiter 的设备/IP 限制、在线/访问记录 → `node/user.go` 的在线上报。客户端优选入口 IP 和源站回源对端属于不同链路位置，不能把第三方测速 TXT 当可信回源清单。
- 适用边界：VLESS/VMess/Trojan/AnyTLS 等由现有生成器生成的普通 WS 入站。开启 PROXY Protocol 或 finalmask 的入站保持原传输，避免把可被替换的 RemoteAddr 当物理对端。CF → Nginx/其他中转 → 节点、Tunnel、Workers 特殊转发链以及 HTTPUpgrade/XHTTP/gRPC 均需单独核实可信链；本次不自动信任它们，也不在面板按 NodeID 打补丁。CF 若移除真实 IP 头，节点不能凭空还原。
- 本地验证：先添加入站选择回归测试并确认旧实现失败；现有 `core/custom_test.go` 追加官方网段、IPv4/IPv6/mapped、Pseudo IPv4、头伪造/重复/非法、坏清单保留快照、取消、Host/path、TLS WS early data/双向收发、关闭监听保留连接等覆盖；真实本地 VLESS 认证进入 limiter 后，`GetOnlineDeviceState` 的用户/设备来源在可信夹具下使用客户端地址、直连伪造下保留 socket 地址。循环回源白名单仅为测试夹具，正式内置清单不信任 loopback。测试目标限制在本机；Xray 默认 freedom 会 blackhole 回环目标，在线统计回归在认证/统计完成后结束，不修改出站保护或以其宣称代理转发验收。
- 发布/验收：源码与相关记录纳入本次本地提交，未构建发行包、推送、更新生产节点、重启或执行清理 SQL。发布需更新节点二进制；先备份实际运行二进制及校验和，再在一个 CF 节点验证版本、WS/TLS 可连、真实客户端新连接、后台实时来源和后续历史，然后扩展其他节点。切换需重连才适用新入站；旧 MySQL 历史、Redis 在线窗口和审计不会自动改写。回滚使用现场备份版本；不能用清空记录代替恢复真实来源。

## 2026-11-05 晚间：同 NodeID 原证书跨机对齐后恢复指纹共识

- 四组自签节点 230/231/761/762 的冲突来自两台活跃机器使用不同证书；不是观察 TTL 尚未到期。原机器的实际叶证书指纹与面板既有 pin 一致，761/762 临时新建证书只解决了启动，最终必须沿用同 NodeID 原证书以保留订阅信任。
- 先只读核对原文件、公私钥匹配、有效期、SAN、实际监听证书及面板 pin。新增机器先在 root 专用目录备份配置和旧文件，再以 SSH/SFTP 在内存中传递原证书及私钥，暂存校验后原子替换，目标文件 0600；不跨 NodeID 复制密钥，不改原机器。
- `node/reconcile.go` 会复用配置/节点信息相同的 Controller，改磁盘证书不保证进程已切换。此次只临时调整目标 NodeID 的 Timeout 触发其 Controller 重建，确认使用原证书后逐字节恢复原配置。两机各四个监听和主进程 PID/NRestarts 保持，无删除节点、作用域、直接 SQL、清缓存、强设 pin 或主进程重启。
- 北京时间 21:59，面板自然回报四组均为 consistent、两实例/一个指纹；已有 pin 值未改，后端仅自行补齐缺少的别名。四个目标本机/外部 PROXY v1 + TLS 1.3 实际证书匹配原 pin；761/762 显式信任原自签证书时严格域名校验通过，当前证书到期 2027-11-03 17:13:45（北京时间），临时重建证书的到期日已不适用。
- 22:05:38 管理端实际配置状态汇总确认三台目标机器的 14 个 NodeID 全部 applied，failed/blocked/unavailable 均为 0，回报距检查 7—40 秒。对应链路是 `ConfigStatusReporter` → `/api/v2/server/config-status` → `V2nodeConfigStatusService` → `NodeConfigStatusTag`；它与普通运行态和 TLS 证书观察相互独立。
- 230/231 原 SAN 与面板校验名存在既有不一致，原机器也能复现；没有把 pin 一致宣称为严格域名验收通过。客户实际代理认证/转发未测试。托管作用域 2 另行保持 v2、2/2、pending 0；self/file 没有该托管自动分发语义。
- 下次检查：`node/cert.go`、`api/v2board/node.go`、`node/reconcile.go`；面板 `ServerController::recordTlsPeerCertRuntimeStatus`、`V2nodeTlsCertificateReconciler`。新系统 Python 可能没有 `ssl.match_hostname`，域名验收使用实际 TLS 上下文的主机名验证，探针自身异常不能当成节点启动失败。

## 2026-11-05 晚间：托管凭据首次超时后卡住，作用域同步停在 1/2

- 链路：`Controller.Start` → `managedTLS.Prepare/Start` → `GetManagedTLSCertificate` → 自动领取凭据 → 证书安装 → 业务启动 → 正常状态回报。原实例已有 scope 2 / v2；新增实例约 20:30 凭据超时后，到 21:31 仍为 `managed_tls_config_invalid` / version 0 / token 未配置，无凭据文件及监听。
- 根因：TLS pending 时只运行证书管理器及专用状态报告器，配置轮询尚未启动；`validateConfiguration` 将缺凭据作为静态配置错误，拦截了取证 API 自带的自动领取，因而没有恢复入口。首次网络超时原因未确定，不解释成正常同步等待。
- 源码：只移除管理器中的 token 已配置前置条件，保留 client/store/scope/domain 检查。`managedTLSRequest` 仍负责正常领取、凭据校验及 HMAC，缺凭据不等于允许匿名取证。现有测试增加首次失败后重试及有效本地证书回退，先失败，修复后 `go test ./node -run '^TestManagedTLS' -count=1` 通过；修复和测试纳入本次提交，尚未构建、发版或部署。
- 线上恢复：用真实节点身份从正常接口领取并校验凭据，保存标准 credentials 文件（目录 0700、文件 0600）；备份配置，仅临时调整目标 NodeID 的 Timeout，使该 Controller 重建并自动下载已有证书，再恢复原配置值。其他 NodeID 配置逐项不变，主进程未重启，没有新签证书或人工 ready。
- 验收：约 21:34 目标监听恢复，21:38 面板自然回报聚合 2/2、pending 0；本机及外部 PROXY v1 + TLS 1.3 严格公共 CA / SNI 校验通过，证书到期 2027-01-15 21:04:46（北京时间），PID/NRestarts 不变。实际账号的代理认证/转发未验收。
- 下次入口：`node/controller.go::Start`、`node/managed_tls_manager.go::validateConfiguration`、`api/v2board/managed_tls.go::managedTLSRequest`、`managed_tls_credential.go`；面板 `tlsCredential` 和 `V2nodeManagedTlsStatusService`。curl 空 POST 显式发送零长度 body，检查 HTTP 200 内的业务错误码，避免混淆探针请求差异与应用故障。

## 2026-11-05 晚间：file 文件缺失挡住冷启动，补证书后恢复

- 两台 v5.0.0.92 / Debian 13.3 节点主进程 active，但整代证书预检分别报 `tls_certificate_missing`，因此各四个业务端口都未启动；与热加载失败时保留旧监听不同。file 模式不会生成或从面板下载证书，路径为空时展开成 `/etc/v2node/trojan<NodeID>.cer`、`.key`。
- 原配置允许自签证书但保存了旧 pin。用户选择补齐证书而非移出失败节点后，为目标 NodeID 重建独立、一年期、SAN 匹配 SNI/既有校验域名的自签证书；验证公私钥匹配，文件权限分别 0644/0600，不覆盖已有证书、不跨节点复制私钥。新指纹需要由运行状态上报形成面板共识，旧订阅需刷新；不要声称新证书恢复了原指纹。
- 一台随退避到期恢复；另一台先在服务器 root 专用目录备份原配置，再仅追加 JSON 尾部空白，原子替换配置以改变原始字节摘要。`CleanupManager.ConfigDigest` 对文件原始字节取摘要，`reloadRetryState.Allow` 对相同摘要仍执行最长 300 秒退避；仅触发相同字节文件事件不能保证立即重试。保留全部节点与业务配置值，不重启服务。
- 验收：两台均记录恢复成功，共八个业务端口本机/外部 TCP 可达；携带正确 PROXY v1 头后，各 Trojan 以本地证书作为明确的进程内信任完成 TLS 1.3 与域名校验，PID 和 NRestarts 不变。仍需单独核对面板 pin、客户端刷新和实际代理认证/转发；这些不由 TCP/TLS 检查代替。此次 file 证书到期日 2027-11-05。
- 相关链路：`api/v2board/node.go` → `node/cert.go` → `node/reconcile.go` → `cmd/server.go` / `cmd/reload_retry.go`，以及面板 `ServerController::recordTlsPeerCertRuntimeStatus` → `V2nodeTlsCertificateReconciler`。另一台五节点主机的托管凭据请求超时仅造成约 17 秒 startup 等待，现场已运行，不能误作本次缺文件根因。
- 后续未闭环：北京时间 21:09，四个相关 Trojan 的当前面板 pin 仍与本机证书不一致；真实证书/网卡状态补报 HTTP 200 且 `data=true`，配置加唯一查询参数后仍未变化。未取得其他同 NodeID 机器或面板观察缓存证据，不能确认多实例冲突、旧观察未过期或线上实现差异的具体原因；不强改 pin 或停用校验。待核对原来可用机器及面板共识后才可要求刷新客户端订阅，当前恢复结论仅覆盖启动与 TCP/TLS。

- 后续证实：21:30 面板缓存显示四个相关 NodeID 各有两台持续活跃机器，指纹不一致、校验名一致，`tls_status=conflict`，当前面板配置为 self；旧观察自然过期不足以解决。原实例 SSH 尚不可访问，需其登录方式后沿用同 NodeID 原证书；独立托管作用域 2/2 的恢复不代表这些节点已恢复信任一致性。

用于排查 SNTP 节点端用户访问、在线设备、连接拒绝、设备/IP 限制和 `blocked_ips`。

文档里的 IP 示例已脱敏成 `x`。线上日志会输出完整 IP，方便定位真实客户端和目标。

## 新增节点无监听与失败配置恢复（2026-11-03，本地源码）

先区分管理进程、配置应用、业务监听和真实代理连接。systemd active 只说明进程仍在；候选配置失败时，旧节点可以继续服务，新增节点却没有监听。查看 `journalctl -u v2node -n 100 --no-pager` 中的配置错误、NodeID 和 `retry_at`，再用 `ss -lntp` 对照目标服务端口。

`cert_mode=file` 要求运维提供已有证书和私钥。节点现在会在业务激活前检查路径、文件类型、可读性、PEM 和密钥匹配，错误码如下：

| 错误码 | 含义 / 下一处检查 |
| --- | --- |
| `tls_certificate_config_missing` | TLS 证书配置缺失，核对面板下发配置 |
| `tls_certificate_missing` | 证书路径为空或文件不存在 |
| `tls_private_key_missing` | 私钥路径为空或文件不存在 |
| `tls_certificate_unreadable` | 文件属性无法读取，或路径不是普通文件 |
| `tls_key_pair_invalid` | 文件内容无法读取、PEM 无效，或证书和私钥不匹配 |
| `tls_certificate_prepare_failed` | 其他证书准备失败，查看所在节点的本地错误 |

file 模式不会自行生成、替换或续签证书；`self` 保留原有自动生成逻辑。本次预检不新增域名匹配或信任链要求，证书过期维护与客户端可信验证仍按原有策略处理，不能把预检通过当作客户端验签成功。

失败后，冷启动和热加载都按 30、60、120、240、300 秒等待重试，之后每次最多等 300 秒。重试会重新读取当前配置与面板状态；补齐证书即可等待恢复，无需再次改写配置或重启整个进程。相同本地配置的面板通知不跳过退避；本地配置内容改变可立即尝试。管理通道删除节点后重试以最新文件为准，不会恢复已经移除的条目。

候选失败时沿用原有回滚规则，尽量保留旧运行态；无法恢复或清理的致命错误仍退出交给服务管理器处理。成功后清空重试状态，并在 warning 级输出“节点配置已恢复并生效”。这条日志在默认 warning 配置下可见。

面板新增独立 `POST /api/v2/server/config-status`：监听尚未启动也能异步报告 `failed`，受同机其他节点阻塞的条目报告 `blocked`，成功后报告 `applied`。报告每分钟续报，使用独立缓存 `V2NODE_CONFIG_STATUS_INSTANCES_<NodeID>`，按已有实例活跃窗口过滤过期条目；不会替代流量、TLS 或访问审计报告。面板只显示固定错误原因、受影响实例数、保留旧运行态数量和重试时间，不接收原始错误路径。`unknown` 表示缺少新鲜报告，`applied` 也不代替代理认证和目标请求验收。

发布依赖：重试和预检需要新节点二进制；管理标签还需要面板 PHP 后端与管理台资源同时更新。新节点连接旧面板时，独立接口可能返回 404，仅影响新状态展示；普通运行态和业务路径继续沿原接口工作。这是本地源码修复，尚未构建发行包或部署；完整验证与边界见 `LESSONS_LEARNED.md` 同日记录。

## 默认日志策略

新装生成配置时，`/etc/v2node/config.json` 的 `Log` 段默认使用安静配置：

```json
{
  "Log": {
    "Level": "warning",
    "Output": "",
    "Access": "none",
    "SNTPAccess": false
  }
}
```

这套配置的含义：

| 配置 | 含义 |
| --- | --- |
| `Level: warning` | 只输出警告、错误和 SNTP 业务访问审计，避免 Xray 底层 info 刷屏 |
| `Output: ""` | 写到 stdout，由 systemd journal 收集 |
| `Access: "none"` | 关闭 Xray 原生 access log，避免 `email: tag|uuid` 这类底层日志刷屏 |

`SNTP user access ...` 是 v2node 自己的本地业务访问审计，默认关闭，并且不需要把 `Level` 调成 `info`。如果临时需要在本机 journal 里排查访问明细，可以手动改为：

```json
{
  "Log": {
    "Level": "warning",
    "Output": "",
    "Access": "none",
    "SNTPAccess": true
  }
}
```

注意：更新不会自动重写已有 `/etc/v2node/config.json`。如果线上机器已经手动改过日志字段，需要按上面的示例手动改回默认值。

`Access` 的取值建议：

| 取值 | 含义 |
| --- | --- |
| `none` | 推荐，关闭 Xray 原生 access log |
| 空字符串 | 会被 v2node 当成 `none` 处理 |
| `console` | 显式打开 Xray 原生 access log 到控制台 |
| 文件路径 | 把 Xray 原生 access log 写到指定文件 |

日常排查优先看 `SNTP user access ...`，不要优先开 Xray 原生 access log。

## 远端 ClickHouse 访问审计

默认不会把访问明细发到远端。需要把访问明细写入独立日志库时，可以在 `/etc/v2node/config.json` 顶层增加 `AccessAudit`：

```json
{
  "AccessAudit": {
    "Enabled": true,
    "Endpoint": "https://logs.sntp.uk/api/v1/access-events",
    "Token": "替换为 ingest-api 的 INGEST_TOKEN",
    "BatchSize": 1000,
    "MaxQueueSize": 10000,
    "FlushInterval": "1s",
    "Timeout": "5s",
    "SpoolPath": "/var/lib/v2node/access-audit-spool/access.db",
    "MaxSpoolBytes": 1073741824,
    "MaxSpoolAge": "168h",
    "FlowTraffic": {
      "Enabled": false,
      "CheckpointInterval": "5m",
      "SpoolPath": "/var/lib/v2node/access-audit-spool/flow.db",
      "MaxSpoolBytes": 268435456,
      "MaxSpoolAge": "24h"
    }
  }
}
```

字段含义：

| 配置 | 含义 |
| --- | --- |
| `Enabled` | 是否开启远端访问审计上报，默认关闭 |
| `Endpoint` | ingest-api HTTPS 接收地址 |
| `Token` | HMAC-SHA256 签名密钥，必须与 ingest-api 的 `INGEST_TOKEN` 一致 |
| `BatchSize` | 单批最多上报条数 |
| `MaxQueueSize` | 等待本地批量事务的内存请求上限；不是远端积压容量 |
| `FlushInterval` | 定时刷新间隔 |
| `Timeout` | 单次 HTTP 上报超时时间 |
| `SpoolPath` | 普通访问记录持久队列，默认 `access.db` |
| `MaxSpoolBytes` | 普通访问队列容量上限，默认 1 GiB |
| `MaxSpoolAge` | 普通访问队列保留期，默认 168 小时 |
| `FlowTraffic.*` | 双向流量事件开关、检查点与独立 `flow.db` 边界；默认 256 MiB/24 小时 |

普通访问记录和 FlowTraffic 都先通过有界单写者批量事务写入各自 bbolt 文件，再由独立上传循环发送。网络、鉴权、限流、ClickHouse 5xx 或超时不会确认磁盘事件；进程重启后继续按 FIFO 补传。HTTP 400/413 会二分批次，只有确定无法接收的单条事件进入 rejected。

达到容量或保留期时，从队首裁剪并累计 dropped 数量、字节和时间范围；本地目录/磁盘错误、写事务失败或等待超过 1 秒时继续放行代理流量，但累计 persistence failure 并限频输出错误，不会静默当作成功。`SNTPAccess: false` 只关闭本地 `SNTP user access ...` 输出；只要 `AccessAudit.Enabled` 为 `true`，远端审计仍工作。

常用持久队列检查：

~~~bash
systemctl show v2node -p MemoryCurrent -p MemoryPeak -p NRestarts
du -h /var/lib/v2node/access-audit-spool/access.db \
      /var/lib/v2node/access-audit-spool/flow.db
journalctl -u v2node --since "-15 min" --no-pager \
  | grep -E "persistence|access audit|FlowTraffic|reportUserTrafficTask|panic|fatal|OOM"

# PprofPort 已配置且只监听 127.0.0.1 时
curl -fsS "http://127.0.0.1:6060/debug/pprof/goroutine?debug=1" | head -n 1
curl -fsS "http://127.0.0.1:6060/debug/pprof/goroutine?debug=2" \
  | grep -F -c "go.etcd.io/bbolt.(*DB).BeginRWTx"
~~~

## 日志在哪里

安装脚本部署的 systemd 服务可以直接用管理脚本：

```bash
v2node log
```

等价于：

```bash
journalctl -u v2node.service -e --no-pager -f
```

常用命令：

```bash
# 实时看最新日志，不带历史
journalctl -u v2node.service -f -n 0 --no-pager

# 实时看最近 200 行并继续跟随
journalctl -u v2node.service -f -n 200 --no-pager

# 看最近 30 分钟
journalctl -u v2node.service --since "30 min ago" --no-pager

# 看指定时间段
journalctl -u v2node.service --since "2026-05-23 04:30:00" --until "2026-05-23 05:00:00" --no-pager

# 查看服务状态
systemctl status v2node --no-pager -l
```

如果配置了 `Log.Output`，v2node 自身日志和 SNTP 访问审计会写到文件：

```bash
grep -A8 '"Log"' /etc/v2node/config.json
tail -f /path/to/v2node.log
```

Docker 部署时看容器日志：

```bash
docker ps
docker logs -f --tail 200 <container_name_or_id>
```

## 实时看用户访问

默认 `warning / Access none` 配置下就可以看 SNTP 访问审计。

按后台用户 ID 看实时访问，不带历史：

```bash
journalctl -u v2node.service -f -n 0 --no-pager \
  | grep --line-buffered -F "SNTP user access uid=145817"
```

按设备 UUID 或节点认证 UUID 看实时访问：

```bash
journalctl -u v2node.service -f -n 0 --no-pager \
  | grep --line-buffered -F "SNTP user access" \
  | grep --line-buffered -F "uuid=2386a7273a3ce22fa775118ce048de58"
```

按客户端来源 IP 看实时访问：

```bash
journalctl -u v2node.service -f -n 0 --no-pager \
  | grep --line-buffered -F "SNTP user access" \
  | grep --line-buffered -F "source_ip=222.128.x.x"
```

按访问目标看实时访问：

```bash
journalctl -u v2node.service -f -n 0 --no-pager \
  | grep --line-buffered -F "SNTP user access" \
  | grep --line-buffered -F "target=www.google.com:443"
```

访问日志示例：

```text
SNTP user access uid=145817 uuid=2386a7273a3ce22fa775118ce048de58 source_ip=222.128.x.x target=example.com:443 inbound_tag=[https://panel.example.com]-shadowsocks:514 outbound_tag=5555
```

字段含义：

| 字段 | 含义 |
| --- | --- |
| `uid` | 后台用户 ID |
| `uuid` | 节点认证 UUID。设备 UUID 模式下通常就是后台设备记录里的设备 UUID |
| `source_ip` | 客户端连接节点时的来源 IP |
| `target` | 访问目标，通常是域名/IP 加端口 |
| `inbound_tag` | 节点入站标签，格式通常是 `[面板地址]-协议:节点ID` |
| `outbound_tag` | 最终选择的出站标签 |

如果看到 Xray 原生 access log 里的 `email:`，它不是用户邮箱，而是 v2node 复用的内部用户标识：

```text
email: [https://panel.example.com]-shadowsocks:514|2386a7273a3ce22fa775118ce048de58
```

含义是：

```text
[面板地址]-协议:节点ID|节点认证UUID
```

这类日志来自 Xray 原生 access log。生产环境建议保持 `Access: "none"`，使用 `SNTP user access ...` 排查即可。

## 临时开 info 看更多细节

在线设备心跳、用户列表同步、流量上报、嗅探、路由 detour 等普通运行细节是 `info` 级别。需要这些细节时，临时改成：

```json
{
  "Log": {
    "Level": "info",
    "Output": "",
    "Access": "none"
  }
}
```

重启服务：

```bash
systemctl restart v2node
```

排查结束后手动改回默认 `warning / Access none`。

## 实时看在线设备心跳

在线设备心跳对应后台设备页的在线状态上报。需要临时开启 `Level: "info"`。

按用户 ID 看：

```bash
journalctl -u v2node.service -f -n 0 --no-pager \
  | grep --line-buffered -F "SNTP online device heartbeat" \
  | grep --line-buffered -F "uid=145817"
```

按设备 UUID 看：

```bash
journalctl -u v2node.service -f -n 0 --no-pager \
  | grep --line-buffered -F "SNTP online device heartbeat" \
  | grep --line-buffered -F "uuid=2386a7273a3ce22fa775118ce048de58"
```

心跳日志示例：

```text
SNTP online device heartbeat ip=222.128.x.x tag=[https://panel.example.com]-shadowsocks:514 uid=145817 uuid=2386a7273a3ce22fa775118ce048de58
```

## 查连接拒绝

连接拒绝通常是 `warning` 级别，默认配置即可看到。

实时看所有拒绝原因：

```bash
journalctl -u v2node.service -f -n 0 --no-pager \
  | grep --line-buffered -E "user_not_found|blocked_ip|device_limit_exceeded|rejected by limiter|SNTP Eclipse user not found|SNTP Eclipse user rejected by limiter"
```

按用户 ID 看拒绝：

```bash
journalctl -u v2node.service -f -n 0 --no-pager \
  | grep --line-buffered -E "user_not_found|blocked_ip|device_limit_exceeded|rejected by limiter|SNTP Eclipse user not found|SNTP Eclipse user rejected by limiter" \
  | grep --line-buffered -F "uid=145817"
```

按客户端 IP 看拒绝：

```bash
CLIENT_IP="222.128.x.x"
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "user_not_found|blocked_ip|device_limit_exceeded|rejected by limiter|SNTP Eclipse user not found|SNTP Eclipse user rejected by limiter" \
  | grep -E "source_ip=${CLIENT_IP}|client_ip=${CLIENT_IP}|${CLIENT_IP}"
```

拒绝原因：

| reason | 含义 |
| --- | --- |
| `user_not_found` | UUID 不在节点当前用户/限制表里，常见于节点还没拉到新用户表、用户表过旧、用户被删除 |
| `blocked_ip` | 客户端 IP 命中后台下发的 `blocked_ips` |
| `device_limit_exceeded` | 触发设备/IP 数限制 |

设备限制字段：

| 字段 | 含义 |
| --- | --- |
| `device_limit` | 后台给该用户设置的设备/IP 上限 |
| `alive_count` | 后端当前认为在线的设备/IP 数 |
| `pending_device_count` | 节点本地当前窗口内还没上报给后端的设备 UUID 数 |
| `cached_device_overlap` | 为避免后端缓存窗口重复计算而抵扣的数量 |
| `effective_device_count` | 最终用于判断是否超限的数量 |
| `device_limit_by_uuid` | 是否启用 UUID 设备限制模式 |

只有 `effective_device_count > device_limit` 时，才应按设备限制拒绝。

## 查 blocked_ips

实时看被 blocked 的连接：

```bash
journalctl -u v2node.service -f -n 0 --no-pager \
  | grep --line-buffered -F "reason=blocked_ip"
```

按 IP 查是否被 blocked：

```bash
CLIENT_IP="222.128.x.x"
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -F "reason=blocked_ip" \
  | grep -E "source_ip=${CLIENT_IP}|client_ip=${CLIENT_IP}|${CLIENT_IP}"
```

## 查设备/IP 超限

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -F "reason=device_limit_exceeded"
```

重点看：

```text
device_limit=1
alive_count=1
pending_device_count=2
cached_device_overlap=1
effective_device_count=2
device_limit_by_uuid=true
```

如果 `alive_count=1`、`pending_device_count=1`、`cached_device_overlap=1`、`effective_device_count=1`，说明后端缓存和节点本地 pending 被当作同一台设备处理，不应该因为缓存窗口重复计算而拒绝。

## 查 UUID 不在节点用户表

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -F "reason=user_not_found"
```

常见原因：

- 节点还没拉到最新用户表。
- 后端刚更新或重启，节点仍使用旧用户表。
- 用户已被删除或订阅已失效。
- 节点和后端通信失败，用户列表同步失败。

同时看用户同步日志。这里需要临时开启 `Level: "info"`：

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "Get user list failed|user deleted|user added|user modified|User list no change|Added [0-9]+ new users"
```

## UUID、UID 和设备 UUID

| 字段 | 含义 |
| --- | --- |
| `uid` | 后台用户 ID，表示账号本身。流量、在线设备数最终都按这个 ID 归属 |
| `uuid` / `tag|uuid` | 节点认证 UUID，客户端拿它连接节点 |
| 设备 UUID | 后台用户详情页“设备记录”里的 UUID。开启设备 UUID 模式后，会下发给 v2node 作为节点认证 UUID |

兼容迁移期如果开启 `device_uuid_keep_legacy_user_uuid`，节点用户表里可能同时存在：

- 旧用户 UUID：账号原始订阅 UUID，老配置可能还在用。
- 设备 UUID：设备记录里的 UUID，新订阅配置使用它。

排查时更稳的组合是：`uid` + 设备 UUID + 客户端 IP + 时间段。

## SNTP Eclipse 排查

SNTP Eclipse 为避免日志暴露完整 UUID，部分日志会输出脱敏 UUID，例如 `2386a7...48de58`。排查时优先用 `uid`、`client_ip`、时间段和 `reason` 组合查。

```bash
UID="145817"
CLIENT_IP="222.128.x.x"
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "SNTP Eclipse|rejected by limiter|user not found" \
  | grep -E "uid=${UID}|client_ip=${CLIENT_IP}|${CLIENT_IP}"
```

## 按 UUID 查不到日志怎么办

按设备 UUID 查不到日志不一定代表没有连接。常见原因：

- SNTP 访问审计被手动设置了 `SNTPAccess: false`。
- SNTP Eclipse 拒绝日志会脱敏 UUID，直接 grep 完整 UUID 查不到。
- `/etc/v2node/config.json` 配了 `Log.Output`，日志写到了文件，不在 journal 里。
- 查询时间窗口不对，或者服务刚重启导致你看的时间段没有对应事件。
- 服务器上的二进制不是包含新审计日志的版本。

先确认服务确实有日志：

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager | tail -n 100
```

确认新日志代码是否已经部署到服务器：

```bash
strings /usr/local/v2node/v2node | grep -E "SNTP user access|SNTP user rejected by limiter|cached_device_overlap"
```

确认是否写到了文件：

```bash
grep -A8 '"Log"' /etc/v2node/config.json
```

## 保存日志

保存最近两小时全部日志：

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager > /tmp/v2node-last-2h.log
```

保存某个用户最近两小时访问日志：

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -F "SNTP user access uid=145817" \
  > /tmp/v2node-user-145817-access.log
```

保存拒绝日志：

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "user_not_found|blocked_ip|device_limit_exceeded|rejected by limiter|SNTP Eclipse user not found|SNTP Eclipse user rejected by limiter" \
  > /tmp/v2node-rejects.log
```

保存某个客户端 IP 的日志：

```bash
CLIENT_IP="222.128.x.x"
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "source_ip=${CLIENT_IP}|client_ip=${CLIENT_IP}|${CLIENT_IP}" \
  > /tmp/v2node-client-ip.log
```

## 2026-11-05：SS 在带流量重启时卡在用户加载

- 症状：升级或降级后进程保持 active，核心已启动，但只出现第一个业务监听；CPU 高，后续节点未启动、状态未正常上报。此现象在 v5.0.0.91 和 v5.0.0.90 均有观察，不能仅凭时间先后归因于多出口配置。
- 已确认链路：`Controller.startRuntime` → `AddNode` 开启监听 → `AddUsers/addManagedUsers` 逐用户写入 → SS `Validator.Add`。入口已有流量时，认证的 `GetWithCache` 全量扫描持读锁，`Add` 等写锁；同时大量新连接排队在 `GetBehaviorSeed`。节点逐个启动，前一个用户未加完会阻塞后续节点。
- 证据采集：先核对 `systemctl show`、`ss -lntp`、最近启动日志。必要时备份配置，临时设置 `PprofPort`，只通过 `127.0.0.1` 获取 `/debug/pprof/goroutine?debug=2`；仅保留主协程及少数去重的相关栈，避免输出全部连接栈或配置/凭据。一次 `bbolt.freepages` 采样不表示持续卡在审计库，需重复采样确认主协程进展。
- 应急恢复：只临时拦截目标业务端口 TCP/UDP 的 IPv4/IPv6 入站，避免修改 SSH 与出站；重启并等待完整用户加载及所有业务端口就绪，再立即删除临时规则。保证异常路径也撤回规则。已实测短暂拦流后数秒内两个入站完成启动，并完成 SS AES-128-GCM 加密请求验证。
- 永久修复：`core/node.go:AddNodeWithUsers` 在尚未发布的 handler 上加载用户，然后注册并启动监听；`node/controller.go` 改为一次激活。端口绑定失败清理 handler、用户映射及连接登记，重复 tag 拒绝覆盖；旧 `AddNode` 与增量 `AddUsers` 保留。回归补入 `core/inbound_reload_test.go`。
- 恢复验收：业务端口监听加上真实加密转发，不能仅看 active；域名探测若超时，再用固定 IP 分开检查 DNS 与代理数据面。本次第二个节点域名探测曾超时，固定 IP 加密转发成功，DNS 超时原因未进一步确认。
- 发布边界：本轮线上保留用户已降级的版本；恢复原始配置并关闭临时诊断端口，临时防火墙规则全部撤销。源码修复本地验证通过，尚未构建、部署或发布；普通重启仍有旧版本启动竞争风险。现有单出口规则未更改。


## 2026-11-06：普通用户 SOCKS5 节点与路由标签

- 面板协议 `socks`，能力 `socks5_users_v1`；原生 TCP，`network_settings.udp` 默认关闭。用户名/密码均使用同步下来的账号 UUID（设备账号同样适用），拒绝匿名、HTTP 和 SOCKS4。没有给探测用本地 SOCKS 入口开放普通用户权限。
- `api/v2board/node.go` → `core/node_preflight.go` → `core/socks.go` 维护动态凭据/监听，复用核心 SOCKS 的 CONNECT、UDP ASSOCIATE；通过 `socksUserDispatcher` 映射既有 UserTag 后进入 dispatcher、limiter、计费和设备/在线上报链。撤销关闭已认证连接，停节点关闭监听及全部连接；用户变更不重建监听，监听配置变化走原 reconciliation。
- UDP 是每个认证控制连接分配动态端口，防火墙或 NAT 仅放行配置的 TCP 端口时，TCP 正常并不代表 UDP 可用。SOCKS5 代理协议本身不加密；没有新增 PROXY protocol 或 TLS 包装支持。
- 路由 action_value 可直接引用本机同时下发定义的标签，支持先引用后定义和 SS 轮询组；Default/block/dns_out 为内置引用，不能重新定义。严格编译缺失标签时拒绝更新；兼容启动仍按既有策略记录警告并跳过错误规则，不能忽略启动日志。
- 验证：本地真实 TCP/UDP echo、错误密码/匿名拒绝、UID 流量归属、账号撤销断连、重新授权及监听重建通过；面板生成 10 组不同协议/传输出站均经当前核心编译。API 能力/配置解析及相关已有路由、用户连接管理、启动与重载分类定向检查通过。临时测试文件与模拟配置检查后删除，没有修改依赖 fork/cache，也未打包、部署、压测或实机验收。
- 后台操作及完整参数/文件范围见相邻 v2board 的 `docs/server-routing-forms.md`。A→B 使用所选账号的参数快照，B 按该账号与倍率正常计费；不能把选择节点理解为自动跟随目标地址或免计费内部通道。

## 2026-11-06：路由回显复核与 DNS 显式全局设置

- 后台旧 JSON 标签回显问题发生在方式切换：空引用草稿覆盖完整定义。后台修复后会保留定义与引用草稿，并在保存时检查标签有其他定义；节点仍要求引用与定义同时下发到当前进程。DNS 查询 tag 不是代理出站标签。
- 本端发现 DNS 合并未复制 Script，且显式 false/0 被忽略；严格解析还拒绝只包含关闭开关的有效配置。现在保留 JSON 字段存在性，未提供的全局字段继承，显式值按接收到的路由顺序覆盖；服务器列表继续按原逻辑追加。该行为需更新节点二进制才会生效。
- 单服务器继续继承路由匹配；多服务器继续使用各自 domains。当前核心不允许 DNS script 与 domains/skipFallback/finalQuery、disableFallback/disableFallbackIfMatch/enableParallelQuery 同用；后台也拦截这些冲突，跨规则全局冲突仍以节点严格编译为准。
- 在既有 core/custom_test.go 中复现并验证旧/严格路径的显式关闭、TTL 清零、字段继承及脚本编译；既有 core/route_compile_test.go 增补定义在前/后、轮询组、内置标签与缺失定义。核心、DNS 运行时、dispatcher 及节点启动/重载定向测试通过。后台另完成 37 项临时 React/AntD 回填/保存检查；本轮未发布、部署、生产写入或实机解锁验收。


## 2026-11-14：同进程按节点隔离 DNS（源码修复）

- 旧问题：此前 JP/US 虽分别绑定节点，但 DNS config 与缓存按整个进程合并；出站解析接口又不携带入站身份，仅拆面板编号/端口无法提供独立地区线路。旧二进制曾实测两个 SS 入口都识别为日本。
- 新链路：面板 `Common.routes` → `core/route_compile.go::NodeDNSConfigs` 按节点标签独立编译 → `core/route_dns.go` 为每节点创建独立 DNS backend/缓存/全局选项 → `RouteRuntimeLease.Context` 绑定当前入站及运行代 → 带 context 的实际解析接口。正式入站与其 `-stream-unlock-probe` 别名使用同一节点 backend；嵌套转发即使重写入站标签也保留原始入口所属 DNS；未下发 DNS 的其他节点使用自身默认配置。
- 覆盖路径：DNS outbound 的 A/AAAA 截获、默认与自定义 Freedom 出站、outbound targetStrategy、sockopt DomainStrategy、UDP 多目标包和路由 IP 解析均传递连接 context。原 IPv4/IPv6、Force、fallback、私网安全规则语义保留；同一自定义出站对象被多个节点引用时，不把节点 DNS 保存到共享出站字段。
- 热更新：整代原子发布，配置哈希包含节点标签与其 DNS 配置。新连接选新代，旧连接继续其原代；每次 DNS 查询另持租约，连接先关闭也不会提前释放正在查询的 backend。候选创建/脚本失败则关闭候选资源并继续旧代，DNS upstream 的 IsOwnLink 仍检查所有在用运行代，避免 UDP/53 自递归。
- 构建：`go.mod` 的 Xray replace 指向仓库 `third_party/xray-core`。基于此前固定的 `dcfae7c4776b` 源码，只修改 7 个上下文相关文件；来源、许可证和迁移清单见 `third_party/xray-core/SNTP_PATCHES.md`。正常构建/现有发布工作流直接带上修复，不能重新改回未打补丁的远端 replace。
- 验证：修复前真实 DNS outbound 与普通 TCP 同域名双节点回归均复现第二节点误用第一节点结果；修复后通过 DNS 截获/探测别名、TCP、UDP 多目标新域名、真实 AES-128-GCM 双 SS 入站、共享自定义出站 targetStrategy/sockopt、热更新既有流持续传输、并发查询、失败候选回滚、路由解析、单节点兼容及旧查询租约测试。测试 DNS/目标均为本机临时监听；加密 SS 用例仅为测试目标显式允许 loopback，不放宽生产私网规则。
- 检查命令：`go test ./... -count=1 -timeout=120s`（另按发布工作流启用 `GOEXPERIMENT=jsonv2` 跑全量）、`git diff --check`，并逐文件对比固定依赖，确认其余 903 个引入文件未改。当前 Windows 环境 CGO 关闭且无 gcc，未执行 race detector；并发与租约行为已用受控回归覆盖。
- 发布：需要发布新二进制并重启 v2node，配置热加载不能替换旧进程代码；后续 DNS 规则更新仍走运行代热更新，不要求每次重启。既有面板两节点分别绑定 JP/US 的方式可保留，无需为了 DNS 隔离拆成两个服务实例。
- 范围/上线验收：本轮仅本地源码和自动化测试，不部署、不改生产规则/系统 DNS、不重启线上服务。上线后仍需经两条真实入口复测 Netflix/ABEMA 等地区；自动化隔离通过不等于已验证线上会员播放、DRM 或登录后的 AI 调用。系统 DNS 与各节点内部 DNS 继续独立。
