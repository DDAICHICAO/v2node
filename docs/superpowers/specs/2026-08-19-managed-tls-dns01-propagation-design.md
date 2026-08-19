# 托管 TLS DNS-01 传播稳定性设计

## 背景与现场证据

NodeID 304 在 `v2node v5.0.0.69` 上已经进入真实签发流程。现场日志显示：

1. Cloudflare API 成功创建 `_acme-challenge.gcore-jp-03.as9929.uk` TXT 记录；
2. lego 随后通过节点 `/etc/resolv.conf` 中的 `127.0.0.53` 执行传播预检；
3. 预检持续两分钟仍未完成，最后清理 TXT 并向面板报告 `managed_tls_obtain_failed`；
4. 证书目录没有生成文件，失败发生在 Let’s Encrypt 签发完成之前。

这证明租约恢复、Cloudflare 凭据和 TXT 写入路径已经工作，剩余故障边界是节点本地 DNS-01 传播预检。

## 目标

- 托管 TLS 不依赖宿主机的 systemd-resolved stub 完成 DNS 区域发现；
- Cloudflare 写入 TXT 后采用确定性的短等待，再交由 Let’s Encrypt 从公网执行最终验证；
- 面板继续接收稳定的机器错误码，节点日志同时保留可诊断的底层 ACME 错误；
- 不修改节点端口、证书作用域、SNI 迁移、证书落盘和运行时激活流程。

## 方案选择

采用方案 A：

- 为 managed TLS 的 lego DNS-01 challenge 显式配置 `1.1.1.1:53` 与 `8.8.8.8:53`，用于区域和权威 DNS 发现；
- Cloudflare TXT 写入后固定等待 15 秒，并跳过节点本地的 TXT 完整传播预检；
- 15 秒后仍由 Let’s Encrypt 完成真正的公网 DNS-01 校验，节点不会自行判定证书有效；
- `Certificate.Obtain` 失败时记录底层错误，但对面板仍返回 `managed_tls_obtain_failed`。

未采用的方案：

- 只替换递归 DNS：仍保留当前完整权威传播预检，无法消除已观察到的两分钟卡死；
- 只延长 Cloudflare 传播超时：会把失败等待从两分钟放大到五至十分钟，不能处理本地预检链异常。

## 组件与数据流

`managedTLSLegoIssuer.Issue` 保持串行环境变量隔离，`issueManagedTLSWithLego` 的流程调整为：

1. 校验域名、provider 和 DNS 环境变量；
2. 创建或恢复 ACME 账户；
3. 创建 Cloudflare DNS provider；
4. 使用 managed TLS 专用 DNS-01 options 注册 provider；
5. Cloudflare 创建 TXT 后等待 15 秒；
6. Let’s Encrypt 校验 TXT 并签发证书；
7. 成功后沿现有路径返回 fullchain、private key 和 ACME account。

公共递归 DNS 和等待策略只作用于 managed TLS 签发器，不修改旧 `Lego` 证书模式。

## 错误与安全边界

- Cloudflare provider 创建或配置失败仍返回 `managed_tls_dns_provider_failed`；
- Let’s Encrypt、CAA、速率限制或 DNS 验证失败仍返回 `managed_tls_obtain_failed`；
- 底层错误只写节点日志，不进入面板 API 响应，避免改变现有错误码契约；
- 日志不得主动输出 Cloudflare Token、Authorization、密码或 DNS 环境变量值；
- 跳过的是节点本地预检，不是 Let’s Encrypt 的最终验证，因此不会降低证书签发权限边界。

## 测试与验收

- 单元测试先证明 managed TLS DNS-01 配置包含固定公共递归 DNS 和 15 秒跳过预检策略；
- 单元测试证明 Obtain 的底层错误仍映射为 `managed_tls_obtain_failed`；
- 运行 managed TLS 相关 Go 测试、`gofmt`、`go test` 和 `git diff --check`；
- 部署后在 NodeID 304 点击一次“重新签发证书”，预期约 15 秒后进入 ACME 公网验证并生成 v1 证书，不再本地等待两分钟后失败；
- 若 Let’s Encrypt 仍拒绝，节点日志必须显示真实 ACME 原因，便于继续定位 CAA、授权或速率限制。

## 非目标

- 不改 v2board 数据库和后台页面；
- 不自动重启 v2node；
- 不自动触发后台重签 API；
- 不修改现有证书作用域成员关系或域名迁移规则。
