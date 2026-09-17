# Agent 恢复索引

数据库基础由 `pkg/agents` 和 `internal/metadata/agent_sessions.go` 提供；原生采集、启动编排和 UI 随后接入。这里不保存 prompt、操作输出或待办任务。

`dune_agent_sessions` 按 Owner / Tenant 保存一份不可变 `launch` 和可修订的 `state`。启动前提交实际 Profile、cwd、原 Runner binding、存储身份、恢复适配器及来源 Profile 修订。读取恢复配置不依赖 Profile 表，Profile 修改或删除不会改变旧会话。恢复适配器负责从原配置构造 resume 命令，不能重复运行 setup / 首条任务。启动后注入的一次性 MCP 凭据不属于快照。

快照使用现有应用数据库的访问保护，与 Profile 存储边界一致：SQLite 私有目录及文件权限，PostgreSQL 受应用数据库账号保护。快照可能包含用户填写的敏感环境或命令，属于私有存储数据，不通过 Agent 发现 / MCP / 会话 JSON 返回，也不能写入日志。`Session.Launch` 明确禁止 JSON 序列化；后续 API 单独构造必要摘要。该实现不提供数据库字段加密或凭据管理系统。

## 状态边界

- 创建记录：`pending_capture`，attempt 为 `starting`；数据库提交失败或未知时不得执行 start。
- 收到 Runtime 启动回执：attempt 为 `capturing`。普通终端可以标为 `unavailable`，保留连接能力。
- 收到可靠原生 ID：核对 attempt 和完整 Runtime 身份，记录采集来源、Agent 版本和恢复能力。只有保存成功才能显示 `available`。
- 原生 ID 不可覆盖；切换到另一原生会话需要新记录。旧 attempt 的迟到回调被拒绝，同一确认可重复保存。
- 执行回执未知：记录 `unknown`，不按超时夺取执行权、不自动重发。原 attempt 的可靠迟到确认可以消除未知状态。

## 显式继续

服务先校验原 Runner / 存储身份和恢复适配器，确认旧 Runtime 以及可能留下的 attempt Runtime 已停止，再用记录修订号调用 `BeginAgentResume`。数据库 CAS 只向一个调用方返回 `claimed=true`；其他 Pod 返回同一 attempt。已完成的重复点击仍返回该 attempt，不再启动。

恢复期间保留原 `last_runtime`；只有新 Runtime 确认加载同一原生 ID 后才替换。新 attempt 的 operation 引用完全独立，旧队列不恢复。已知失败允许用户再次明确发起；未知失败保持屏障。提交回执丢失也不授予启动权，即使另一连接可读到已提交的 attempt。

仅保存最近一次 attempt 的 ID、状态和基准修订号，用于重复点击与迟到回调检查；没有租约、选主、后台任务接管或消息重放。
