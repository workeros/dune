# managed ACP 原生会话编排

`App.AgentNativeSessions().OpenSession(scope, request)` 只在已有 Runtime 内执行显式 `new` / `load`。请求包含 `agent_ref`、`action`、load 的 `session_id`、可选绝对 `cwd` 和 `wait_ms`（0–30000）。两产品共享 `POST /api/v1[/tenants/{tenant}]/agents/open-session`，PTY 不适用。

服务校验身份、Tenant、Runner binding、Runtime 和原生会话引用，确认 ACP 就绪后经 SDK / Gateway 进入 fabricd 的同一队列。load 能力由 fabricd 检查，不要求 list，不回退 new。可选等待失败也保留操作引用，供 wait/read 查询。

初始启动在确认 Runtime 后配置 MCP，再提交一次 new。成功 RPC 才更新当前 `native_session`；pending、失败或未知结果不假称切换成功。输出中的原生确认属于该操作，Runtime 中保留最新确认。宿主不复制这些信息到恢复数据库。

这项能力用于存活 ACP 进程中的原生对话切换；进程退出后没有重启和工作台恢复。两产品 new/load/prompt 使用共享引用，list、permission、cancel 沿用 managed 控制路径；原始 ACP 透传仍由调用方编排。

本地协议回归覆盖启动后立即 prompt、busy 时 load 排队、按匹配操作确认切换、旧引用与跨 Tenant 拒绝，以及初始化失败时保留 Runtime / 操作且不重放。
