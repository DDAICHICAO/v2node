# v2node 日志排查速查

这份文档用于排查 SNTP 节点端连接被拒、用户偶发连不上、设备/IP 限制、`blocked_ips` 等问题。

## 日志在哪里

如果是安装脚本部署的 systemd 服务，直接用管理脚本：

```bash
v2node log
```

等价于：

```bash
journalctl -u v2node.service -e --no-pager -f
```

更常用的命令：

```bash
# 实时看最近 200 行
journalctl -u v2node.service -f -n 200 --no-pager

# 看最近 30 分钟
journalctl -u v2node.service --since "30 min ago" --no-pager

# 看指定时间段
journalctl -u v2node.service --since "2026-05-21 08:00:00" --until "2026-05-21 09:00:00" --no-pager

# 查看服务状态
systemctl status v2node --no-pager -l
```

如果 `/etc/v2node/config.json` 里配置了 `Log.Output`，日志会写到指定文件：

```bash
grep -A5 '"Log"' /etc/v2node/config.json
tail -f /path/to/v2node.log
```

Docker 部署时看容器日志：

```bash
docker ps
docker logs -f --tail 200 <container_name_or_id>
```

## 日志级别

配置文件位置通常是：

```bash
/etc/v2node/config.json
```

相关配置：

```json
{
  "Log": {
    "Level": "warning",
    "Output": "",
    "Access": "none"
  }
}
```

连接拒绝日志是 `warning` 级别，默认 `warning` 就能看到。需要更多路由、嗅探、普通运行细节时，可以临时改成 `info` 或 `debug`，改完重启：

```bash
systemctl restart v2node
```

## 快速查拒绝原因

```bash
journalctl -u v2node.service -f -n 300 --no-pager \
  | grep -E "user_not_found|blocked_ip|device_limit_exceeded|rejected by limiter|SNTP Eclipse user not found"
```

拒绝原因含义：

| reason | 含义 |
| --- | --- |
| `user_not_found` | UUID 不在节点当前用户/限制表里，常见于节点还没拉到新用户表、用户表过旧、用户被删除 |
| `blocked_ip` | 客户端 IP 命中后台下发的 `blocked_ips` |
| `device_limit_exceeded` | 触发设备/IP 数限制 |

设备限制相关字段：

| 字段 | 含义 |
| --- | --- |
| `device_limit` | 后台给该用户设置的设备/IP 上限 |
| `alive_count` | 后端当前认为在线的设备/IP 数 |
| `pending_device_count` | 节点本地当前窗口内还没上报给后端的设备 UUID 数 |
| `cached_device_overlap` | 为避免后端缓存窗口重复计算而抵扣的数量 |
| `effective_device_count` | 最终用于判断是否超限的数量 |
| `device_limit_by_uuid` | 是否启用 UUID 设备限制模式 |

## UUID、UID 和设备 UUID 的区别

日志里容易混的字段有两个：

| 字段 | 含义 |
| --- | --- |
| `uid` | 后台用户 ID，用来表示这个账号本身。流量、在线设备数最终都按这个 ID 归属到用户。 |
| `uuid` / `tag|uuid` | 节点认证用的 UUID，也就是客户端配置里拿来连节点的 UUID。 |

开启新设备 UUID 模式后，后台会把设备记录里的 `设备 UUID` 下发给 v2node，作为该设备的节点认证 UUID。也就是说，后台用户详情页“设备记录”里的“设备 UUID”，就是新节点日志里 `tag|uuid` 后半段对应的值。

兼容迁移期如果开启了 `device_uuid_keep_legacy_user_uuid`，节点用户表里可能同时存在两类 UUID：

- 旧用户 UUID：账号原本的订阅 UUID，老配置可能还在用。
- 设备 UUID：你在后台“设备记录”列表里看到的 UUID，新订阅拉取后生成的设备身份。

所以排查时更稳的组合是：`uid` + 设备 UUID + 客户端 IP + 时间段。SNTP Eclipse 日志会脱敏 UUID，优先用 `uid` 和 `client_ip` 查。

## 查某个用户

普通入站日志里的用户通常是 `tag|uuid`。开启设备 UUID 模式时，这里的 UUID 通常就是后台设备记录里的“设备 UUID”；兼容模式下也可能是旧用户 UUID。

新版本会在设备在线上报时输出 `SNTP online device heartbeat` 日志，可以直接按 UID 或设备 UUID 查：

```bash
# 按后台用户 ID 查在线设备心跳
journalctl -u v2node.service -f -n 300 --no-pager \
  | grep --line-buffered -F "uid=145817"

# 按设备 UUID 查在线设备心跳
journalctl -u v2node.service -f -n 300 --no-pager \
  | grep --line-buffered -F "uuid=2386a7273a3ce22fa775118ce048de58"
```

日志示例：

```text
SNTP online device heartbeat ip=222.128.15.208 tag=... uid=145817 uuid=2386a7273a3ce22fa775118ce048de58
```

新版本也会在用户访问时输出 `SNTP user access` 日志，可以直接按 UID 或设备 UUID 实时查看访问目标：

```bash
# 按后台用户 ID 实时看访问目标
journalctl -u v2node.service -f -n 500 --no-pager \
  | grep --line-buffered -F "SNTP user access uid=145817"

# 按设备 UUID 实时看访问目标
journalctl -u v2node.service -f -n 500 --no-pager \
  | grep --line-buffered -F "uuid=2386a7273a3ce22fa775118ce048de58"
```

访问日志示例：

```text
SNTP user access uid=145817 uuid=2386a7273a3ce22fa775118ce048de58 source_ip=222.128.15.208 target=example.com:443 inbound_tag=... outbound_tag=...
```

```bash
NODE_UUID="设备UUID或旧用户UUID"
journalctl -u v2node.service --since "2 hours ago" --no-pager | grep -F "$NODE_UUID"
```

按 UID 查：

```bash
UID="123"
journalctl -u v2node.service --since "2 hours ago" --no-pager | grep -E "uid=${UID}([^0-9]|$)"
```

按客户端 IP 查：

```bash
CLIENT_IP="1.2.3.4"
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "client_ip=${CLIENT_IP}|source_ip=${CLIENT_IP}"
```

按拒绝原因查某个用户：

```bash
NODE_UUID="设备UUID或旧用户UUID"
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -F "$NODE_UUID" \
  | grep -E "user_not_found|blocked_ip|device_limit_exceeded"
```

SNTP Eclipse 为避免日志暴露完整 UUID，日志里会输出脱敏 UUID，例如 `abcdef...uvwxyz`。排查 SNTP Eclipse 用户时，优先用 `uid`、`client_ip`、时间段和 `reason` 组合查：

```bash
UID="123"
CLIENT_IP="1.2.3.4"
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "SNTP Eclipse|rejected by limiter|user not found" \
  | grep -E "uid=${UID}|client_ip=${CLIENT_IP}"
```

## 按 UUID 查不到日志怎么办

按设备 UUID 查不到日志不一定代表没有连接。常见原因：

- 成功连接默认不会按 UUID 打一条业务日志，只有拒绝、异常、access log 开启后才容易查到。
- SNTP Eclipse 拒绝日志会脱敏 UUID，例如 `2386a7...48de58`，直接 grep 完整 UUID 查不到。
- 当前服务器可能还没部署包含新拒绝日志的 v2node 二进制。
- `/etc/v2node/config.json` 配了 `Log.Output` 时，日志可能写到文件，不在 journal 里。
- 查询时间窗口不对，或者服务刚重启导致你看的时间段没有对应事件。

先确认服务确实有日志：

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager | tail -n 100
```

查拒绝日志不要只按 UUID，先按原因查：

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "reason=|user_not_found|blocked_ip|device_limit_exceeded|rejected by limiter|SNTP Eclipse user not found"
```

SNTP Eclipse 按脱敏 UUID 查。设备 UUID `2386a7273a3ce22fa775118ce048de58` 对应：

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager | grep -F "2386a7...48de58"
```

也可以按 IP 查：

```bash
CLIENT_IP="222.128.15.208"
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "client_ip=${CLIENT_IP}|source_ip=${CLIENT_IP}|${CLIENT_IP}"
```

确认新日志代码是否已经部署到服务器：

```bash
strings /usr/local/v2node/v2node | grep -E "device_limit_exceeded|cached_device_overlap|SNTP user rejected by limiter"
```

如果没有输出，说明服务器上的二进制还不是新版本，需要重新部署或更新后再重启 v2node。

确认是否写到了文件：

```bash
grep -A5 '"Log"' /etc/v2node/config.json
```

## 查 blocked_ips

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager | grep -F "reason=blocked_ip"
```

按 IP 查是否被 blocked：

```bash
CLIENT_IP="1.2.3.4"
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -F "reason=blocked_ip" \
  | grep -E "client_ip=${CLIENT_IP}|source_ip=${CLIENT_IP}"
```

## 查设备/IP 超限

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager | grep -F "reason=device_limit_exceeded"
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

如果 `effective_device_count > device_limit`，才会按设备限制拒绝。

## 查 UUID 不在节点用户表

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager | grep -F "reason=user_not_found"
```

常见原因：

- 节点还没拉到最新用户表。
- 后端刚更新或重启，节点仍使用旧用户表。
- 用户已被删除或订阅已失效。
- 节点和后端通信失败，用户列表同步失败。

同时看用户同步日志：

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "Get user list failed|user deleted|user added|user modified|User list no change"
```

## 保存一份日志给排查

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager > /tmp/v2node-last-2h.log
```

只保存拒绝日志：

```bash
journalctl -u v2node.service --since "2 hours ago" --no-pager \
  | grep -E "user_not_found|blocked_ip|device_limit_exceeded|rejected by limiter|SNTP Eclipse user not found" \
  > /tmp/v2node-rejects.log
```
