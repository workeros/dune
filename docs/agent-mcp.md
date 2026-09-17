# Tenant Agent MCP 接入

Dune 宿主在部署前缀下提供 `POST /api/v1/agent-mcp`，SandDance 通过现有 Dune 路由挂载同一入口。协议使用官方 Go MCP SDK 的 Streamable HTTP、stateless 和 JSON response 模式；宿主不持有 MCP 连接状态、任务队列或输出副本。每次请求都重新认证，协议 session ID 不授予权限。

仅接受 `Authorization: Bearer <本次 Agent 凭据>`，拒绝 URL query 中的凭据、浏览器 cookie 替代认证及跨站 Origin。凭据从数据库取得调用方身份、Tenant 与启动 attempt，并经 SDK / Gateway 验证原 Runner binding 和 Runtime 仍在运行。身份命名空间变化、撤销、到期、调用 Runtime 退出均拒绝继续使用；认证依赖临时不可用时返回 503，不误报为已执行。

## 工具

| 工具 | 行为 |
| --- | --- |
| `runners_list` | 返回 Tenant 内已有 Runner、就绪状态、部分失败和分页游标 |
| `profiles_list` | 只返回 Agent Profile 的名称、描述、固定修订与 PTY/ACP 类型；分页，不返回命令、环境或凭据 |
| `agents_list` / `agents_get` | 共享 AgentDirectory 摘要与明确引用 |
| `agents_start` | 选择已就绪 Runner、固定 Profile 或项目默认值、当前目录 / 新 worktree；返回 `agent_ref`、Runtime、恢复摘要和 ACP 初始操作 |
| `agents_prompt` | 返回本次操作引用，可选等待；PTY delivered 仅证明投递 |
| `agents_wait` | 按 operation_ref 等待该操作，或明确按 agent_ref 观察活动；两者互斥 |
| `agents_read` | ACP 按操作位置读有界输出；PTY 读取当前会话屏幕 |
| `agents_send_keys` | 显式原生 PTY 按键；可能确认当前交互，需要调用方明确选择 |

输入 schema 不接受 Tenant、Owner 或调用身份。创建助手只使用已保存的 Profile，不向模型暴露任意自定义启动配置表面。列表页默认 32、最多 100；等待最多 30 秒；HTTP 请求体最多 256 KiB。工具描述说明 pending、PTY delivered、输出缺口和未知结果的区别。

工具业务失败用 `isError=true` 返回，structuredContent 保留 `code`、说明和部分 `result`。已受理操作引用、已创建 worktree 和已启动 Runtime 不因后续等待失败而丢失。没有执行重试、任务重放或跨 Pod 去重队列；后续使用原 operation_ref 查询原 fabricd。底层未分类错误不原样输出私有连接或配置内容。

## 已验证与后续接入

真实 HTTP MCP 客户端配合本地 Gateway / fabricd 协议进程已验证：九项工具 schema、Profile 分页与 Tenant 过滤、启动 ACP 后直接提交任务、操作 wait/read、轮流进入两个独立 HTTP handler、撤销后原连接被拒绝、调用 Runtime 退出、cookie / query / Origin 拒绝以及未知结果保留引用且不重发。它证明协议与本地执行链路，不代表两个实际 Pod 或厂商 Agent 已验收。

managed ACP 从工作台或 MCP 启动时，先保存 Runtime 回执，再签发凭据并配置 fabricd，最后提交初始 new；显式恢复在 load 前签发新凭据。两产品复用同一宿主装配，endpoint 来自部署 PublicURL，保留部署路径前缀。普通终端内手动启动与原始 ACP 透传不自动改造；受支持 PTY 的原生注入继续接入。

原生会话切换不轮换调用进程的凭据；显式恢复启动新的进程时轮换，旧 attempt 的凭据失效。凭据和 endpoint 不进入保存的 Profile 或恢复配置快照，也不返回给页面。恢复使用原配置快照，当前宿主的 MCP 地址与本次凭据作为运行注入单独处理。

配置失败保留已确认的 Runtime，未提交 native new/load 时不假称已有原生会话。厂商连接 MCP 失败时依其 ACP 响应返回失败或状态，Runtime 在线不等于 MCP 工具就绪；不会因配置或 new 失败而重启/重发。真实本地 ACP 协议进程已在 HTTP 与 stdio 两条注入路径中完成 `agents_list`，验证凭据先绑定 Runtime 再使用、原生切换与恢复轮换、不可达时保留部分结果。仍需验证实际厂商客户端及目标部署网络。

ACP inspector 已隐藏所有结构化 `mcpServers` 配置，包含 HTTP URL/header 与 stdio argv/env；发给 Agent 的真实 RPC 保持原样。fabricd 也会在接收 ACP JSON 时遮盖本次生成凭据的原文，覆盖操作输出、权限参数和错误信息；stderr 按字节流处理，跨读取边界的完整凭据仍会被遮盖。此保护不识别任意编码或拆成多条协议消息的变形回显；实际厂商日志另需验收。

## 原生 stdio bridge

只支持 stdio MCP 的 Agent 可由启动适配器调用当前 Dune 程序的内部 `__agent-mcp` 入口。它在常规机器配置解析前启动，只从 `DUNE_AGENT_MCP_URL` / `DUNE_AGENT_MCP_TOKEN` 环境变量取配置，stdout 专用于 MCP。它不提供协作 CLI，也不持有队列或业务状态；工具 schema、说明和调用结果来自宿主 HTTP MCP。

bridge 固定目标 URL，不接受 userinfo、query 或 fragment，不跟随 HTTP 重定向，凭据只发往配置的 endpoint。初始化和目录读取有超时，工具列表与输入帧有上限；失去调用回执时返回 `RESULT_UNKNOWN`，不重连重放。stdio EOF 或取消会释放上游连接。真实进程、HTTP MCP 与官方客户端测试覆盖了这些边界；厂商 Agent 的启动注入另行验收。

## fabricd managed ACP 配置

SDK `ConfigureAgentMCP` 经 Gateway 调用 `acp.mcp.configure`，参数仅含宿主 endpoint 与本次凭据。fabricd 在 ACP initialize 确认后、首次原生会话之前接受一次配置；后续替换或并发第二次配置返回冲突。`agentCapabilities.mcpCapabilities.http` 为 true 时生成 HTTP 配置，否则使用当前 Runner 程序的内部 stdio bridge；宿主不指定 Runner 的 executable 路径。

Profile 的 `require_agent_mcp` 只适用于 managed ACP。设置后，未配置 MCP 的原生动作不会受理，因此 Runtime 出现在列表与宿主完成注入之间的竞态不会创建缺少 MCP 的会话。配置保存在 controller 内存，后续所有 new/load（含 Web 通用动作入口）复用；公开 state 只返回 `mcp_transport`。退出 Runtime 或 fabricd 后不恢复凭据配置。原始 ACP 透传仍由调用方自行提供配置。
