# v2node 面板失联离线模式

## 行为

- 面板不可达时，v2node 继续使用最后一次成功快照为缓存用户提供服务，不主动踢出已连接用户，也允许缓存中的有效用户继续建立连接。
- 离线服务没有固定停服期限。失联 24 小时和 72 小时只输出一次告警，不删除用户、不清空在线限制、不触发 reload。
- 面板恢复且节点配置、用户和在线限制完成同步后，v2node 自动应用最新数据并刷新快照。
- 首次安装尚未生成成功快照，或快照损坏、版本未知、节点身份不匹配时，v2node 不会用空授权启动。
- 面板失联期间产生的封禁、流量耗尽和权限变化要等通讯恢复后才会生效。

## 状态目录

默认目录：`/etc/v2node/offline-state`

可在 `config.json` 顶层覆盖目录：

```json
{
  "StatePath": "/var/lib/v2node/offline-state"
}
```

检查默认目录和文件权限：

```bash
sudo ls -lah /etc/v2node/offline-state
sudo stat -c '%a %U:%G %n' /etc/v2node/offline-state/*.json
```

状态目录应为 `700`，快照文件应为 `600`。不要把快照内容粘贴到工单或聊天，因为文件可能包含节点私密配置。

每个快照按规范化后的 `ApiHost + NodeID` 独立保存。临时文件完成写入和同步后才原子替换旧文件，因此失败写入不会主动删除最后可用快照。

## Docker

容器必须持久化整个状态目录。只挂载单个 `config.json`，容器重建后不会保留默认目录中的快照。

沿用默认 `StatePath` 时，可以持久化整个 v2node 配置目录：

```yaml
volumes:
  - ./v2node-config:/etc/v2node
```

如果把 `StatePath` 配置为 `/var/lib/v2node/offline-state`，则单独挂载对应父目录：

```yaml
volumes:
  - ./v2node-state:/var/lib/v2node
```

## 故障检查

```bash
systemctl status v2node --no-pager
journalctl -u v2node --since '30 min ago' --no-pager | grep -Ei 'offline|snapshot|panel|recovered|keeping current runtime'
```

重点日志含义：

- `Panel unavailable; keeping last known runtime state`：已进入离线服务，当前运行态继续保留。
- `Panel has been unavailable for 24 hours` / `72 hours`：仅告警，节点不会因此停服。
- `Panel communication recovered`：同步和上报失败组件均已恢复。
- `Offline snapshot is invalid; trying panel state`：快照校验失败，程序仍会尝试从面板在线启动。
- `Persist ... offline snapshot failed`：当前数据面继续运行，但新的最后成功状态没有落盘，应检查目录权限和磁盘状态。

损坏快照会被拒绝加载，但不会自动删除。先保留文件排查；面板恢复并成功生成新快照后，再人工清理不再使用的旧文件。

## 验证命令

本仓库使用 Go `jsonv2` 实验特性，本地验证需要与 Dockerfile 一致：

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./conf ./common/task ./api/v2board ./node -count=1
go test ./... -count=1
```
