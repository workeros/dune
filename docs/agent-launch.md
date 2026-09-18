# 统一 Agent 启动服务

`host.App.AgentLauncher()` 提供共享的 `agents.Launcher`，供 Dune、SandDance 工作台及 MCP 使用。调用者从认证上下文提供 `agents.Scope`。仅选择已有就绪 Runner，不创建云环境。

`StartRequest` 指定完整 Runner binding、固定 Profile 修订或自定义 Profile、可选的项目修订和目录、cwd，以及可选的新 worktree 路径 / 分支 / ref。未选 Profile 时使用所选项目的默认修订；项目、配置、权限或绑定错误在修改工作树前拒绝。

启动顺序：

1. 校验访问与完整 Runner binding，解析项目、目录和固定 Profile 修订。
2. 合并宿主 `AgentEnvironment` 默认值，验证最终配置。
3. 如选 worktree，经 SDK / Gateway 创建一次工作树，不复制未提交内容，不覆盖路径或分支。
4. 经同一连接发送一次 `profile.start`，保存返回值中的 Runtime；不写数据库恢复档案。
5. 对受支持的 PTY 签发并注入 MCP 凭据。managed ACP 最多等待初始化 15 秒，再签发 MCP 凭据、配置 fabricd、提交初始 new 并最多等待该操作 30 秒。

`LaunchResult` 包含已确认的 Runtime、`agent_ref`、worktree 和 ACP 初始操作。失败也保留部分结果：工作树已创建时可由用户选该目录再次启动；Runtime 已启动而 MCP 配置或 new 失败时直接打开原 Runtime。pending 可按操作引用等待；未知结果不自动重发，需先发现当前 Runtime / 查询原操作。

项目 ID、目录 ID 作为非敏感标签随当前 Runtime 返回，由 fabricd 保留。标签不授予访问权；宿主覆盖 Profile 中的标签，仅采用已验证的项目选择。worktree 保留项目 ID，清空源目录 ID，不修改共享项目目录配置。重连存活 tmux Runtime 时标签仍在；不保存启动快照供进程退出后恢复。

## HTTP 与页面

`POST /api/v1/runners/{runner}/sessions?machine_id=…&fabric_id=…&revision=…` 接收 `StartRequest`，正文不能覆盖 URL 的绑定。成功返回 201 和 `LaunchResult`；失败返回 `code`、`error`、`result`。

```json
{
  "project": {"id": "project-id", "revision": 2},
  "profile": {"id": "profile-id", "revision": 3},
  "working_directory": "/workspace/source",
  "worktree": {"path": "/workspace/helper", "branch": "feat/helper"}
}
```

省略 `worktree` 使用当前目录。`custom` 与 `profile` 互斥。SandDance 普通终端由 `terminal-sessions` 选择已保存的 Shell 配置后使用同一启动服务，禁止正文混入 Profile。

工作台刷新通过共享 AgentDirectory 重连存活实例，进程退出或 Runtime 已消失显示结束，可显式新建 Agent；没有 `/agent-sessions` 列表、详情或 resume 接口。MCP 凭据不进入 Profile、公共结果或日志。当前范围见 [存储精简方案](herdr-simplification.md)。
