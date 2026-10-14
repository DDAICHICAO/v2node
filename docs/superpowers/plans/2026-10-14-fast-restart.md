# SNTP 节点快速重启实施计划

当前根会话执行；沿用用户已确认的设计，不委派。Go 1.26、现有面板接口与核心。

- [x] 临时回归先复现 bootstrap 强制全量/串行在线状态、非法配置破坏旧核心、文件监控延迟。
- [x] node/bootstrap.go 复用快照并追平增量，过滤到期用户；用户/在线状态并发，保留失败兜底。node/node.go 有界并发初始化。
- [x] node/reconcile.go 对比核心配置/路由与本地身份，局部增删、替换、回滚；core/node.go 与 core/user.go 保护自研运行时表，校验候选节点。
- [x] cmd/server.go 在关闭旧核心前准备配置，错误保留旧服务，共享变化整体切换与回滚。
- [x] conf/watch.go 支持原子替换和短尾部防抖；新增阶段耗时。
- [x] 运行 GOEXPERIMENT=jsonv2 go test ./node ./cmd ./conf ./core ./api/v2board ./common/task ./core/app/dispatcher；覆盖真实 held TCP、单节点修改及新增、错误回滚、增量失效、授权删除/到期。
- [x] 更新离线模式文档与经验记录、移走临时测试、检查差异；仅暂存本次文件及本次经验记录增量，中文提交。

追加验证：保留节点的用户撤销必须真实断开代理流；共享配置变更完整切换及回滚后新代理可用；晚到同步不得覆盖为旧序号；旧版快照验证标记、授权 403 与未激活 TLS 控制器也已覆盖。本地 Windows 缺少 CGO C 编译器，未运行 race 检查；Linux 与生产恢复秒数由部署验收补充。
