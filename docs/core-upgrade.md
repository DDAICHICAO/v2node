# SNTP 节点核心升级

本入口记录 2026-11-02 的完整核心升级及后续复核方法。安装命令仍见仓库 README；离线恢复和审计队列的日常运维分别见 `panel-offline-mode.md`、`audit-spool-storage.md`。本次只在本地修改和验证，没有发布或替换生产节点。

## 版本依据

作者当前开发分支为 `main`。本次对齐 `dde9f237fad986dabe662db18025fb9ae349b5bf`，其前一提交 `5935d2e` 更新工具链、Xray/REALITY fork 并默认关闭 MPTCP；最后一个提交整理依赖。不要把本地分支名 `dev` 或发布工作流的 `master` 与作者分支混淆。

| 项目 | 升级前 | 本次固定值 |
| --- | --- | --- |
| Go | 1.26.10 | 1.27.3，主模块继续设置 `godebug multipathtcp=0` |
| Xray 模块标识 | `github.com/xtls/xray-core v1.260425.0` | `github.com/xtls/xray-core v1.260930.0` |
| 实际 Xray 源码 | wyx2685 fork `6f1e299a2bce` | `github.com/wyx2685/xray-core v0.0.0-20261102040409-dcfae7c4776b`，运行版本 **26.10.31** |
| REALITY | 原替换版本 | `github.com/wyx2685/REALITY v0.0.0-20261102081311-25ea3c0fa255`，全版本替换，保留 standalone X25519 兼容 |
| 构建 | Go 1.26.10 镜像 | Docker builder 改为 `golang:1.27.3-alpine`；CI 已通过 `go-version-file: go.mod` 读取版本 |
| 审计存储 | `go.etcd.io/bbolt v1.3.2` | 保留该版本和既有存储格式 |

`go.mod` 的 require 版本只是模块标识，实际源码以 replace 和二进制的 `go version -m` 为准。其余原有直接依赖对齐作者版本，经 `go mod tidy` 解析间接依赖并更新 `go.sum`。

参考：[作者变更 PR](https://github.com/wyx2685/v2node/pull/52)、[作者 v0.4.7](https://github.com/wyx2685/v2node/releases/tag/v0.4.7)、[Go Security Team 公告](https://groups.google.com/g/golang-announce/c/PCoQcdxoScw/m/aD-MgXEvAAAJ)。作者的发布号不能代替 SNTP 自有产物的发布号。

## 行为变化与兼容边界

| 范围 | 升级前问题 | 修改与结果 | 对应文件 |
| --- | --- | --- | --- |
| 工具链与依赖 | 原工具链和核心版本落后于作者安全更新 | 固定 Go 1.27.3、两个 fork 和作者依赖版本；保留 MPTCP 默认关闭与审计库版本 | `go.mod`、`go.sum`、`Dockerfile` |
| Mieru | 用户不再需要该协议，仍携带监听、用户和依赖路径 | 删除实现及专属用例，移除 API 协议入口、运行状态和限流例外；旧面板下发 `mieru` 会明确返回不支持 | `core/mieru.go`、`core/mieru_test.go`（删除），`core/core.go`、`core/node.go`、`core/node_preflight.go`、`core/user.go`、`api/v2board/node.go`、`limiter/limiter.go`、`go.mod`、`go.sum` |
| 传输与错误接口 | 新核心移除 `singquic` 和 `AtError()` | 显式注册 Hysteria、TUIC 新传输；保留 FakeDNS 原始错误 | `core/distro/all/all.go`、`core/app/dispatcher/fakednssniffer.go` |
| SS/SS2022 | `none/plain` 的枚举已删除；SS2022 占位用户原密钥长度不合法 | 拒绝明文算法；占位用户使用 AES128 的 16 字节或 AES256 的 32 字节随机密钥，真实用户密钥不变 | `core/inbound.go`、`core/user.go` |
| 链式路由 | 新核心删除 `proxySettings`，直接 Build 失败 | 兼容迁移到 `streamSettings.sockopt.dialerProxy`，继续验证依赖缺失、循环、冲突和逻辑 tag 到版本 tag 的重写 | `core/route_compile.go`、`core/route_compile_test.go` |
| AnyTLS 用户同步 | 用户增删按 200ms 合并；刚添加用户在快照不可见，待删除用户仍可见，快速撤销或重新启用会遗漏更新 | 增删操作均提交到 AnyTLS 待处理队列以覆盖相反操作，修复快速撤销和重新启用；保留批量同步方式 | `core/user.go` |
| 流量审计和路由代次 | 原始 socket splice 能绕过链路包装，漏记字节或提前释放路由资源 | 需要审计或代次租约的连接禁用该复制路径，沿受跟踪链路转发；可能降低部分直连吞吐，尚未压测 | `core/app/dispatcher/default.go` |
| 维护入口 | README 缺少 SNTP 核心版本、升级行为和验证边界 | 增加本升级入口和计划完成记录，在既有排障记录后追加本次经验，便于发布前复核 | `README.md`、`docs/core-upgrade.md`、`docs/superpowers/plans/2026-11-02-core-upgrade.md`、`LESSONS_LEARNED.md` |

链式路由自动迁移支持普通 TCP/TLS 形式，以及原本已设置 `transportLayer:true` 的形式。旧 `transportLayer:false` 会忽略部分传输配置；若携带非 TCP 传输、REALITY、TCP header、finalmask、地址覆盖或 socket 选项，本次会报 `legacy outbound proxy transport settings require explicit dialerProxy migration`，避免这些参数升级后意外生效。应明确迁移到现代字段并核查预期传输。候选热更新失败保留旧代次；兼容启动仍沿既有方式跳过无效路由并标记降级，必须检查健康原因。

新核心 freedom 的默认最终规则会阻止 VLESS、VMess、Trojan、Hysteria、Shadowsocks 等标准入口访问私有地址。访问确有需要的内网服务时，在对应最终 freedom 出站使用 `settings.finalRules` 精确允许目标网络、IP 和端口。SNTP 自有协议、AnyTLS、TUIC 不在该上游默认列表中，不能把此规则描述为全协议统一内网封禁。本次验证只在临时配置放行本地 echo 端口，没有放宽生产默认值。

SS2022 本次验证 AES128/AES256 多用户；新核心不提供 Chacha20 的多用户服务端。TUIC 节点入站已实测；核心的 TUIC 出站在升级前后都未完成，不应配置成链式代理客户端。

SNTP 自有协议的 wire format、用户同步接口、用户/设备流量格式、访问审计队列、离线快照、限速/设备限制、证书托管及路由代次实现保留。核心依赖升级后重新执行相应验证，不以接口存在代替功能验证。

## 本地验证记录

2026-11-02，在 Windows 使用 Go 1.27.3、`GOEXPERIMENT=jsonv2`、`CGO_ENABLED=0` 完成：

- `go test -json -p 8 -mod=readonly ./... -count=1 -timeout 90s`：259 个顶层用例，连子测试共 367 个通过；9 个测试包通过，其余 14 个包没有测试文件。
- 仓库外 Go overlay 验证：VMess、VLESS、Trojan、经典 SS、SS2022 AES128/AES256、Hysteria2、TUIC、AnyTLS 的真实监听和用户增删；标准协议真实本地 TCP 代理转发，其中 TUIC 使用 QUIC v5 认证/流中继客户端。
- REALITY：临时 TLS1.3 目标、standalone X25519 客户端完成握手和真实 VLESS 转发；链式出站完成旧字段迁移，切换路由后旧连接继续收发，新连接使用新代次，撤销用户关闭真实连接，旧出站回收；审计事件经临时 bbolt 落盘后送达本地 HTTP 接收端。
- SNTP native-1：真实加密握手、本地 TCP 转发、上下行计数和用户撤销断连；现有自有协议硬化用例也包含在全量回归中。
- 现有回归另覆盖用户增量/离线恢复、审计队列恢复、限流/设备限制、DNS 防回环、路由切换与失败回滚、托管证书激活。
- 完整主程序构建：Windows amd64、Linux amd64、Linux arm64；Linux ELF 的架构编号分别为 62 和 183。检查构建信息中的工具链、两个 fork、`DefaultGODEBUG=multipathtcp=0`、`CGO_ENABLED=0` 和 `GOEXPERIMENT=jsonv2`，并记录 SHA256。
- `go mod verify`、格式检查和 `git diff --check`，以及 Mieru 源码/模块图残留检查。

临时验证代码通过仓库外 overlay 加载，完成后清理，不新增仓库测试文件。现有 `route_hot_reload_integration_test.go` 的流量探针直接连接 echo 服务，因此上述 REALITY 真实代理回归单独验证了经过代理的连续传输。

本地旧 govulncheck 使用 Go 1.26 编译，无法分析 Go 1.27 语法；已用 1.27.3 在临时目录重编同版扫描器。随后因官方漏洞库连接 EOF 未完成扫描，不能据此报告“无漏洞”。

尚未验证 Linux 实机监听、Linux race、全协议 UDP/真实三端客户端矩阵、吞吐和长期压力；本地成功不代表生产已上线或安全风险归零。

## 复现构建与发布核对

在仓库根目录，使用 Go 1.27.3 或与固定模块兼容的新工具链：

```sh
GOEXPERIMENT=jsonv2 go mod verify
GOEXPERIMENT=jsonv2 CGO_ENABLED=0 go test -p 8 -mod=readonly ./... -count=1 -timeout 90s
GOEXPERIMENT=jsonv2 CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -o /tmp/v2node-linux-amd64 .
GOEXPERIMENT=jsonv2 CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=readonly -trimpath -o /tmp/v2node-linux-arm64 .
go version -m /tmp/v2node-linux-amd64
go version -m /tmp/v2node-linux-arm64
```

发布需要使用本仓库的自有产物。安装脚本和节点自更新读取 `DDAICHICAO/v2node` 的 release，Linux 包名为 `v2node-linux-64.zip` 和 `v2node-linux-arm64-v8a.zip`；本地分支 `dev` 不会触发只监听 `master` 的 push 发布流程。不要仅拉取上游源码或执行安装脚本，就认为当前工作区已发布。

逐节点替换前保留旧二进制和配置，持久化离线状态与审计队列目录。进程更新仍需重启，已有连接会中断；进程内 DNS/路由热更新的连接保留能力不代表跨进程连接迁移。部署后核查实际运行文件、版本和模块、节点用户同步、真实代理请求、流量/审计上报及 DNS/路由切换。

默认关闭 MPTCP 是应用侧缓解。运行环境的 `GODEBUG` 或显式 socket 配置可能覆盖默认值；Linux 内核漏洞仍需使用发行版的修补内核并启动后复核。容器使用宿主内核，更新本程序不能代替内核修补。本次没有连接生产、执行 SQL、修改服务或重启节点。
