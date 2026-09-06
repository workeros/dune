# 执行流访问检查

`pkg/access` 是协议核心之上的访问模块。`Grant.Policy` 为经过认证的 SDK 连接选择一个 `Checker`；`Scope` 来自宿主验证的用户、Runner 及绑定记录，不能来自执行请求体。`Bind` 复制该关联，后续消息不能替换用户或目标。机器连接不接受用户 Policy。

```go
binding, handler, err := (access.Grant{
    Target: scope.Binding.MachineID,
    Role: gateway.RoleSDK,
    Valid: sessionAndBindingStillValid,
    Policy: &access.Policy{Scope: scope, Checker: checker},
}).Bind()
// 接入模块将已认证的连接及这两个结果交给 gateway.ServeConn。
```

这里的 `scope`、`checker` 和有效性函数由可信宿主提供。`access.Owner{}` 是默认个人 owner 检查器；企业实现替换它，不采用任意一个允许就放行的组合。`Grant.Valid` 独立复核会话、机器及绑定，企业 allow 不能越过它。单独使用 `Grant` 且不设置 Policy 仍表示宿主明确选择的连接级授权，适用于原有独立协议宿主。

目前这份处理器已通过独立 Gateway、真实 fabricd 和执行客户端验证，尚未装配到 `host.Options` 的企业配置入口。默认工作台仍使用已有 owner 授权。企业开关将在 Web/CLI 发现及管理入口也接入同一检查器后提供；不能将此组件验收视为完整 S1 企业授权已交付。

## 决定及期限

`Checker.Check(ctx, Request)` 返回 `Decision`：`Allowed`、受控的 `Reason`（1–64 位大写字母、数字或下划线）、非空决定 `ID`（最多 128 字节）和 `ValidUntil`。检查器须并发安全并响应 context。错误、超时、无效或过期决定均拒绝，不将上游错误详情传给执行客户端，也不回退到 owner 检查。

每个新流在转发前检查。每种持续操作取得该流自己的短期决定，终端字节不逐个调用检查器。单次检查最多一秒，允许期限最多三十秒；外部绝对时间转换为本机单调期限。后台在有效期中点提前复核，独立定时器在截止时关闭流，因此空闲流或未返回的检查器也不能延长访问。复核失败立即关闭该流，迟到 allow 不恢复旧流。关闭访问不回滚已受理操作，也不停止远端任务。

## 操作映射

检查请求保留固定 Scope、原始流 RequestID、实际 operation/子操作和必要的选择属性。没有自建企业角色目录，也不接受检查器下发任意协议约束。未识别的请求、子操作和模式拒绝。

| 协议入口 | 检查中的子操作或模式 |
| --- | --- |
| `machine.info`、`runtime.list` | 对应操作；不隐式授予执行权限 |
| `profile.start` | 保留 adapter、managed ACP 标记和工作目录；不提供命令或环境变量 |
| `runtime.attach` | 保留 observe 标记；只读订阅拒绝所有输入类型 |
| `runtime.get`、`runtime.capture`、`runtime.stop`、`runtime.forget` | 分别检查对应操作和 Runtime 身份 |
| `runtime.history` | `older`、`newer`、`close` |
| `exec` | 对应操作及工作目录；无命令正文 |
| `files` | `stat`、`list`、`read`、`write`、`mkdir`、`rename`、`remove` |
| `upload` | `create`、`query`、`chunk`、`commit`、`cancel`，提交单独检查 |
| `git` | `status`、`diff`、`log`、`show`、`stage`、`unstage`、`discard`、`commit`、`amend`、`branch`、`checkout`、`stash`、`fetch`、`pull`、`push`、`merge`、`rebase`、`conflicts` |
| Git 模式 | branch 的 `list/create`、checkout 的 `switch/create`、stash 的 `push/list/pop/apply/drop`、merge/rebase 的 `start/continue/abort`；按执行参数归一化 |
| `agent.config` | `list`、`save`、`delete`；不提供保存的 Agent 命令 |
| `acp.state`、`acp.action` | state 与 `new/load/list/prompt/permission/cancel` 分别检查；无 prompt 或审批正文 |
| `ports.connect` | 具体 loopback 端口；后续 `data/eof` 各自取得决定 |
| PTY 持续流 | 在原 start/attach 下分别检查 `input`、`resize`、`signal`；signal 仅传 INT/TERM/HUP/QUIT 名称 |
| 原始 ACP 输入 | `acp.raw/exchange` 整体授权；不声称逐条解释或授权任意 JSON-RPC 方法 |

持续流的实际 Runtime ID、incarnation/generation 与 adapter 从 fabricd 返回中取得，收到有效 Runtime 响应前不能输入；attach 响应还须与请求的 Runtime 身份一致。首条请求中选择的 Runtime 身份仍由协议与 fabricd 校验，企业检查不能代替执行身份约束。

属性只表达已知的选择信息。上传仅在 create 时提供路径，后续动作只提供上传句柄，忽略客户端伪填的路径；托管 ACP 的 prompt/permission/cancel 不把被执行端忽略的 cwd 当作实际目录。空属性不代表根目录或任意资源授权，需要缺失属性才能成立的策略应拒绝。文件内容、Git patch/提交正文、命令、环境变量、终端内容、Agent prompt 和凭据不进入检查请求。目录属性不是 OS 沙箱，允许 Shell 执行后不能据此限制其内部所有系统调用。

## 验证

`go test -race ./pkg/access -count=1 -timeout=60s` 使用独立 Gateway 和真实临时 fabricd/PTY，验证拒绝写入与上传提交、只读订阅的三种输入隔离、固定身份、内容裁剪、输入决定复用、空闲撤销以及阻塞/迟到检查器。测试使用构建产物 `bin/tmux`（也可通过 `DUNE_TMUX` 指定），在自身私有目录清理进程和 tmux 会话，不调用真实 Agent 服务。
