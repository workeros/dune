# 统一 Agent 启动服务

`host.App.AgentLauncher()` 提供共享的进程内 `agents.Launcher`，供 Web 和 MCP 装配使用。该服务已实现；页面和 MCP 接入单独交付。调用者从认证上下文提供 `agents.Scope`，不能采信模型传入的 Tenant。

`StartRequest` 指定完整 Runner binding、固定 Profile 修订（或自定义 Profile）、可选的项目修订和目录、工作目录，以及可选的新 worktree 路径 / 分支 / ref。未选 Profile 时使用所选项目的默认修订。项目修改冲突、跨 Owner 访问、失效绑定和未就绪环境在创建 worktree 前拒绝。只使用已有 Runner，不触发云环境创建。

启动顺序：

1. 验证访问与 Runner binding，解析固定项目 / Profile 修订和所选目录。
2. 应用宿主提供的 `AgentEnvironment` 默认值，读取 Runner 执行用户与 home，确定实际存储身份。
3. 用户选择 worktree 时，经 SDK / Gateway 创建一次新工作树；不复制未提交内容，不覆盖已有路径 / 分支。
4. 保存实际启动快照和 `starting` attempt，然后经同一 SDK 连接调用一次 `profile.start`。
5. 保存确认的 Runtime。PTY 尚无原生适配器时标记不可恢复；ACP 等后续 new / load 确认 ID，不能仅凭进程存活标记可恢复。

`LaunchResult` 返回会话摘要、确认的 Runtime 和已创建 worktree。出现错误时也可能有部分结果，调用方必须保留并显示：例如 worktree 创建成功但 Agent 命令不存在，用户可以直接选该目录再次启动；Runtime 已运行但索引写入失败时，应连接返回的 Runtime，不能重新 start。

服务持有宿主请求生命周期，保存已确认结果时给数据库最多三秒的独立上下文，避免浏览器断开立即丢弃已确认 Runtime。网络 / 写入结果未知不自动重发；未取得确认的 attempt 留在索引供后续检查。

恢复适配器目前只识别标准 ACP transport 启动：`<agent> --acp`、`opencode acp`、`gemini --experimental-acp`、无额外参数的 `codex-acp` / `claude-agent-acp`。这表示命令形态可以构造 load，不表示已完成厂商互操作验收。任意 shell 命令或含额外一次性参数的命令不声明可自动恢复。后续恢复仍需确认 Agent 支持 load、原生 ID 有效、程序版本与存储前置条件满足。

新 worktree 的会话保留项目 ID，但不冒充源目录 ID，也不自动修改共享项目目录配置。页面通过会话摘要关联新工作树；需要长期固定时由用户添加项目目录。
