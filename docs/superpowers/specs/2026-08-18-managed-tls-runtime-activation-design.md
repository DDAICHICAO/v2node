# 托管 TLS 证书运行时激活设计

## 背景

证书作用域域名迁移会先签发同时包含旧域名和新域名的双 SAN 证书，再由各个 v2node 实例安装并上报迁移准备状态。当前实现完成了证书落盘和 `current` 版本切换，但运行中的 Xray 入站仍可能继续使用旧证书。

线上实例已经验证出具体故障链：

1. v2board 下发双 SAN 证书 v2。
2. v2node 将 v2 原子安装到证书目录并上报 `migration_prepared=true`。
3. Xray 的证书文件轮询读到 v2，但 OCSP 处理失败后提前返回，没有把新证书写入运行时。
4. v2board 的入口探测发现新 SNI 与实际证书不匹配，因此正确阻止最终切换。

首轮修复上线后又验证出第二条中断链：

1. 双 SAN v2 已在运行时生效，v2board 的实例门禁和入口探测均通过。
2. v2board 原子切换节点的托管 SNI 后，v2node 的 `nodeInfoMonitor` 收到新节点配置。
3. `nodeInfoMonitor` 把仅 `server_name` / `server_names` 变化当成普通配置变化，写入全局 `ReloadCh`。
4. 主循环因此关闭同机全部控制器和 Xray Core，再顺序重新拉取并启动所有 NodeID；线上观测到端口约 22 秒不可用，尽管 systemd PID 和 `NRestarts` 都没有变化。

本设计同时修复“落盘完成不等于入口已激活”和“托管 SNI 切换误触发全量重载”两类状态错误。

## 目标

- 新证书安装后，仅重建对应 NodeID 的运行时入站，不重启 v2node 进程或整个 Xray Core。
- 已建立连接不由 v2node 主动批量关闭；新连接仅可能遇到入站替换期间的毫秒级重试窗口。
- 只有实际运行的入站已加载目标证书版本后，才上报 `migration_prepared=true`。
- NodeID、端口和实例 ID 都来自当前控制器配置，不硬编码线上节点 `302`。
- 激活失败时保持迁移门禁关闭并自动重试，避免 v2board 误判为可以切换。
- v2board 切换托管 SNI 时，仅原地更新该控制器和证书管理器的权威域名，不写入全局 `ReloadCh`。

## 非目标

- 不修改证书签发、DNS Provider、DNS Env、ACME 账户或证书作用域的数据模型。
- 不修改节点端口、用户计费、流量上报、设备限制或普通用户同步语义。
- 不把任意节点配置变化都改造成热更新；端口、协议、传输、权限和其他运行时字段变化仍沿用现有全量重载。
- 不自动执行最终域名切换，也不缩短旧域名的 24～48 小时回滚窗口。
- 不通过缩短 `OcspStapling` 间隔制造高频 OCSP 请求。
- 不修改或维护外部 Xray fork。

## 方案

### 1. 区分“已安装版本”和“已激活版本”

`managedTLSManager` 继续负责证书获取、校验和原子安装，同时记录两个不同事实：

- installed version：本地 `current` 指向的证书版本；
- activated version：对应运行时入站已经成功加载的证书版本。

现有一次性的 `readyNotified` 改为按证书版本通知。相同版本只需成功激活一次；新版本安装后会再次触发控制器回调。回调失败不会推进 activated version，后续协调轮询会继续重试。

迁移准备状态必须同时满足：

- 本地证书状态为 ready；
- 证书包含当前迁移要求的双域名；
- activated version 等于 installed version。

### 2. 控制器执行单节点入站替换

控制器收到新版本激活通知后：

1. 若该 NodeID 的运行时尚未启动，沿用现有首次启动流程，并在启动成功后记录 activated version。
2. 若运行时已经启动且版本未变化，直接确认当前版本，避免重复替换。
3. 若运行时已经启动且版本变化，调用 Core 的单入站替换能力。

替换前先根据当前 `NodeInfo` 和用户快照构建候选入站并预装用户。只有候选配置和用户都准备成功后，才关闭旧监听并启动候选入站。这样不会重启进程、其他 NodeID 或任务调度器。

替换操作使用控制器级互斥，并与用户状态提交串行化，避免证书激活与增量用户同步同时修改同一个入站。

### 3. 失败与恢复

- 候选配置构建或用户预装失败：旧入站保持不动，activated version 不变。
- 旧入站移除后候选启动失败：使用替换前捕获的旧入站配置和用户快照立即恢复旧入站；激活状态设为 degraded，迁移准备状态保持 false。
- 恢复也失败：记录明确的 `managed_tls_runtime_activation_failed` 错误并保持门禁关闭，交由下一轮重试；不伪造 ready 状态。
- 控制器关闭期间不再开始新的激活；已开始的替换完成或失败后再结束管理协程。

日志只记录 NodeID、tag、scope ID、installed version、activated version 和错误类型，不记录证书私钥、面板 token 或完整用户凭据。

### 4. 托管 SNI 切换原地应用

`nodeInfoMonitor` 收到新 `NodeInfo` 后，先判断变化是否严格限定在托管域名字段：

- 当前与新配置都必须是同一 NodeID、同一 tag、同一协议、同一端口、同一证书作用域的 managed TLS 节点；
- 忽略 `TlsSettings.ServerName`、`TlsSettings.ServerNames` 和派生的 `CertInfo.CertDomain` 后，其余配置必须完全一致；
- 新主域名必须合法，且当前本地证书必须真实包含该域名 SAN。

满足条件时按以下顺序处理：

1. 先把新节点配置写入离线快照；保存失败则保持当前运行时并重试。
2. 以并发安全方式把 `managedTLSManager` 的权威域名改为新域名，并用当前证书重新验证 scope、SAN 和有效期。
3. 原子更新控制器持有的 `NodeInfo`，但不替换入站、不关闭任务、不发送 `ReloadCh`。

TLS 服务端入站的运行时构造只依赖证书/私钥路径和 `RejectUnknownSNI`，不依赖托管配置里的 `server_name`，因此双 SAN 证书已激活后不需要为了 SNI 字段变化重建监听器。证书管理器更新权威域名后，观察期结束时的单域名续签仍会按新域名获取、校验和激活。

如果除了域名字段还有其他运行时配置变化，则不进入该专用路径，继续使用原有全量重载。若变化看似仅域名但当前证书不包含新 SAN，则返回明确错误并保持现有运行时，不用一次有风险的全量重载掩盖证书门禁异常。

## 数据流

```text
v2board certificate vN
        |
        v
managedTLSManager validates and installs vN
        |
        v
Controller activates vN for this NodeID
        |
        +-- runtime absent --> normal first start
        |
        +-- runtime on vN --> no-op confirmation
        |
        `-- runtime on older version --> replace only this inbound
                                      |
                                      +-- success --> activated=vN
                                      `-- failure --> restore old inbound, retry later
        |
        v
runtime status reports migration_prepared only when installed == activated
        |
        v
v2board ingress probe remains the final cutover gate

v2board switches managed SNI after the gate passes
        |
        v
nodeInfoMonitor verifies this is a domain-only managed TLS change
        |
        +-- current certificate contains target SAN
        |       |
        |       `-- persist snapshot -> update manager domain and controller info
        |                              -> no ReloadCh, listener remains active
        |
        `-- any other config changed -> preserve existing full reload behavior
```

## 测试设计

按测试驱动顺序覆盖：

1. 相同证书版本只成功通知一次，新版本会再次通知。
2. 激活回调失败时不推进 activated version，并在后续轮询重试。
3. installed version 与 activated version 不一致时，迁移准备状态为 false。
4. 候选入站准备失败时旧入站不被移除。
5. 单入站替换成功时，候选入站包含当前完整用户快照，且不触发全局 reload channel。
6. 候选启动失败时恢复旧入站，并返回可观察错误。
7. 运行时尚未启动的托管证书节点仍能沿用首次 ready 后启动的现有行为。
8. 非托管 TLS 节点和其他 NodeID 的入站不受影响。
9. 仅 managed TLS 的 `server_name` / `server_names` 变化会原地应用且不写入 `ReloadCh`。
10. 端口、协议、证书作用域或其他字段同时变化时仍走原有全量重载。
11. 当前证书不包含目标 SAN 时保持原运行时、返回错误并等待重试。
12. 原地切换管理域名后，后续证书协调使用新域名，并且并发访问不产生数据竞态。

实现后执行目标 Go 测试、相关 node/core 包测试、`go test ./...`（使用仓库当前要求的 `GOEXPERIMENT=jsonv2`）以及 `git diff --check`。不启动生产服务或长时间运行的 watcher。

## 上线与当前迁移

该修复发布到当前实例需要替换 v2node 二进制并发生一次进程重启，因此本次部署应选低流量窗口。重启后进程会直接从已安装的 v2 双 SAN 证书启动，随后重新执行入口探测。此后再发生托管证书续期或域名迁移时，由单入站激活流程完成，不再要求重启整个 v2node。

最终切换仍由 v2board 的动态实例门禁和真实入口 TLS 探测决定；旧域名继续保留 24～48 小时用于回滚。切换成功后的验收还必须确认 v2node PID、`NRestarts` 和目标端口连续稳定，并检查日志中没有出现全量 Xray 重新启动。
