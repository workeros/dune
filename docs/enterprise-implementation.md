# 企业实现状态

当前 Dune 已实现以下边界：

- `pkg/identity.Service` 与可选 `PasswordService`；本地密码实现保留，企业会话由
  宿主验证并直接使用企业 UID；
- `pkg/managed.Service` 高层接口及既有 Managed Web API/前端；Dune 内部 lifecycle
  编排、worker 和十张状态表已删除；服务通过 `BindRunnerAccess` 获取窄接入能力，
  Dune 自己执行 pause/resume/destroy 的持久访问门控；
- SQLite 单实例与 PostgreSQL `dune_routes` 多 Gateway owner/epoch 接管；
- 进程内浏览器票据与 peer nonce；peer owner 独立重验 Session、Runner 和策略；
- Runner disabled 状态、当前实例主动断开和周期有效性检查；
- 启动时校验精确 Dune 表与列集合，旧或混杂的 Dune schema 明确拒绝；
- API-only 后端与本地静态资源组合两种启动方式。

SandDance 相邻模块提供企业 Session adapter，并保留 TAE provider 作为
SandDance 自己实现 Managed 服务时使用的底层能力。Dune 不再接受 provider、
renewal policy 或 worker 配置。

当前不支持数据库备份恢复、历史回滚、恢复协调、旧 schema 升级，以及故障期间
已建立 WebSocket 的透明迁移。接管后的用户需重新连接；tmux Runtime 可以继续
attach，结果未知的写入或 Agent 请求不能自动重放。

验证入口见[开发流程](workflow.md)。
