# 统一 Agent 启动服务

`host.App.AgentLauncher()` 提供共享的进程内 `agents.Launcher`，两产品 Web 已接入，MCP 随后装配。调用者从认证上下文提供 `agents.Scope`，不能采信模型传入的 Tenant。

`StartRequest` 指定完整 Runner binding、固定 Profile 修订（或自定义 Profile）、可选的项目修订和目录、工作目录，以及可选的新 worktree 路径 / 分支 / ref。未选 Profile 时使用所选项目的默认修订。项目修改冲突、跨 Owner 访问、失效绑定和未就绪环境在创建 worktree 前拒绝。只使用已有 Runner，不触发云环境创建。

启动顺序：

1. 验证访问与 Runner binding，解析固定项目 / Profile 修订和所选目录。
2. 应用宿主提供的 `AgentEnvironment` 默认值，读取 Runner 执行用户与 home，确定实际存储身份。
3. 用户选择 worktree 时，经 SDK / Gateway 创建一次新工作树；不复制未提交内容，不覆盖已有路径 / 分支。
4. 保存实际启动快照和 `starting` attempt，然后经同一 SDK 连接调用一次 `profile.start`。
5. 保存确认的 Runtime。PTY 尚无原生适配器时标记不可恢复；managed ACP 等待初始化（最多 15 秒），提交一次 new 并等待本操作（最多 30 秒），成功后保存原生 ID。pending 仍返回操作引用，不能仅凭进程存活标记可恢复。

`LaunchResult` 返回会话摘要、确认的 Runtime、已创建 worktree，以及 ACP 初始 new 的操作引用。出现错误时也可能有部分结果，调用方必须保留并显示：例如 worktree 创建成功但 Agent 命令不存在，用户可以直接选该目录再次启动；Runtime 已运行但索引写入失败时，应连接返回的 Runtime，不能重新 start。

服务持有宿主请求生命周期，保存已确认结果时给数据库最多三秒的独立上下文，避免浏览器断开立即丢弃已确认 Runtime。网络 / 写入结果未知不自动重发；未取得确认的 attempt 留在索引供后续检查。

恢复适配器目前只识别标准 ACP transport 启动：`<agent> --acp`、`opencode acp`、`gemini --experimental-acp`、无额外参数的 `codex-acp` / `claude-agent-acp`。这表示命令形态可以构造 load，不表示已完成厂商互操作验收。任意 shell 命令或含额外一次性参数的命令不声明可自动恢复。后续恢复仍需确认 Agent 支持 load、原生 ID 有效、程序版本与存储前置条件满足。

新 worktree 的会话保留项目 ID，但不冒充源目录 ID，也不自动修改共享项目目录配置。页面通过会话摘要关联新工作树；需要长期固定时由用户添加项目目录。

## HTTP 与页面

`POST /api/v1/runners/{runner}/sessions?machine_id=…&fabric_id=…&revision=…` 接收 `StartRequest`，URL 固定 Runner binding；正文若提供不同 binding 会被拒绝。不再接收裸 Profile。成功返回 201 和 `LaunchResult`；失败返回 `code`、`error`、`result`，其中 `result` 可以包含已经确认的 worktree、Runtime 或会话索引，调用方不得忽略部分成功并自动重发。

```json
{
  "project": {"id": "project-id", "revision": 2},
  "profile": {"id": "profile-id", "revision": 3},
  "working_directory": "/workspace/source",
  "worktree": {"path": "/workspace/helper", "branch": "feat/helper"}
}
```

省略 `worktree` 使用当前目录。`custom` 可以提供自定义 Profile，与 `profile` 互斥。SandDance 普通终端继续走 Tenant 的 `terminal-sessions` 路由，由后端选择已保存 Shell 配置后转入同一个启动服务；该入口接收目录、项目和 worktree 选择，禁止用户混入 Profile。

两产品的 `GET {prefix}/agent-sessions` 和 `GET {prefix}/agent-sessions/{id}` 返回 Owner / Tenant 内的恢复摘要；列表使用 `limit` / `cursor` 分页，不返回启动命令或环境。工作台把摘要与完整执行身份匹配，将 `session_record_id` 和项目关联保存到 pane，新 worktree 不会在刷新后丢失项目归类。索引读取失败会单独显示错误，不以索引代替 Runtime 存活检查。

页面对部分成功保留现场：已创建 worktree 时切换为该目录；确认 Runtime 已启动时直接打开会话并显示索引错误。超时或断线不自动重发启动。此入口直接采集初始 new 的确认；尚未完成的操作由 wait/read 或 AgentDirectory 采集，不能重复 new 来补索引。初始化或 new 失败仍保留 Runtime，原生确认未取得时保留待采集状态；用户可以在原 Runtime 就绪后明确创建会话，无需再次启动。显式继续见 [恢复索引](agent-recovery.md)。
