# Agent 发现与原生采集

`App.AgentDirectory()` 向可信宿主代码提供 `List(scope, query)` 和 `Get(scope, agent_ref)`。
Scope 来自已认证用户与 Owner / Tenant，引用本身不授予访问权。

HTTP 使用个人前缀 `/api/v1` 或 Tenant 前缀 `/api/v1/tenants/{tenant}`：

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| GET | `/agents?limit=32&cursor=…` | 按 Runner 分页，返回 items、runners、issues、next_cursor |
| POST | `/agents/get` | 接收 `{agent_ref}`，重新验证固定目标后返回当前观察 |

每页先进行 Owner / Tenant 发现授权，再查既有 Gateway 路由的在线状态。最多四个 Runner 并发读取，每个 Runner 最多等待五秒；整个发现有二十秒期限。没有 binding 或离线的 Runner 不发起连接。暂时无法读取的 Runner 单独进入 issues，已取得的其他 Agent 仍返回。ready 是在线状态和启动策略的当前预检查，实际启动继续校验绑定、配置及环境准备状态。

每个 Agent 包含不透明 agent_ref、完整执行目标、Runtime 活动摘要、Runner、可选恢复摘要。引用固定 Runner binding、Runtime incarnation / generation，以及已确认的原生 ID / cwd。每次 Get 重新经过授权和 SDK → Gateway → fabricd；已确认的原生会话改变时返回 `STALE_SESSION`。引用不包含 Tenant 凭据，也不能跳过所有权校验。它与 fabricd 的操作引用用途不同。

List / Get 从 fabricd 回执采集 native_session 到数据库，不接受调用方提交原生观察。启动快照存在时，按确认序号维护恢复记录和当前关联。直接由底层 SDK 启动且没有快照的 Runtime 仍可发现，但不会伪造恢复配置。数据库采集失败时保留 Runtime 并返回 `recovery_error`，不能据此再启动一次。原生会话改变后不会把旧恢复摘要关联到新观察。

公共发现不返回启动命令、环境或存储身份。ACP 的 load 支持与 list 支持独立；当前仅在可靠 ID 已入库后显示可恢复。此入口本身不发送 new/load/prompt，也不自动恢复已停止的 Runtime。两产品工作台已经使用此分页入口，不再在浏览器按 Runner 分别执行 runtime.list 或拼接历史索引。单个 Runner 暂时失败保留已有会话，访问被拒绝则断开；原生会话切换只更新 pane 的恢复记录，不重建终端 / ACP 连接。MCP 工具装配仍待接入。
