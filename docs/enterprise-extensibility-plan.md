# 企业集成边界

Dune 的企业扩展有五条公开边界：身份、访问策略、Managed 服务，以及受信宿主的
Profile 执行和 Attached 操作。

## 身份

宿主实现 `pkg/identity.Service`，验证自己的 opaque 浏览器 Session，并直接返回
企业用户唯一标识。Dune 不实现企业登录协议，不保存企业账号、Session 或身份
映射。SandDance 的 `SessionIdentity` 提供这一适配形状。

## 访问策略

宿主实现 `pkg/access.Checker`。Dune 先读取权威 Runner/binding，再向策略传递固定
scope；拒绝、错误、timeout 或非法结果都不会回退到默认 owner allow。peer owner
对每个请求重新验证 Session、Runner 和策略。

## Managed

宿主实现 `pkg/managed.Service`，负责模板、创建、暂停、恢复、销毁、Operation
和状态。该实现自己拥有 provider 凭据、数据库、worker 与失败策略。
Dune 只提供现有 Web 路由和前端展示，不持久化 lifecycle 状态。`host.Open` 在
发布路由前调用服务的 `BindRunnerAccess`，注入只能创建/查询 Dune Runner、签发
或撤销一次性 enrollment，以及开关用户访问 gate 的能力；外部服务不接触 Dune
SQL 或 Gateway 内部对象。Managed 服务按 provider 生命周期顺序调用这些 gate，
Dune 始终依据自己的 Runner 记录处理，不接受服务回传的 machine ID 作为授权
事实。

SandDance 可以继续使用其 TAE provider 和 tenant binding registry 作为内部实现
细节，但不能把底层 provider 或 worker 配置注入 Dune。

## Profile 宿主执行

`host.App.RunnerExecutor()` 是受信进程内的 Environment Profile 执行边界。
宿主提供已验证主体、精确 owner/binding、稳定 execution ID 和完整 Profile；Dune
复核访问策略后经 Gateway 路由到 fabricd。`Prepare` 透出有界进度与最终结果，
`Status` 只观察原尝试，不会触发执行或自动重放。该边界不发布远程身份入口；
主体续期和撤销不属于当前合同，由宿主在调用前保证主体仍有效。

## Attached 宿主操作

`host.App.AttachedRunnerManager()` 是受信进程内边界，不发布用户 HTTP API。
`Get` 只返回活动 Attached Runner 的 owner、creator 和 binding 事实，供宿主应用
自己的成员与角色策略；`HasActive` 用于 Tenant 删除前的有界检查。
`CancelEnrollment` 由宿主授权后调用，Dune 在同一事务内删除一次性 token 并停用
尚未绑定的逻辑 Runner。它与 enrollment 使用相同的锁顺序；若 enrollment 先完成，
返回 `runner.ErrBindingChanged` 且保留 binding 和 machine credential。
若数据库提交结果不可确认，则返回 `host.ErrAttachedResultUnknown`；宿主必须先用
`Get` / `HasActive` 刷新 Dune 事实，不能自动重试取消。

## Provider 配套工具

`fabric.NewBootstrapPlan` 接受原 `BootstrapCall` 和明确的目标 OS/architecture，
生成下载对应 Dune archive、解压、enroll、启动 fabricd 及写完成标记的脚本。
计划包含一次性 token，不得记录或持久化。Provider 仍负责自己的远端命令 API，
并在 `ReconcileBootstrap` 中只读检查 `fabric.BootstrapCompletionPath`，不能因标记
未知而重放安装脚本。

`pkg/fabric/fake` 是确定性的进程内测试 Provider，覆盖 create/bootstrap/inspect、
renew、pause/resume、destroy 和 candidate 合同。其状态不持久化，也不提供生产
资源隔离或恢复保证。

## 存储

企业 PostgreSQL 的 Dune schema 固定为 `dune_runners`、`dune_enrollments`、
`dune_routes` 三张逻辑表。Managed 服务自己的表不属于 Dune schema。Dune 不
提供备份恢复、历史回滚、恢复代次或旧 schema 迁移。

## 部署

静态 Web 由 Nginx 服务；`/api/v1/`、`/api/v1/downloads/`、`/api/v1/ws/` 代理到 Dune 后端；
peer mTLS listener 直接连接各实例。PostgreSQL route 租约允许 Gateway 自动接管，
但活跃浏览器流会短暂断开，结果未知的请求不重放。
