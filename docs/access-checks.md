# 访问检查与有界发现

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

默认工作台通过 `host.Options.AccessChecker` 装配同一个检查器，覆盖浏览器、人类 CLI 的发现、Attached 管理、连接交换和执行流。留空选择 `access.Owner{}`；设置企业检查器后由它明确决定共享访问，仍独立核验当前用户会话、身份源、机器和固定绑定。检查器须在共享数据库的每个宿主上采用一致策略；身份提供方不替代此决定。

```go
app, err := host.Open(ctx, host.Options{
    DataDir: privateDataDir,
    PublicURL: publicURL,
    Assets: webAssets,
    Identity: companyIdentity,
    AccessChecker: companyAccessChecker,
})
```

`companyAccessChecker` 是宿主实现的 `access.Checker`，可调用企业私有 SDK；官方二进制仍采用默认 owner 规则，不内置企业依赖或共享管理 UI。`pkg/access` 也继续支持上面的独立协议宿主用法。工作台使用逻辑 Runner 和固定绑定；Managed 生命周期和集群自动路由属于后续检查点。

`Scope.PrincipalID` 是稳定 Dune 用户 ID；`Namespace`、`Subject` 是本次登录由提供方验证的外部身份引用（OIDC issuer/sub），本地密码登录时两者为空。企业检查器可用该引用查询自己的权限系统；不要将 Dune ID 当成上游 subject，也不要按邮箱或当前关联列表猜测本次身份。同一 principal 关联的不同 subject 可以取得不同企业决定。引用随浏览器会话持久化，CLI 复制已确认父会话的引用，短期凭据在签发事务中捕获并在消费及持续流复核时保持不变，不传递上游 token、原始声明或登录时的 group 快照，也不向浏览器用户 JSON 暴露该引用。

Attached 安装材料另保存签发者的身份引用，消费时以原引用检查 `runner.create`，不能切换到宿主当前配置的其他身份源。它仍是独立的十分钟单次能力，浏览器退出不会删除已签发材料；到期、用户停用或显式身份关联使其失效，消费时的企业拒绝也会阻止接入。

## 产品入口与发现

| 产品操作 | 检查 operation / suboperation |
| --- | --- |
| Web/CLI 机器列表 | `machine.list/attached`，逐候选检查 |
| Web/CLI Runner 列表和详情 | `runner.list/attached`、`runner.get/attached` |
| 访问凭据签发与消费 | `runner.connect/attached`，两处都复核 |
| Attached 接入材料签发与消费 | `runner.create/attached`，两处都复核 |
| Attached 解绑 | `runner.unbind/attached` |

用户、owner、Runner/Fabric/机器和绑定修订均由当前认证与 SQL 元数据构造；当前只有 Attached。机器身份接入单独校验机器凭据，不把它当作人类登录。所有执行 API 经 Gateway 的同一流处理器再检查实际子操作。绑定位于检查与写入之间发生变化时，事务拒绝旧快照；写入时还复核原浏览器会话，停用后再启用也不恢复旧会话的写入资格。

`GET api/machines`、`api/runners` 及对应 `api/cli/` 列表返回 `{"items": [...], "next_cursor": "..."}`；没有下一页时省略 `next_cursor`。支持 `limit`（默认 32，最大 100）和不透明 `cursor`，单页最多扫描 128 个候选，总期限五秒。默认 owner 模式先按用户筛选候选；企业模式交给所选检查器逐项决定。拒绝项不出现在响应中，也不返回其 ID 或全局总数。达到扫描上限时可能返回空页和下一游标，调用方应继续翻页，不能将空页误认成列表结束。

游标只表示位置，不授予访问，绑定用户、身份源和列表用途。它在 SQL 中保存十分钟，每用户最多 64 个有效位置，相同位置复用已有游标；支持 SQLite 重开和 PostgreSQL 跨实例。每页重新授权，不能利用旧游标恢复旧权限。过期或不匹配返回 400，客户端可回到第一页；检查器故障返回 `503 ACCESS_UNAVAILABLE`，不能伪装成成功的空列表。明确拒绝的资源详情返回 404，避免泄露存在性。

正式 CLI 使用 `runners|machines --limit N --cursor CURSOR`，公开 Go 客户端为 `Runners(ctx, session, runner.Query)` 和 `Machines(ctx, session, runner.Query)`，返回相应 Page。它们不自动遍历全库或重新选择环境。

## 浏览器 Runner 入口

浏览器 `GET api/runners` 和详情在 Runner 模型上补充当前绑定的 `online`、`os`、`arch` 展示事实；未绑定时不允许执行。用户点击环境后保留完整绑定快照，后续请求不能只按 Runner ID 重新解析目标：

| 请求 | 路径 |
| --- | --- |
| 工作台操作 | `POST api/runners/{runner}/call` |
| 创建 PTY/托管 ACP 会话 | `POST api/runners/{runner}/sessions` |
| 事件订阅 | `GET api/runners/{runner}/sessions/{runtime}/events` |
| Attached 解绑 | `DELETE api/runners/{runner}/binding` |

这些入口均要求查询参数 `machine_id`、`fabric_id`、`revision`，Runner ID 由路径给定。参数是选择约束，不是凭据或权威身份；仍必须有当前浏览器会话，再由元数据、检查器及绑定事务验证。缺失或重复字段返回 400，已解析资源的过期/不匹配快照返回 `409 BINDING_CHANGED`（资源不可访问或不存在时返回 404），不会拨号到替代环境。解绑在事务内比较同一快照，成功后撤销对应机器身份及连接。

前端发现刷新不覆盖用户选中的绑定。发现绑定变化后卸载旧工作区和订阅，明确确认后才使用当前绑定；进入环境后仍需点选已有 Runtime，不自动重放创建、输入或其他写入。旧机器路径作为具体机器 API 保留，工作台自身所有执行与订阅均使用上述 Runner 路径。

Runner 和旧机器路径的事件订阅还必须携带用户选定的 `incarnation`、`generation`（正整数且各出现一次）。服务端先以完整 Runtime 身份执行有五秒期限的 `runtime.get`，确认返回身份一致后才执行 `runtime.attach`；企业策略须允许这两个操作。缺失或歧义身份返回 `400 INVALID_RUNTIME`，旧执行身份返回 `STALE_RUNTIME`，不再列出会话后按 ID 补齐最新身份。前端保留选择时的完整 Runtime，自动重连沿用原身份；刷新发现同 ID 的身份变化时关闭旧视图，用户重新点选后才连接新身份。升级须同步部署静态资源及事件 API 客户端，仅提供 Runtime ID 的旧订阅会被拒绝。

## 决定及期限

`Checker.Check(ctx, Request)` 返回 `Decision`：`Allowed`、受控的 `Reason`（1–64 位大写字母、数字或下划线）、非空决定 `ID`（最多 128 字节）和 `ValidUntil`。检查器须并发安全并响应 context。宿主将并发调用限制为 64，等待名额也计入检查期限。错误、超时、无效或过期决定均拒绝，不将上游错误详情传给执行客户端，也不回退到 owner 检查。

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

`TestEnterpriseSharedExecutionAndRevocation` 使用 SQLite 和两个共享 PostgreSQL 的宿主，验证另一个账号的机器只能经企业决定访问，真实 Web PTY/CLI 执行、文件写入拒绝、空闲策略撤销和父会话撤销。`TestAuthorizedDiscoveryAndWrites` 覆盖扫描上限、游标隔离/重开/跨池、拒绝或故障无 owner 回退、固定归属和写入竞争；SQL 转库及 PostgreSQL 原生备份保留归属和游标字段。测试检查器是有意控制允许与拒绝的测试适配器，不代表某个企业权限 SDK 的部署验收。
