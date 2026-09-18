# Agent 发现

`App.AgentDirectory()` 提供 `List(scope, query)` 和 `Get(scope, agent_ref)`。Scope 来自已认证用户和 Owner / Tenant，引用本身不授予访问权。HTTP 前缀为 `/api/v1` 或 `/api/v1/tenants/{tenant}`。

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| GET | `/agents?limit=32&cursor=…` | 按 Runner 分页，返回 items、runners、issues、next_cursor |
| POST | `/agents/get` | 接收 `{agent_ref}`，验证固定目标并返回当前观察 |

先授权，再查询现有 Gateway 路由的在线状态。最多四个 Runner 并发读取，每个最多五秒，整体二十秒；离线或无 binding 不连接。单 Runner 失败单独进入 issues，不隐藏其他 Agent。ready 是当前启动预检查，实际启动仍再次检查。

Agent 包含不透明引用、完整目标、Runtime 和 Runner。Runtime 提供活动、当前原生会话及项目 / 目录标签；不返回启动命令、环境或凭据。引用固定 binding、Runtime incarnation / generation 和已确认原生 ID / cwd，原生会话改变后旧引用返回 `STALE_SESSION`。

List / Get 只读取 fabricd 状态，不写入会话档案，不调用 new/load/prompt，不恢复或重新启动退出的 Runtime。暂时读取失败保留页面中已打开的连接并显示错误；访问被拒绝则断开。刷新仅重连仍存活且身份一致的实例。新 worktree 的项目标签随 Runtime 保存，其他外部启动的 Agent 可按 binding / cwd 匹配项目目录。
