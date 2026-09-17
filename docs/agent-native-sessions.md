# managed ACP 原生会话编排

`App.AgentNativeSessions().OpenSession(scope, request)` 在已有 Runtime 内执行明确的 `new` / `load`。请求包含 `agent_ref`、`action`、load 所需 `session_id`、可选绝对 `cwd` 和 `wait_ms`（0–30000）。Scope 来自宿主身份；PTY 不适用。两产品共享 `POST /api/v1[/tenants/{tenant}]/agents/open-session`。

服务检查 Tenant / Owner、Runner binding、Runtime 和所选原生引用，确认 ACP 就绪后经 SDK / Gateway 提交给 fabricd。load 能力由 fabricd 验证，不调用 list，也不回退 new。排队与执行仍只发生在 fabricd。返回 [操作引用](agent-messaging.md)，可经 `agents/wait`、`agents/read` 查询；可选等待失败也保留已经受理的引用。

初始启动在保存实际配置和 Runtime 后走同一提交路径，自动创建原生会话。正常返回时可以直接 prompt；未完成时返回 pending/running 操作供查询。会话只在匹配 RPC 确认后入索引，new/load 失败、pending 或响应丢失不改变原来的原生 ID。成功 load 切换会话会建立独立恢复记录，保留原配置来源。

操作 wait/read 与共享发现同样能保存可靠确认；这些操作只补索引，不重做 new/load。Runtime 消失前仍未观察到确认时不能声称可恢复。Web 的原生会话按钮仍待迁移到此共享入口，原始 ACP 透传继续由调用方编排。

本地 Gateway/fabricd 验证覆盖启动即用、无发现请求时的索引保存、busy 时 load 排队、匹配操作确认后切换索引、旧原生引用拒绝、跨 Tenant 拒绝，以及初始 new 失败或回执丢失时保留 Runtime/操作且不重放。使用协议夹具，不代表厂商 Agent 或 MCP 注入验收。
