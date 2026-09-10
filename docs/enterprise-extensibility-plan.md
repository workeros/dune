# 企业集成边界

Dune 的企业扩展只有三条公开边界：身份、访问策略和 Managed 服务。

## 身份

宿主实现 `pkg/identity.Service`，验证自己的 opaque 浏览器 Session，并直接返回
企业用户唯一标识。Dune 不实现企业登录协议，不保存企业账号、Session 或身份
映射。SandDance 的 `SessionIdentity` 提供这一适配形状。

## 访问策略

宿主实现 `pkg/access.Checker`。Dune 先读取权威 Runner/binding，再向策略传递固定
scope；拒绝、错误、timeout 或非法结果都不会回退到默认 owner allow。peer owner
对每个请求重新验证 Session、Runner 和策略。

## Managed

宿主实现 `pkg/managed.Service`，负责模板、创建、暂停、恢复、销毁、Operation、
状态与 review。该实现自己拥有 provider 凭据、数据库、worker、重试和核对。
Dune 只提供现有 Web 路由和前端展示，不持久化 lifecycle 状态。`host.Open` 在
发布路由前调用服务的 `BindRunnerAccess`，注入只能创建/查询 Dune Runner 与
一次性 enrollment 的能力；外部服务不接触 Dune SQL 或 Gateway 内部对象。
pause/resume/destroy 被服务接受后，Dune 依据自己的 Runner 记录冻结、解冻或
撤销连接访问，不接受服务回传的 machine ID 作为授权事实。

SandDance 可以继续使用其 TAE provider 和 tenant binding registry 作为内部实现
细节，但不能把底层 provider 或 worker 配置注入 Dune。

## 存储

企业 PostgreSQL 的 Dune schema 固定为 `dune_runners`、`dune_enrollments`、
`dune_routes` 三张逻辑表。Managed 服务自己的表不属于 Dune schema。Dune 不
提供备份恢复、历史回滚、恢复代次或旧 schema 迁移。

## 部署

静态 Web 由 Nginx 服务；`/api/`、`/downloads/`、`/tunnel` 代理到 Dune 后端；
peer mTLS listener 直接连接各实例。PostgreSQL route 租约允许 Gateway 自动接管，
但活跃浏览器流会短暂断开，结果未知的请求不重放。
