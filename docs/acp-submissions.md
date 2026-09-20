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

托管 ACP 的网络入口统一为 `submission.acp`；不再接受无键 `acp.action` 请求。
Gateway 将 envelope 内的实际 action 交给授权策略，策略仍分别检查
new/load/list/prompt/permission/cancel，envelope 本身不授予宽泛操作权限。

HTTP 提供 `POST /agents/submit`（`agents.SubmissionRequest`）和
`POST /agents/submission`（`agents.SubmissionQuery`）；MCP 对应
`agents_submit` 和 `agents_submission`。两者以当前授权 Owner 和调用方原
`agent_ref + submission_id` 定位完整键。原生会话已变更或 Runtime 不在活动目录时，
查询仍按原 Runtime 键进行；不会先读取当前原生会话来替换目标。通用 submit 直接
传递显式 ACP 参数，prompt 的 `expected_conversation_id` 和控制目标仍由宿主验证。
`action: "stop"` 通过同一 HTTP/MCP 入口停止所选 Runtime（包括 PTY），不带其他
ACP 参数，也不依赖当前原生会话仍与 agent_ref 一致。`host.AgentConnection.Stop`
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
forget 的执行接入与崩溃恢复仍在后续实现切片中；此处的本地额度不代表清理已交付。
