# ACP 提交与启动回执

调用方在首次发送前生成并保存 `submission_id`。ID 为 1–128 个 ASCII
字母、数字、`-` 或 `_`；SDK 不生成、不补齐、不替换业务 ID。网络 Request ID
独立存在。完整键包含 Owner、Runner ID、Fabric ID、Machine ID 和 binding revision。
启动键不包含 Runtime；运行期键另外包含原 Runtime ID、incarnation 和 generation。

## 启动

```go
// owner 和 binding 来自已经授权且固定的环境选择；submissionID 在调用前保存。
key := api.SubmissionKey{
    SubmissionID: submissionID,
    Target: api.SubmissionTarget{
        OwnerID: owner, RunnerID: binding.RunnerID,
        FabricID: binding.FabricID, MachineID: binding.MachineID,
        BindingRevision: binding.Revision,
    },
}
result, stream, err := connection.Start(ctx, api.StartRequest{
    SubmissionKey: key,
    Profile: profile,
})
if stream != nil {
    defer stream.Close() // 只关闭观察连接，不停止已接纳的会话。
}
if err != nil {
    // result 仍携带原键；errors.As 可取得 *api.SubmissionError。
    // 保留原键，重新连接后 QuerySubmission；不得自动再次 Start。
    return err
}
runtime := *result.Runtime // Start 成功要求 admission=accepted、stage=started。
_ = runtime
```

`api.StartRequest.Worktree` 可指定源目录、目标路径、分支和 ref。worktree 创建、
Profile setup 和 Agent 启动都在独立索引接纳之后执行；worktree 仍单独经过现有权限
检查。初始 Runtime 身份及 stop/forget 配额在接纳事务内预留。

`api.StartResult` 包含原提交键、admission、operation_ref、stage、关联 Runtime 和已确认
worktree。setup 失败的当前返回还带有界 `failure` 诊断；独立索引保留阶段及错误码，
不持久保存 setup 输出、完整环境、提示词或凭据。Launcher/MCP/HTTP 的
`agents.StartRequest.submission_id` 同样必填，错误 `LaunchResult` 保留键和部分结果。

`stage` 是最近一次持久确认的检查点：`accepted`、`worktree`、`setup`、
`host_starting`、`started` 或 `failed`。`accepted` 中的 Runtime 是预留身份，
不证明进程已启动。setup 中杀死连接器后，`stage=setup` 不证明 setup 仍运行或完成；
重连不会重做它。独立宿主实际启动后可自行登记 `started`，不依赖原响应连接。

## 查询和重复提交

```go
receipt, err := connection.QuerySubmission(ctx, key)
```

查询使用当前认证和原完整键，不要求活动 Runtime 存在。它不创建任务、不消耗提交
额度，也不产生拒绝墓碑或恢复执行副作用。

| admission | 含义 |
| --- | --- |
| `unknown` | 尚无足够接纳证据；无记录也可能是原请求尚未到达 |
| `accepted` | 原提交已接纳；operation_ref 只标识原操作，不代表业务成功 |
| `not_accepted` | 已持久封闭该键，后到副本也不能执行 |
| `expired` | 结果无法再确认，不恢复重放许可 |

相同 Runtime 键用于所有操作类型；更改正文或操作会返回 `SUBMISSION_CONFLICT`。
精确重复 ACP 提交返回原 operation_ref。重复启动返回 `ALREADY_SUBMITTED` 和原回执，
防止 Launcher 再次执行后续 MCP 配置或初始 new。旧请求不能通过改用新 ID 自动恢复；
新 ID 代表新的用户意图。

当前最小接纳证据不会按 TTL 删除。普通表满后拒绝新普通提交，已有键仍可查询；
控制预留独立计费。实际运行期控制接入、原始 ACP 续接及完整验收进度见
[实施记录](acp-session-lifecycle-implementation.md)。

## 运行期提交与必要控制

`Client.ACPSubmit(ctx, key, action)` 接纳 new/load/list/prompt，返回的
`AgentOperation.Submission` 保存原键及回执；初始 `pending` 仅表示已经接纳，
当前执行结果由 `WaitAgentOperation`/`ReadAgentOperation` 读取。
`agents.PromptRequest`、`agents.OpenSessionRequest` 和 `host.AgentAction` 同样要求
调用方预先持有 `submission_id`。Launcher 的初始 new 使用原 launch ID 加新 Runtime
作用域，不生成无法预先查询的业务 ID。

`Client.ACPControl(ctx, key, action)` 接纳 permission/cancel。permission 指定原
`permission_id` 和有效 `option_id`；cancel 必须指定原 prompt 的 `operation_ref`。
这些控制在发布权限或接纳 prompt 时已经预留容量，不占普通操作结果和普通键额度。
无效目标不占用预留；已消费目标的精确重复返回原回执，不再次写入 Agent。
`stage=written` 只确认管道写完；失败保留 accepted，并标记 `input_unrecoverable`。
控制结果使用原键查询，不进入普通操作输出日志。

`Client.Stop(ctx, key)` 返回 `SubmissionReceipt`。网络 `runtime.stop` 请求体必须为
`SubmissionRequest{SubmissionKey: key, Operation: "runtime.stop"}`，不含 ACP payload。
提交键、Runtime 专属停止额度和 `accepted/stopping` 在同一事务内登记；普通结果缓存
或普通键表满不会阻断停止。宿主关闭 Agent 所有权管道并核实原进程组退出后，才记录
`stage=stopped`。显式 new/load 的并发进程替换也纳入停止范围；阻塞的 ACP 输入不能
阻挡停止。响应丢失时使用原键查询，不重新发送一个新 ID。相同键的重复停止只返回
原回执；`accepted/stopping` 仍然不是退出确认。PTY 的既有停止行为仍清理终端及历史。

```go
// stopKey 包含发送前保存的 submission_id 和准确的原 Runtime 身份。
receipt, err := connection.Stop(ctx, stopKey)
if err != nil || receipt.Stage != "stopped" {
    // 此处保留 stopKey；重新连接后只读 QuerySubmission(ctx, stopKey)。
    // 不把超时、EOF 或 Runtime 从目录消失当作停止成功。
}
```

已完成控制证据达到硬上限后，限制新权限/新可取消工作；不回收证据来释放重复执行
许可。状态和操作读取使用独立并发额度，不使用普通传输结果缓存。

### 流并发与持久证据预算

SDK、Gateway、fabricd 与独立宿主按解码后的实际操作隔离并发。控制 envelope 必须
包含合法提交键、匹配的完整 Runtime 和实际控制动作；并发类别不授予业务权限。
Gateway 仍执行正常鉴权，宿主仍检查当前权限／可取消目标。各层不借用其他类别的空槽。

| 类别 | 单 SDK／Gateway 连接 | 每个 fabricd／独立宿主 | Gateway 全局 |
| --- | --- | --- | --- |
| 普通操作及订阅 `ordinary` | 64 | 64 | 512 |
| 状态、模型、回执等短查询 `read` | 16 | 16 | 128 |
| 操作长轮询 `wait` | 16 | 16 | 128 |
| 权限回答 `permission` | 16 | 16 | 128 |
| 取消 `cancel` | 16 | 16 | 128 |
| 停止 `stop` | 16 | 16 | 128 |
| 清理 `forget` | 16 | 16 | 128 |
| 未分类首包 `opening` | Gateway 8；SDK 无入站首包 | 8 | 64 |

这些默认值也是当前硬上限，尚不提供用户配置范围。fabricd 和宿主预算覆盖该进程的
全部连接，旧连接排空期间仍计费。清理由 fabricd 的独立注册索引执行，宿主不接受
forget。首包最多 4 MiB、读取期限 5 秒；Yamux 待接收 backlog 为 169（包含连接
控制流），增加 backlog 不增加普通执行额度。未分类首包、网络和鉴权本身仍受有界
限流；这里保证普通**已分类工作**占满后保留控制能力，不保证在无限连接或半包洪泛
下每次请求都成功。首包槽不足时关闭对应流，不猜测业务接纳结果。

`Binding.Limits` 公布 `streams_ordinary/read/wait/permission/cancel/stop/forget/opening`，
`streams=168` 是含首包读取的总业务流预算；单独的连接控制流不计入该值。
`machine.info.stream_capacity` 返回 fabricd 各类实际 `used/limit`；
`Gateway.Status().StreamCapacity` 返回 Gateway 的全局用量，包含尚未退出的回调。
并发拒绝使用 `RESOURCE_EXHAUSTED`，detail 指明层和类别；它不是持久 `not_accepted`。

流并发不替代持久预算：每个安装的普通提交键最多 4096；permission 和 cancel
分别最多 4096 个“未消费预留＋已消费证据”，已完成控制不另借一份额度；每个注册
Runtime 各有一个 stop 和 forget 槽。安装最多 16 个活动 Runtime、256 个保留身份。
这些是当前 fabricd 默认和硬上限，尚无对外配置。注册库内部测试／嵌入选项的键与
控制上限范围为 1–65536，配置持久化后，各宿主必须使用相同值。
普通操作结果为每 Runtime 64 条、每条最多 512 KiB／1024 条更新，完成后最多保留
15 分钟；排队上限为 32。当前最小接纳证据不自动回收，容量不足限制新普通工作，
已有键和控制预留保留。内部回执读取另限 8 并发、状态读取 16、模型分页读取 8，
操作长轮询另限 16，不占用状态读取槽。

托管 ACP 的网络入口统一为 `submission.acp`；不再接受无键 `acp.action` 请求。
Gateway 将 envelope 内的实际 action 交给授权策略，策略仍分别检查
new/load/list/prompt/permission/cancel，envelope 本身不授予宽泛操作权限。

HTTP 提供 `POST /agents/submit`（`agents.SubmissionRequest`）和
`POST /agents/submission`（`agents.SubmissionQuery`）；MCP 对应
`agents_submit` 和 `agents_submission`。两者以当前授权 Owner 和调用方原
`agent_ref + submission_id` 定位完整键。原生会话已变更或 Runtime 不在活动目录时，
查询仍按原 Runtime 键进行；不会先读取当前原生会话来替换目标。通用 submit 直接
传递显式 ACP 参数，prompt 的 `expected_conversation_id` 和控制目标仍由宿主验证。
`action: "stop"` 和 `action: "forget"` 通过同一 HTTP/MCP 入口控制原 Runtime（包括 PTY），
不带其他 ACP 参数，也不依赖当前原生会话仍与 agent_ref 一致。`host.AgentConnection.Stop`
同样要求调用方提供 submission_id，并提供按原 Runtime 查询回执的方法。
返回 receipt 中的 `operation_ref` 是原 Runtime 的 SDK 操作选择器；普通
Prompt/OpenSession 便利接口返回的 operation 引用另外封装了跨 host 路由目标。

Web 的 Runtime 面板在“提交记录”中保留刷新后的查询入口。每条记录在发送前保存
原 binding、Runtime 身份、agent_ref 和 submission_id，不保存任务或权限正文。
普通记录上限为 64，permission/cancel 记录独立上限为 512；stop 和 forget 各有
256 条独立记录空间，序列化总量限制为 4 MiB。
达到上限时要求显式移除不再需要的本地记录；不会自动丢弃未知提交来腾空间。
“查询原提交”只读接纳证据及原操作状态，不重发操作；“移除本地记录”不停止 Agent。
工作台的“全部提交记录”在 Runtime 已不在目录或面板关闭后仍保留查询入口。
已丢失会话仍可显式清理；清理记录在活动 Runtime 消失后保留，显示已确认步骤及未完成状态。

## 显式清理与恢复

```go
// forgetKey 在首次发送前保存，包含原 Runtime 的完整身份。
receipt, err := connection.Forget(ctx, forgetKey)
if err != nil || receipt.Stage != "completed" {
    // 保留原键，只读 QuerySubmission；accepted 只表示原清理已接纳。
}
```

网络 `runtime.forget` 必须携带 `SubmissionRequest{SubmissionKey: key,
Operation: "runtime.forget"}`，不再接受无标识请求。独立索引先检查同作用域的键占用，
再在与进程注册共用的事务边界内核实已退出或宿主及进程组已确认丢失、保存固定资源
计划并封闭 Runtime。IPC 超时不能授权清理。原宿主已丢失时无需 stop、原宿主 ACK
或替代进程。生命周期核实失败但未保存拒绝决定时，回执仍是 `unknown`。

`cleanup.confirmed` 和 `cleanup.remaining` 保存可信步骤。常驻 ACP 的步骤为
`host`、`ipc`、`runtime_directory`；PTY 为 `terminal`、`runtime_directories`。
退役 tmux 实例时核对原身份；目录与 socket 先移入不覆盖现有资源的专属隔离位置，
再通过固定目录句柄核对原 inode／实例标记并清理。路径复用、权限错误或无法确认
删除时保留 `accepted/cleaning` 及有界错误码，不猜测完成。项目文件和 Agent 原生
历史不属于清理计划。PTY stop 已删除的活动条目可由其独立 stopped 回执核实后清理
身份预留；自然退出的 PTY 则清理终端历史及 Dune 生成的辅助缓存。

全部步骤持久确认后才标记 `completed`，释放 Runtime 活动配额；原键、资源计划、
完成回执与身份墓碑继续有界保留。fabricd 启动及每 30 秒的恢复调度只续办原已接纳
计划，单次执行使用 20 秒期限，未确认步骤等待后续调度。旧执行器的实际动作退出
前不会释放安装锁；步骤登记另有执行器代次隔离。查询和重复提交都不启动调度，
也不会重放其他任务。适用平台及全部 L01–L55 验收范围见实施记录。

## 宿主丢失的证据

独立索引在 Agent 执行前保存原 Runtime、宿主实例、机器启动标识和宿主进程记录。
guardian 先等待登记屏障，原进程组登记成功后才启动 Agent；登记失败或宿主在屏障
期间死亡不会启动任务。显式 new/load 必须先确认上一进程组不存在，再更新原宿主的
进程代次。已退出或已封闭的 Runtime 不能登记替换进程。

发现从独立索引读取原身份，不依赖 Runtime 目录仍在。IPC 失败本身只返回
`availability=unavailable` 并保留最后确认状态；原宿主和所属进程组均不存在（或已换
机器启动代次）时，返回 `state=lost`、`availability=lost`，不编造退出码或任务成功。
PID 只参与只读存在性检查，不用于发信号、重建或认领活宿主。旧 operation 查询报告
`SESSION_LOST`，提交回执仍可按原键读取。工作台保留原面板并区分“暂不可用”和
“已丢失”，不会自动启动替代会话。
