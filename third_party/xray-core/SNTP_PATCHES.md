# SNTP 核心补丁来源与维护

本目录固定自 `github.com/wyx2685/xray-core v0.0.0-20261102040409-dcfae7c4776b`（提交 `dcfae7c4776b`，核心报告版本 `26.10.31`）。根 `go.mod` 通过本地 replace 使用这里的源码，正常 `go test` / `go build` 和现有发布工作流都会包含补丁，不依赖开发机模块缓存或额外构建步骤。

保留上游许可证和运行/生成源码，省略 `*_test.go`、`.github/`、`testing/`、上游 `.gitignore` 和非运行所需的 buf 测试数据。引入时 903 个文件与固定依赖逐字节一致，只有下表 7 个文件包含功能修改。没有升级核心版本或修改协议实现。目录 `.gitattributes` 仅对 8 个未修改的上游文件保留其原始行尾/末尾空白，7 个补丁文件仍执行正常空白检查。

| 文件 | SNTP 修改 |
| --- | --- |
| `features/dns/client.go` | 增加通过连接 context 绑定 DNS 客户端及执行查询的辅助接口，保留原 Client 接口。 |
| `transport/internet/dialer.go` | 新增 LookupForIPContext；系统拨号按连接选择解析器，保留 IPv4/IPv6、Force 和 fallback 策略。 |
| `app/proxyman/outbound/handler.go` | 出站 targetStrategy 解析携带连接 context。 |
| `proxy/freedom/freedom.go` | 安全规则预解析和 UDP 多目标包解析携带同一连接 context；保留原公开 PacketWriter 构造器。 |
| `proxy/dns/dns.go` | DNS 截获的异步查询保留节点/运行代上下文；TimeoutOnly 只解除取消/期限，不丢失 context 值。 |
| `features/routing/session/context.go` | 路由上下文可读取原连接 context。 |
| `features/routing/dns/context.go` | IP 路由解析及多层可解析上下文继续传递同一 context。 |

节点配置/缓存、探测别名、查询租约与候选失败回滚由主仓库 `core/route_compile.go`、`core/route_dns.go`、`core/app/dispatcher/route_runtime.go` 管理。单个共享出站可以处理来自不同节点的流量，不将节点 DNS 写到共享出站对象上。

升级核心时，先对比上述固定版本和 7 个补丁文件，再迁移到新源码；不要只删除本地 replace，否则会重新启用进程共享解析路径。运行主仓库的 DNS 隔离、真实加密 SS 入站、UDP 多目标、路由解析、并发热更新及全量回归。完整操作及验收边界见主仓库 `docs/log-troubleshooting.md` 的 2026-11-14 章节。
