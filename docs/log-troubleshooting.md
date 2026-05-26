# v2node 日志排查速查

用于排查 SNTP 节点端用户访问、在线设备、连接拒绝、设备/IP 限制和 `blocked_ips`。

文档里的 IP 示例已脱敏成 `x`。线上日志会输出完整 IP，方便定位真实客户端和目标。

## 默认日志策略

新装生成配置时，`/etc/v2node/config.json` 的 `Log` 段默认使用安静配置：

```json
{
  "Log": {
    "Level": "warning",
    "Output": "",
    "Access": "none"
  }
}
```

这套配置的含义：

| 配置 | 含义 |
| --- | --- |
| `Level: warning` | 只输出警告、错误和 SNTP 业务访问审计，避免 Xray 底层 info 刷屏 |
| `Output: ""` | 写到 stdout，由 systemd journal 收集 |
| `Access: "none"` | 关闭 Xray 原生 access log，避免 `email: tag|uuid` 这类底层日志刷屏 |

`SNTP user access ...` 是 v2node 自己的业务访问审计，默认开启，并且不需要把 `Level` 调成 `info`。如果临时不想输出这类访问审计，可以手动加：

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

默认只写本机 journal 或 `Log.Output` 文件，不会把访问明细发到远端。需要把 `SNTP user access ...` 同步写入独立日志库时，可以在 `/etc/v2node/config.json` 顶层增加 `AccessAudit`：

```json
{
  "AccessAudit": {
    "Enabled": true,
    "Endpoint": "https://logs.sntp.uk/api/v1/access-events",
    "Token": "替换为 ingest-api 的 INGEST_TOKEN",
    "BatchSize": 1000,
    "MaxQueueSize": 10000,
    "FlushInterval": "1s",
    "Timeout": "5s"
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
| `MaxQueueSize` | 本地内存队列上限；队列满时丢弃新日志，不阻塞用户连接 |
| `FlushInterval` | 定时刷新间隔 |
| `Timeout` | 单次 HTTP 上报超时时间 |

远端上报是异步队列：日志服务不可用时会输出 `SNTP access audit report failed` 警告，但不会阻塞代理连接。`SNTPAccess: false` 只关闭本地 `SNTP user access ...` 输出；如果 `AccessAudit.Enabled` 仍为 `true`，远端审计会继续上报。

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
