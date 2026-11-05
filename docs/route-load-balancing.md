# SNTP 出站负载均衡

## 兼容现有配置

原来 `settings.servers` 只有一台的 Shadowsocks 出站无需调整，`tag`、密码、规则匹配和单出口选路保持原样。已有单台 `settings.address/port/method/password` 简写也保持原样。其他协议仍使用原来的单出口解析路径。

只有 Shadowsocks 的 `settings.servers` 包含两项及以上时，SNTP 才会把每项编译为独立出站，并为当前规则建立等权轮询组。JSON 的 `protocol` 可继续使用原来的 `Shadowsocks` 写法。

## 配置示例

在管理端原来的“出站 JSON”中保留同一个 `tag`，向 `servers` 增加成员即可。下列地址和密码均为示例，使用时替换为真实出口参数；每台可用不同端口、加密方式和密码。

```json
{
  "protocol": "Shadowsocks",
  "settings": {
    "servers": [
      {
        "address": "203.0.113.10",
        "port": 666,
        "method": "aes-128-gcm",
        "password": "REPLACE_WITH_A_PASSWORD"
      },
      {
        "address": "203.0.113.20",
        "port": 666,
        "method": "aes-128-gcm",
        "password": "REPLACE_WITH_B_PASSWORD"
      }
    ]
  },
  "tag": "hk-pool"
}
```

域名目标、IP 目标和默认出站三种动作均可使用这种写法。规则的匹配顺序和所属入站不变；两级分流仍须保证第一层先把目标请求送到第二层，第二层再选择成员。

## 分配与更新

- 新连接在成员间轮询；多入站引用同名、同配置的组时共享该组。连接建立后固定使用选中的出口，单条下载或播放连接不能叠加多台带宽，也不保证各台实际流量字节完全相等。
- 仅提供等权轮询；没有自动探活、故障剔除、失败重试、权重或会话粘滞。某台停机但配置格式有效时仍可能被选中。需要稳定出口 IP 的业务应先验证跨连接切换的影响。
- 开启路由热更新时，成员变化走既有候选校验与代次发布流程；已有连接保留旧出站，新连接按新配置分配。旧代次的选择器只包含该代次的物理出站标签，不扩选到新成员。
- 任一成员格式/参数无效会拒绝整组，不会仅启用部分成员。严格热更新失败保留旧代次；首次启动仍遵循原有兼容降级策略，检查配置健康状态，不能把服务 active 当成整组有效。
- 更新只处理 JSON 的解析与选路，不验证成员公网可达性或目标业务解锁。
- 整机运行时和离线节点快照保留原始多成员 JSON。升级后的程序可重新编译恢复；回退旧二进制前先把多成员配置改回单台并确认应用，否则旧程序无法正确处理多成员快照。

不要同时设置 `settings.address` 和多项 `servers`，否则新核心的单地址优先语义会造成歧义，SNTP 会明确拒绝。

组内成员会继承出站的传输、mux 和既有单出口 `proxySettings`/`sockopt.dialerProxy` 配置。组本身是路由目标，不是 socket 代理；本轮不支持其他出站通过 `dialerProxy` 或 `proxySettings.tag` 引用整个组，遇到这种引用会报错并保留旧运行态。原来单出口之间的依赖继续支持。

## 验证与排查

```powershell
go test ./core ./core/app/dispatcher ./node -run 'Test(CompileRouteRuntime|GetCustomConfig|ApplyRouteRuntime|StartKeepsCompatible|RouteRuntime|RouteOutbound|RouteHotReload|RouteUpdateCoordinator|Controller.*Route)' -count=1 -timeout=90s
```

现有测试文件已覆盖单台旧格式与简写的出站 protobuf/物理标签保持不变、多成员编译与共享、配置冲突及脱敏错误、三类路由动作、真实本机 AES-128-GCM 转发轮询、热更新后旧连接持续收发、成员更换与旧代次隔离、无效候选回滚、多台切回单台、快照保存恢复。

本机转发测试使用临时 loopback 监听和虚拟凭据，不连接生产出口。生产发布后还需在 Linux 上检查新旧连接、各成员可达性、真实业务、UDP、并发吞吐及长期稳定性。未发布的新代码不会自动改变生产节点能力。

代码入口：`core/route_balance.go` 解析多成员与构造轮询组；`core/route_compile.go` 处理版本标签、依赖、规则及配置 hash；`core/route_runtime.go` 校验当前代次的组与成员；`node/route_runtime_state.go` 保持原有原子快照格式。
