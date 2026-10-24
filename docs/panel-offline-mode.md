# v2node 面板失联离线模式

## 行为

- 面板不可达时，v2node 继续使用最后一次成功快照为缓存用户提供服务，不主动踢出已连接用户，也允许缓存中的有效用户继续建立连接。
- 离线服务没有固定停服期限。失联 24 小时和 72 小时只输出一次告警，不删除用户、不清空在线限制、不触发 reload。
- 面板恢复且节点配置、用户和在线限制完成同步后，v2node 自动应用最新数据并刷新快照。
- 首次安装尚未生成成功快照，或快照损坏、版本未知、节点身份/已绑定凭据不匹配时，v2node 不会用空授权启动。
- 面板失联期间产生的封禁、流量耗尽和权限变化要等通讯恢复后才会生效；本地快照携带的到期时间仍会用于清理到期授权。
- HTTP 401/403 属于节点授权被拒绝，不等同于面板失联，启动时不会凭快照绕过拒绝。

## 面板已删除节点

面板 `/api/v2/server/config` 当前用 HTTP 200 和 `{"status":"fail","message":"server is not exist"}` 表示节点已删除。SNTP 节点端将这个明确业务响应与超时、普通 HTTP 404、5xx、鉴权失败分开处理；HTTP 200 的 `token is null` / `token is error` 同样属于授权拒绝。

- 默认开启路由热更新时，协调器停止已删除节点的监听、用户连接和后台任务，将它从更新集合排除，继续为其余有效节点发布规则。不会因为这个删除响应取消整批更新，也不会为此重启其他节点的共享核心。
- 本机 `config.json` 尚有旧 NodeID 时，启动和配置重载会跳过明确已删除的节点；全部节点都已删除时允许没有业务监听。关闭 `EnableRouteHotReload` 时仍使用原配置重载路径，不能承诺连接连续性。
- 状态目录保存与面板地址、NodeID、凭据绑定的 `deleted` 标记，替换该节点的旧配置和用户快照。后续面板失联不能恢复已删除节点；后台任务也不能用旧快照覆盖删除标记。回滚候选同样排除已确认删除的节点。
- 不自动修改 `config.json`。建议维护时清理已删除 NodeID；若在面板恢复同一 NodeID，需要重载配置或重启，在线配置和完整授权获取成功后才能重新启用。
- 普通网络故障仍保留当前运行态和既有重试逻辑，不会被当作删除跳过。删除标记写入或监听关闭失败会报错并重试，应检查状态目录和节点日志。

验收时同时查看 `Skipping deleted panel node`、`Deleted panel node stopped; remaining nodes continue serving` 与 `Route runtime generation published`：已删除节点的端口应关闭，有效节点的新规则应发布。用有效节点的真实代理连接验证已有连接和新连接，不以进程 active 或配置拉取成功代替验收。本地已覆盖模拟面板删除响应、启动/回滚、离线标记和真实 VLESS TCP 连续性；线上节点及其他协议仍需部署后验收。

## 审计写盘与授权快照等待

用户同步已收到面板响应但快照 Sync/ACK 仍慢时，同时检查同盘审计队列。2026-10-24 修复了历史积压释放后每次小事务重写大型 freelist 的问题，保留事件持久化和授权确认顺序；更新、回退与可选离线压缩流程见 [审计队列存储维护](audit-spool-storage.md)。

## 快速启动与配置切换

此前正常重启逐个节点串行拉取配置、完整用户列表、在线 IP 与设备状态，配置重载还会先关闭所有节点再等待接口返回。现在节点准备最多四个并发任务；每个节点取得配置后，用户、在线 IP、设备状态并发准备。

已有最近 24 小时内快照时，在线启动可以通过用户增量追平来减少全量下载，但必须同时满足：

- 快照身份与当前凭据指纹匹配，用户序号大于零，保存时间不在未来。
- 快照标记 `fast_sync_ready` 为真：至少完成过一次可信全量同步，或从已验证快照追平增量。
- 本次节点配置在线获取成功；增量已完整追平，事件序号、动作与用户身份有效。

增量明确要求全量、内容不合法、序号回退或十页内仍未追平时，必须成功获取全量用户后才能启动。不会提前开放业务端口再后台补授权。旧版本快照缺少验证标记时，首次升级仍需全量同步；后续正常重启才走快照加增量。无变化增量的快照检查点最多每分钟一次，用户变化仍随事务落盘。快照必须持久化，否则每次更新都会重新全量下载。

配置重载先准备候选配置、接口数据、证书和协议校验，旧节点在此期间继续服务：

- 共享核心配置、编译后路由相同：仅增删或替换变化节点，未变化节点及其连接保留。重载取得的在线用户撤销仍应用到保留节点。
- 共享配置或路由变化：准备完成后整体切换核心；受影响连接会中断。
- 配置加载、授权准备或协议校验失败：保留旧服务；新监听启动失败：尝试恢复旧节点/核心。恢复本身失败会退出主循环，交由进程管理器按既有策略恢复。
- 替换前再次继承旧节点已同步到的更新序号，避免准备期间出现的用户撤销被旧候选覆盖。回滚同样使用切换前的最新用户状态。
- 文件监控支持同目录原子替换，并采用末次变化后 250ms 防抖。托管 TLS 首次签发仍可能等待证书，未就绪节点沿用原有等待策略。

局部保留监听后，用户撤销必须关闭真实数据连接。TCP 连接登记同时保存该流的关闭入口，防止核心直连优化绕开管道关闭；UDP 共享套接字不会按用户关闭。

## 部署后的验收

程序二进制更新仍需要进程重启，已有连接会中断；本次缩短启动准备过程，不提供跨进程连接迁移。安装脚本、镜像拉取、二进制下载耗时也不在本次优化范围内。

建议先在一台节点机器验证：

1. 首次升级成功全量同步，确认状态目录跨更新保留；再次重启记录从进程启动到真实代理请求成功的耗时。
2. 保持另一节点的真实代理连接，改一个普通节点端口或增加节点，确认共享核心未重启、原连接持续传输。
3. 观察 `node_id`、`stage`、`elapsed_ms` 日志：`config`、`users`、`alive`、`device_alive`、`bootstrap`、`all_nodes`。慢于 5 秒的阶段使用 warning 级别；完整耗时需要 `Log.Level=info`。
4. 查看 `SNTP reload candidate prepared while active nodes kept serving` 与 `SNTP shared core configuration switched`，分别评估准备时间和实际全局切换时间。日志可能包含已有节点标识，分享前先脱敏。

本地已验证真实 VLESS TCP 连续性、授权撤销、局部/全局端口占用回滚、非法配置保留旧核心、原子替换监控及快照失效条件。生产面板延迟、真实规模用户列表、Linux/systemd 和各协议部署后的恢复时间仍需现场验收，不能据本地结果承诺固定几秒恢复。

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
go test ./conf ./common/task ./api/v2board ./node ./cmd ./core ./core/app/dispatcher -count=1
go test ./... -count=1
```
