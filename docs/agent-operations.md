# fabricd Agent 操作合同

每个 Runtime 的 managed ACP controller 串行执行 `new / load / list / prompt`。Web、IM 和协作 MCP 通过相同的 SDK → Gateway → fabricd 路径提交；宿主不持有消息队列。原始 ACP 透传保持调用方管理协议的入口，不使用本队列。

## 提交与查询

| 执行 API | 请求 | 返回 |
| --- | --- | --- |
| `acp.action` | `action` 为 new / load / list / prompt，其余字段见 `api.ACPAction` | `operation_ref`、`state` |
| `agent.operation.wait` | `operation_ref`、`timeout_ms`（0–30000） | 指定操作的状态、stop reason 和错误；超时返回当前状态，不取消操作 |
| `agent.operation.read` | `operation_ref`、`position`（首次 0）、`limit`（1–128，默认 128） | 状态、stop reason、错误、ACP update 数组、实际 position、next_position、incomplete |

所有请求必须携带完整的 Runner binding 和 Runtime 身份。`operation_ref` 是原 Runtime 在该 fabricd 实例中生成的不透明引用，不能仅凭会话名称或当前 Runtime 替代。

SDK 提供 `ACPSubmit`、`WaitAgentOperation`、`ReadAgentOperation`。`acp.action` 的 permission / cancel 是即时控制，继续返回 `accepted`；控制写入完成前不会启动下一条 prompt。`acp.state` 额外提供当前 `operation_ref` 和 pending 数量，订阅的 `agent_operation` 事件仅用于展示，可靠查询走上述 API。

状态为 pending、running、completed、failed、cancelled 或 unknown。完成以匹配的 ACP JSON-RPC 响应为准。成功的 new/load 另返回 `native_session`（确认的 ID、cwd、Agent 版本、load 能力及 Runtime 内确认序号），供调用方可靠采集恢复索引，避免从可能已切换的会话状态猜测 ID。prompt 的 `session_id` 可显式指定；省略时在受理时固定到当前原生会话。此前排队的 new/load 改变会话后，该 prompt 失败，不会被发送到另一个原生对话。

Runtime get/list 也保留最近一次成功确认的 `native_session`，便于宿主断开后补采集。它表示最后确认的原生会话；当前能否发 prompt 仍按 ACP state 判断。pending / failed 的 new/load 不覆盖这份确认，旧操作中的确认也不随当前会话变化。确认序号只在同一 Runtime 内排序恢复索引的更新，不作为 MCP prompt 输出位置或跨 Runtime 的事件序号；load 能力与 list 能力独立。

## 输出与寿命

- 每个 Runtime 最多 32 个 pending、64 个操作记录。完成记录最多保留 15 分钟；容量不足时淘汰最早完成的记录，不淘汰仍在执行或排队的请求。
- 每个操作最多保留 512 KiB / 1024 个 ACP update。读取位置按本操作的 update 递增，裁剪后不重新编号。读取时已裁剪则返回实际保留起点和 `incomplete=true`；单个超过 512 KiB 的 update 也留下明确缺口。需完整内容时应及时补读，不能将该缓冲当作永久历史。
- 输出在发布浏览器订阅前保存。load 历史重放不会进入 prompt 缓冲。错误会话、无可靠边界或协议解析省略均标记不完整。
- 浏览器 / SDK 断开、宿主 Pod 重启不取消已受理操作。网络回执未知时不自动重放提交。读取和等待可用新请求查询同一个已知操作引用。
- fabricd 重启会清空操作记录和 pending；旧 Runtime 返回 `STALE_RUNTIME`，存活的 tmux Runtime 中旧操作返回 `OPERATION_EXPIRED`。淘汰记录同样返回 `OPERATION_EXPIRED`，不降级读取当前会话内容。
- Agent 退出前未收到匹配响应，正在执行的操作为 unknown、未发送项为 cancelled；已收到匹配响应的结果仍可确认。协议结果不可信时停止继续出队。

IM 入口已改为按操作增量读取和等待，不再把会话 idle 当作某条 prompt 的完成。MCP 工具装配及两产品的操作进度界面属于后续功能。

## PTY 有序投递

`pty.prompt` 接收 `{agent, text}`；`pty.keys` 接收 `{agent, keys}`。SDK 对应 `PTYPrompt` / `PTYSendKeys`，返回同一种操作引用，并通过 `agent.operation.wait` 查询 pending / running / delivered / failed / cancelled / unknown。`delivered` 仅表示文本和 Enter 已按顺序写入 tmux 输入客户端，不表示 Agent 完成任务。PTY 输出仍通过 Runtime 终端快照读取，不做逐 prompt 归属。

fabricd 为每个 PTY Runtime 持有一个共享输入客户端和最多 64 项待输入队列。浏览器键盘、INT / QUIT、resize、历史操作，以及 Agent 投递都经过该队列。浏览器关闭不关闭共享输入客户端；fabricd 关闭时结束客户端但保留 tmux 会话；显式 stop / forget 同时结束输入服务。现有浏览器输入所有权规则保留，Agent API 无需抢占浏览器连接。

普通任务文本使用 bracketed paste，等待 200ms 后发送 Enter，整组写入期间不插入后续人工按键。先检查前台仍是指定 CLI、Runtime 存活且未处于已知 blocked 状态或历史模式，并要求 CLI 已启用 bracketed paste；Enter 前再核验进程与状态。粘贴后失去目标时不发送 Enter，记录 unknown。已有输入框草稿仍按原生 CLI 行为处理，不保存或恢复草稿。

按键支持 Enter、Tab、Escape、Backspace、Delete、方向键、Home / End、PageUp / PageDown、Ctrl+C / D / U / L；每次最多 32 个。普通任务正文允许换行和 Tab，拒绝终端控制字符。当前前台命令可识别 claude / codex / opencode / gemini；这只是投递前置检查，实际厂商 CLI 的身份 hook、权限状态与 MCP 注入仍需相应适配和真实验收。

## 本轮验证边界

controller race 测试覆盖 A/B 分离、load 重放、原生会话变化、队列容量、取消与权限、控制写入边界、立即退出、输出裁剪和引用失效。多进程 SDK / Gateway / fabricd 测试使用可控 ACP Agent，验证两个独立连接统一排队、提交方断线后补读及 Runtime 校验。PTY 使用真实 tmux 和原生字节记录进程，检查粘贴 / Enter / 浏览器按键的顺序、前台 / blocked / 历史拒绝、队列上限，以及 fabricd 重启后 tmux 存活、旧操作引用失效。真实 Agent 和两个宿主 Pod 的验收仍在整体实施清单中，不由这些测试替代。

ACP prompt 的原生目标同时固定 session ID 和 cwd；省略值在入队时从当前会话取得。受理与出队均检查两者，即使 session ID 没变，也拒绝在另一 cwd 执行旧 prompt。
