# Agent 恢复索引

数据库基础由 `pkg/agents` 和 `internal/metadata/agent_sessions.go` 提供；统一启动保存快照，AgentDirectory 采集原生确认。这里不保存 prompt、操作输出或待办任务。

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

## Runtime 与原生会话切换

`dune_agent_runtime_sessions` 是每个完整执行目标的一条数据库索引：原启动记录 / attempt、当前记录和最后确认序号。它不保存消息、不授予执行权。Runtime 回执和初始关联在同一事务提交；两个宿主同时采集时仅锁定这条索引，队列仍在 fabricd。

`ObserveAgentSession` 只接受宿主从 fabricd 取得的确认。首次确认绑定启动记录；后续切换到不同 ID / cwd 时创建独立恢复记录，复用不可变的实际进程配置。再次 load 同一 ID / cwd 复用原记录。迟到确认可以补存历史，但不会倒退当前选择；同一序号对应不同会话时整个事务失败。恢复期间则必须确认原 ID 和 cwd，不能把意外创建的新会话当作恢复成功。

原生 cwd 与进程启动 cwd 分开保存：`native.cwd` 用于 session/load，快照中的工作目录仍用于重新启动进程。摘要显示原生 cwd，二者不同时不冒用原项目目录 ID。`selected` 表示此记录是数据库中关联到 Runtime 的当前选择，不表示 Runtime 在线。两产品发现列表只使用 selected 记录关联项目；历史记录仍可用于显式继续。

本层提供 `RuntimeAgentSession` 查询与 `ObserveAgentSession` 采集方法。[AgentDirectory](agent-directory.md) 的 List / Get 已自动采集 fabricd 原生确认；两产品工作台同步最新 selected 恢复记录到个人布局，并在索引暂不可用时保留运行中的 Agent。`App.AgentRestorer()` 已实现 managed ACP 的显式继续；页面的“继续”入口和 PTY 原生恢复仍待接入。

## 原生 ACP 继续服务

`App.AgentRestorer().Resume(scope, {session_record_id, revision})` 使用已认证 Scope，HTTP 为 `POST /api/v1[/tenants/{tenant}]/agent-sessions/{session}/resume`，body 为 `{revision}`。返回 `session`、已确认的 `runtime` 和可选 `operation`；错误响应也在 `result` 中保留部分进度。

服务先检查 Owner / Tenant、原 Runner binding、旧 Runtime 与遗留 attempt Runtime 已退出、执行用户与 HOME 存储身份，再申请数据库恢复 attempt。首版支持 acp-load v1 的标准 transport 启动，去掉原 setup，原样保留实际命令、环境、进程 cwd 与其他启动选项；不再解析来源 Profile 或宿主环境默认值。已有 Runtime 存活时返回 RUNTIME_ALIVE，可直接重新连接。

恢复进程必须就绪并广告 load 能力；list 能力完全独立，不请求 list，也不会退回 new。双方都有版本信息而版本不同时明确拒绝，避免默默跨版本恢复。原生 cwd 与进程 cwd 分开传入。只有匹配 load RPC 的完成确认包含原 ID / cwd，且数据库确认保存后，attempt 才变为 ready。

同一基准修订的重复请求返回同一 attempt，包括它已经完成或失败之后。新的显式重试须使用最新修订，且上次为已知失败、Runtime 已停止。已知初始化 / load 失败时结束本次新进程；未知执行结果不自动清理或重放。后续可靠的 Runtime / 操作确认可以补齐未知结果。整个恢复最多等待 90 秒，其中初始化最多 15 秒，等待本身不赋予接替权。

成功恢复后，旧 Runtime 即使仍在发现列表中，其迟到原生观察也不重新占用恢复记录，不显示为数据库故障。若恢复后的 Runtime 又被显式切换到另一原生会话，重复点击原恢复请求返回 STALE_SESSION，不能把另一个对话当成原会话的继续。

本地 Gateway / fabricd 验证覆盖修改并删除来源 Profile 后恢复实际快照、setup 只运行一次、原生与进程 cwd 分离、load=true/list=false、并发请求单次启动、已知失败后显式重试、丢失响应保留 unknown 屏障、缺失 load 能力及版本变化拒绝。测试使用可控协议进程；厂商 Agent 原生文件恢复与多 Pod 故障仍需整体验收。
