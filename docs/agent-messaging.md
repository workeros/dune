# Agent 通信服务

`App.AgentMessenger()` 与 AgentDirectory / AgentLauncher 共享 Owner / Tenant 鉴权和 SDK → Gateway 路由。它不保存 prompt、操作或输出，不承担排队。执行与有界结果仍由目标 fabricd 持有。

两产品提供相同 HTTP 入口，个人前缀 `/api/v1`，Tenant 前缀 `/api/v1/tenants/{tenant}`。Scope 从已认证上下文取得；请求不能注入 Owner / Tenant。

| POST 路径 | 参数 | 结果 |
| --- | --- | --- |
| `/agents/prompt` | `agent_ref, text, wait_ms?` | `operation_ref, state, stop_reason?, error?` |
| `/agents/send-keys` | `agent_ref, keys` | PTY 输入操作引用及状态 |
| `/agents/wait` | `operation_ref, timeout_ms` | `operation, timed_out`，只等待这一操作 |
| `/agents/wait` | `agent_ref, timeout_ms, until?` | `agent, timed_out`，仅观察活动 |
| `/agents/read` | `operation_ref, position?, limit?` | `operation`，含本次输出、位置、下一位置、完整性 |
| `/agents/read` | `agent_ref` | PTY `snapshot`，含 Agent 观察及当前终端屏幕 |

`wait_ms` / `timeout_ms` 为 0..30000，0 为立即观察；实际请求另有有限的连接及查询开销。活动模式的 until 为 idle、blocked、exited，或默认 attention（任一上述状态）。unknown / foreground 不等于 idle。引用选择互斥；活动条件不能用于操作等待，位置不能用于终端快照。ACP 内容按操作读取，终端快照不是完整历史。

Agent 引用固定 Runner binding、Runtime 实例及已知原生 ID / cwd。prompt 先读取当前 Runtime，再把确认的原生 ID / cwd 一起交给 fabricd；fabricd 在入队与出队时再次检查。尚未创建 / 加载原生对话时返回 SESSION_REQUIRED。PTY 从可信活动摘要取得目标 CLI 类型，fabricd 在写入前检查前台及 blocked / bracketed-paste 状态。

通信服务的 operation_ref 将完整目标与 fabricd 操作 ID 编码成不透明选择器，调用方原样保存即可。它不是凭据，不绑定某个宿主 Pod；每次使用重新授权。原生会话切换不影响旧操作的结果读取，Runtime / fabricd 失效或记录淘汰则返回明确错误，不能改读最新会话。操作结果保留该操作的原生确认；宿主不写恢复索引。

PTY 的 delivered 只证明输入已投递。需要另外以 agent_ref 观察活动和屏幕，不提供逐 prompt 输出。ACP 的 completed 来自匹配 RPC，也不意味着用户的任务已通过验收。

提交或确认连接中断均不自动重放。prompt 的可选等待失败时，HTTP 错误响应仍在 `result` 中保留已受理操作引用；调用方可以查询它，不应再次发送 prompt。没有引用的未知结果返回 RESULT_UNKNOWN。

验证使用真实本地 Gateway / fabricd、可控 ACP 协议进程及原生 PTY 字节记录进程：覆盖两个独立连接排队、A / B 等待和输出关联、调用方取消后的查询、原生切换、跨 Tenant 拒绝、终端投递与活动语义。这里还不是 MCP 或真实厂商 Agent 验收，跨 Pod 故障验收随整体验收完成。
