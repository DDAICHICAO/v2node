# SNTP 节点端

SNTP 的节点运行服务，连接面板的配置、用户同步、流量上报与访问审计，使用定制 Xray/REALITY 核心和 SNTP 自有协议。

- [核心升级与兼容说明](docs/core-upgrade.md)：固定版本、协议范围、本地验证及发布核对。
- [面板离线与恢复](docs/panel-offline-mode.md)
- [审计持久化存储](docs/audit-spool-storage.md)
- [日志排查](docs/log-troubleshooting.md)
- [出站负载均衡](docs/route-load-balancing.md)：保留单出口配置，多台 Shadowsocks 出口按连接轮询。

## 软件安装

### 一键安装

```
wget -N https://raw.githubusercontent.com/DDAICHICAO/v2node/dev/script/install.sh && bash install.sh
```
