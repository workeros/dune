# 身份与访问检查

Dune 将登录、资源事实、策略和传输分开验证。浏览器给出的 target、Runner、
Fabric、binding revision 或 Runtime 标识都不能替代服务端读取的事实。

## 登录接口

`pkg/identity.Service` 是公开宿主边界：

```go
type Service interface {
    Authenticate(context.Context, string) (identity.Authentication, error)
    Logout(context.Context, string) error
    Namespace() string
    LoginMethods(publicURL string) []identity.LoginMethod
}
```

默认 `internal/identity.Local` 同时实现 `PasswordService`，使用 `dune_users` 与
`dune_sessions`。只有实现了该可选接口时，Web 才发布 register/login 路由。

企业部署由 SandDance 验证已有 opaque 浏览器 cookie，返回企业用户唯一标识。
该 ID 直接成为 `identity.User.ID`；Dune 不创建 principal、影子账号、OIDC 回调、
身份关联或自己的企业 Session。认证结果包含 `User` 和凭据的真实 `ExpiresAt`，
不能返回缓存 TTL、零值或自行延长的期限。每次 `Authenticate` 必须使用身份源能提供的
最新事实。SandDance 的默认 Cloud JWT 适配器离线验签，要求有效 `exp`；它无法发现
上游注销，`LogoutSession` 不撤销 JWT，Dune 不把这一限制表述成可观察撤销。

## 请求授权

资源选择先从 `dune_runners` 读取 owner、creator、kind、Fabric、machine 和 binding revision，
再构造 `access.Request`。默认策略只允许 owner；企业 `AccessChecker` 可以授予共享
访问，但仍收到固定的服务端 scope。

一次浏览器执行连接的顺序是：

1. 通过当前 `identity.Service` 验证 cookie；
2. 读取并固定 Runner binding；
3. 执行 `runner.connect` 策略；
4. 在当前 Gateway 内存中生成最长 30 秒、单次消费票据，同时受凭据与策略期限限制；
5. 握手消费票据时再次验证 Session、Runner 与策略；
6. 每个新协议 request、既有交互流首次出现的新 action，实时验证身份、Runner 和宿主策略；
7. 同一流、同一 action 的连续消息复用流内允许事实，期限不晚于检查开始后 5 秒、
   凭据自然到期和宿主策略到期三者的最早值。

唯一允许缓存与刷新器是 `access.checkedStream`。Web 不按数据帧重复查身份，用户
连接也不另建连接级缓存。空闲流同样按期限刷新和关闭；拒绝、错误、超时或自然到期
均停止放行，迟到回复不能恢复已关闭流。机器连接保留独立凭据检查。

托管 ACP 的每次 prompt/permission/cancel 是新 request；原始 ACP 正文不解析，
只能按 `acp.raw/exchange` 整体执行流授权，不能宣称检查其中每条 JSON-RPC。

票据不写数据库，不能由另一进程消费。Web 后端应通过自己的本地 Gateway 入口
拨号；该 Gateway 如非 owner，再使用 mTLS peer 单跳转发。owner 独立重验当前
Session、Runner 和策略，不能只信入口。

## 撤销

- 身份源能观察到 logout/禁用后，已有用户流最迟在 5 秒内停止由 Gateway 放行；
  凭据自然到期不额外获得 5 秒；
- 本地 `App.SetUserEnabled` 持久禁用用户、递增 auth version、清除 Session 与
  未消费 enrollment；重新启用不会复活旧 cookie；
- Runner 解绑持久设置 `enabled=false`、清除 connector 凭据并主动断开当前
  Gateway；远端副本由周期检查和 owner 租约兜底；
- Managed pause 持久设置 `suspended=true` 并断开当前连接，resume 清除冻结但不
  复活已撤销凭据；destroy 同时禁用 Runner、清除凭据并删除未消费 enrollment；
- 撤销不会删除开发机文件、tmux Session 或自动停止 Agent。

普通用户解绑统一调用宿主 `runner.unbind`，以 Dune 查询到的 owner、creator 和
完整 binding 为准；不存在 `CanDetachAttached` 附加回调。个人待接入取消另行校验
owner 和 pending 状态，enrollment 已完成就返回 binding changed，不降级为解绑。

5 秒窗口从权威接口能够返回失效开始，到 Gateway 停止放行后续消息结束；它不回滚
已受理操作、不停止 Agent，也不清空已经转发的输入。peer owner 独立实时验证，
其租约不继承入口的旧允许事实。禁用某用户不能通过 target 级断连误伤其他身份。

不维护逐实例断开回执。Managed `AccessClosed` 表达持久门禁状态，`revoked` 只确认
门禁，`AccessCloseDeadline` 表达传播截止时间；没有持久事实则为 `unknown`。
若调用返回 unknown，调用方通过当前用户/Runner 状态
核对，不能自动重放写操作。

## 实现要求

自定义身份和策略实现必须响应 context、限制并发、避免在错误中泄露 cookie、
token、工作内容或私有 provider 信息。策略返回的有效期不能超过 Dune 的上限；
超时、panic、错误或非法结果均按不可用/拒绝处理，不回退到 owner allow。
