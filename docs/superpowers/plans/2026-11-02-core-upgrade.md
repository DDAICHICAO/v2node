# SNTP 节点端完整核心升级计划

**目标：** 将节点端运行核心对齐作者 2026-11-02 的版本，移除用户确认不再使用的 Mieru，保留 SNTP 自有协议及用户同步、限速、流量审计、DNS/路由热更新和托管证书功能。

**执行：** 当前会话直接完成，不创建子会话或委派。基线为 `dev` 的 `c6fce73`；原有未提交的 `LESSONS_LEARNED.md` 内容必须保留。此次为本地源码升级，已获用户授权进行本地提交；推送、发版和生产替换不包含在当前执行范围。

**方案：** 保留 SNTP 的 dispatcher、连接登记、运行代次和控制器实现，对齐作者固定的 Xray/REALITY fork 并适配接口。依赖按 Go 模块规则解析，不用上游完整源码覆盖本地定制代码，不升级审计队列存储格式。

## 1. 固定编译器和模块

- [x] `go.mod` 的 Go 版本从 `1.26.10` 升到 `1.27.3`，继续保留 `godebug multipathtcp=0`。
- [x] Xray 对齐作者模块标识 `github.com/xtls/xray-core v1.260930.0`，实际替换为 `github.com/wyx2685/xray-core v0.0.0-20261102040409-dcfae7c4776b`。该固定提交的核心版本为 `26.10.31`，不能仅按 require 的名义版本判断实际代码。
- [x] 新增全版本 REALITY 替换：`github.com/xtls/reality => github.com/wyx2685/REALITY v0.0.0-20261102081311-25ea3c0fa255`，保留 X25519-only 兼容。
- [x] 其余原有直接依赖对齐作者 main 的明确版本，保留本地 WebSocket 与 bbolt 依赖；移除 Mieru require，运行 `go mod tidy` 更新模块图和校验和。
- [x] `Dockerfile` builder 使用 `golang:1.27.3-alpine`；release 和 CodeQL 已从 `go.mod` 读取版本，无需再次硬编码。

## 2. 移除 Mieru 节点入口

- [x] 删除 `core/mieru.go`、`core/mieru_test.go`，这是用户明确授权移除的协议实现及专属用例。
- [x] 从 `core/core.go` 移除 Mieru 实例表、初始化和关闭流程。
- [x] 从 `core/node.go`、`core/node_preflight.go` 移除 Mieru 节点创建、删除和预检分支。
- [x] 从 `core/user.go` 移除 Mieru 专属用户增删和流量采集分支，保留 Titan 与 Xray 路径。
- [x] 从 `api/v2board/node.go` 协议允许集合移除 `mieru`；旧面板仍下发该协议时返回既有不支持协议错误，不能降级为其他协议。

## 3. 适配核心接口

- [x] 先运行新依赖下已有核心用例或编译检查，记录真实的不兼容报错，再修改相应调用。
- [x] 已知 `core/app/dispatcher/fakednssniffer.go` 中的 `.AtError()` 在新核心被删除，改为保留原始错误：

```go
errNotInit := errors.New("FakeDNSEngine is not initialized, but such a sniffer is used")
```

- [x] `core/distro/all/all.go` 的旧 `transport/internet/singquic` 注册替换为新 `transport/internet/hysteria` 与 `transport/internet/tuic`，覆盖两个协议所需传输实现。
- [x] `core/user.go` 删除新核心已移除的 Shadowsocks `CipherType_NONE` 分支；`none`/`plain` 必须报不支持，不能静默映射到其他算法。
- [x] 检查 REALITY 客户端版本限制和 X25519 兼容、TCP 用户撤销时底层连接关闭，以及新版 Xray 的直连转发是否绕过审计/限速。
- [x] 通过现有测试和静态调用链确定其余适配点，改动限制在对应生产入口；不修改 SNTP 自有协议 wire format。

## 4. 验证与交付

- [x] `gofmt -l` 检查改动 Go 文件，`git diff --check` 检查差异。
- [x] `go mod verify` 检查固定模块和模块图，检查 Mieru 已不在源码与最终模块图中。
- [x] 使用 `GOEXPERIMENT=jsonv2` 运行现有 `./...` 回归；覆盖用户增删/撤销、审计持久化、真实本地 VLESS TCP、DNS 防回环、路由代次、失败回滚、托管 TLS 和自有协议握手入口。
- [x] 必要时在仓库外保存临时用例，运行时临时加入或使用 Go overlay，完成后移除；不保留新测试文件。
- [x] 用户已明确批准完整本地编译、现有回归和 Linux amd64/arm64 交叉编译；已完成 Windows amd64、Linux amd64/arm64 主程序构建，产物放仓库外。
- [x] 复查所有修改和受影响调用链，追加 `LESSONS_LEARNED.md`，报告实际通过的检查与尚未完成的线上验收。

**上线边界：** 编译完成不代表生产已更新。仍需发布 SNTP 自有产物、替换节点二进制、核对服务环境与实际监听；Linux 内核漏洞另需安装并启动发行版已修补内核。

## 最终适配与验证结果

- 首次校验、依赖解析、物理 tag 重写统一迁移旧 proxySettings；拒绝会意外启用旧版被忽略传输选项的歧义配置。
- 修复 SS2022 AES128/AES256 占位用户密钥长度；修复 AnyTLS 快速撤销及删除后立即重新启用的异步队列竞态。
- 审计/路由代次依赖链路包装的连接禁止原始 socket splice，保持字节统计和旧代次生命周期，吞吐影响待压测。
- 现有全量回归含子测试 367 个通过；仓库外 overlay 追加验证含子测试 27 个通过，覆盖标准协议真实 TCP 转发、TUIC QUIC v5 入站、REALITY standalone X25519 和真实热更新/审计，以及 native-1 真实加密传输/撤销。
- 三种架构构建及二进制模块/MPTCP 默认/ELF 架构核对通过；临时测试与 overlay 在交付前清理，保留结果日志和产物校验值。
- 详细版本、行为差异、所有修改文件和发布边界见 ../../core-upgrade.md；既有 LESSONS_LEARNED.md 字节前缀完整保留，并追加本次记录。
- 漏洞扫描因官方数据库连接 EOF 未完成；Linux 实机、race、UDP/三端完整矩阵和长期压力未验证。本次升级随本计划进行本地提交；未推送、发布或部署。
