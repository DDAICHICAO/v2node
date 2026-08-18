# 托管 TLS 证书运行时激活设计

## 背景

证书作用域域名迁移会先签发同时包含旧域名和新域名的双 SAN 证书，再由各个 v2node 实例安装并上报迁移准备状态。当前实现完成了证书落盘和 `current` 版本切换，但运行中的 Xray 入站仍可能继续使用旧证书。

线上实例已经验证出具体故障链：

1. v2board 下发双 SAN 证书 v2。
2. v2node 将 v2 原子安装到证书目录并上报 `migration_prepared=true`。
3. Xray 的证书文件轮询读到 v2，但 OCSP 处理失败后提前返回，没有把新证书写入运行时。
4. v2board 的入口探测发现新 SNI 与实际证书不匹配，因此正确阻止最终切换。

本设计修复“落盘完成不等于入口已激活”的状态错误，并让托管证书更新不再依赖 Xray 的定时文件轮询。

## 目标

- 新证书安装后，仅重建对应 NodeID 的运行时入站，不重启 v2node 进程或整个 Xray Core。
- 已建立连接不由 v2node 主动批量关闭；新连接仅可能遇到入站替换期间的毫秒级重试窗口。
- 只有实际运行的入站已加载目标证书版本后，才上报 `migration_prepared=true`。
- NodeID、端口和实例 ID 都来自当前控制器配置，不硬编码线上节点 `302`。
- 激活失败时保持迁移门禁关闭并自动重试，避免 v2board 误判为可以切换。

## 非目标

- 不修改证书签发、DNS Provider、DNS Env、ACME 账户或证书作用域的数据模型。
- 不修改节点端口、用户计费、流量上报、设备限制或普通用户同步语义。
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

实现后执行目标 Go 测试、相关 node/core 包测试、`go test ./...`（使用仓库当前要求的 `GOEXPERIMENT=jsonv2`）以及 `git diff --check`。不启动生产服务或长时间运行的 watcher。

## 上线与当前迁移

该修复发布到当前实例需要替换 v2node 二进制并发生一次进程重启，因此本次部署应选低流量窗口。重启后进程会直接从已安装的 v2 双 SAN 证书启动，随后重新执行入口探测。此后再发生托管证书续期或域名迁移时，由单入站激活流程完成，不再要求重启整个 v2node。

最终切换仍由 v2board 的动态实例门禁和真实入口 TLS 探测决定；旧域名继续保留 24～48 小时用于回滚。
