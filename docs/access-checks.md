# 身份与访问检查

Dune 将登录、资源事实、策略和传输分开验证。浏览器给出的 target、Runner、
Fabric、binding revision 或 Runtime 标识都不能替代服务端读取的事实。

## 登录接口

`pkg/identity.Service` 是公开宿主边界：

```go
type Service interface {
    Authenticate(context.Context, string) (identity.User, error)
    Logout(context.Context, string) error
    Namespace() string
    LoginMethods(publicURL string) []identity.LoginMethod
}
```

默认 `internal/identity.Local` 同时实现 `PasswordService`，使用 `dune_users` 与
`dune_sessions`。只有实现了该可选接口时，Web 才发布 register/login 路由。

企业部署由 SandDance 验证已有 opaque 浏览器 cookie，返回企业用户唯一标识。
该 ID 直接成为 `identity.User.ID`；Dune 不创建 principal、影子账号、OIDC 回调、
身份关联或自己的企业 Session。`Authenticate` 必须反映当前禁用/注销状态。

## 请求授权

资源选择先从 `dune_runners` 读取 owner、kind、Fabric、machine 和 binding revision，
再构造 `access.Request`。默认策略只允许 owner；企业 `AccessChecker` 可以授予共享
访问，但仍收到固定的服务端 scope。

一次浏览器执行连接的顺序是：

1. 通过当前 `identity.Service` 验证 cookie；
2. 读取并固定 Runner binding；
3. 执行 `runner.connect` 策略；
4. 在当前 Gateway 内存中生成 30 秒、单次消费票据；
5. 握手消费票据时再次验证 Session、Runner 与策略；
6. 连接存续期间约每秒再次执行 Session 和 Runner 有效性检查；
7. 每个业务请求及持续输入仍受固定 scope、Runtime identity 和策略限制。

票据不写数据库，不能由另一进程消费。Web 后端应通过自己的本地 Gateway 入口
拨号；该 Gateway 如非 owner，再使用 mTLS peer 单跳转发。owner 独立重验当前
Session、Runner 和策略，不能只信入口。

## 撤销

- logout 由当前身份服务撤销 Session，已有用户连接在下一次有效性检查关闭；
- 本地 `App.SetUserEnabled` 持久禁用用户、递增 auth version、清除 Session 与
  未消费 enrollment；重新启用不会复活旧 cookie；
- Runner 解绑持久设置 `enabled=false`、清除 connector 凭据并主动断开当前
  Gateway；远端副本由周期检查和 owner 租约兜底；
- Managed pause 持久设置 `suspended=true` 并断开当前连接，resume 清除冻结但不
  复活已撤销凭据；destroy 同时禁用 Runner、清除凭据并删除未消费 enrollment；
- 撤销不会删除开发机文件、tmux Session 或自动停止 Agent。

不维护逐实例断开回执。若调用返回 unknown，调用方通过当前用户/Runner 状态
核对，不能自动重放写操作。

## 实现要求

自定义身份和策略实现必须响应 context、限制并发、避免在错误中泄露 cookie、
token、工作内容或私有 provider 信息。策略返回的有效期不能超过 Dune 的上限；
超时、panic、错误或非法结果均按不可用/拒绝处理，不回退到 owner allow。
