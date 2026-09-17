# ACP 操作交互

Dune 与 SandDance 的 ACP 面板使用共享 `agents/prompt`、`agents/open-session`，提交当前发现的 `agent_ref`。prompt 正在执行时，“加入队列”提交新的操作，界面不以会话 idle 推断它已经完成。new/load 也可以排队；permission/cancel 仍直接作用于当前执行。

“本页提交”显示每次操作的独立状态，pending/running 通过 `agents/wait(operation_ref)` 更新。临时查询失败停止该项自动查询，用户可明确重新查询；提交失败如携带操作引用仍保留它，未知结果不重发。确认 new/load 后刷新共享发现，取得当前原生引用。列表有数量上限，记录保存在页面内存，不成为持久任务系统；换 Runtime 会重新建立页面状态，其他分屏连接保持不变。

“查看本次输出”只调用 `agents/read(operation_ref, position)`。第一次从 0 开始，后续使用服务返回的下一位置，不读取其他操作或当前会话快照替代。输出仍使用现有对话和工具渲染；服务报告裁剪、缺口或页面显示缓冲达到上限时显示不完整提示。过期引用明确说明无法查询，不把过期解释为未执行。输出窗口由用户打开，后台状态更新不抢焦点。

当前对话流继续用于实时协作和原生历史。它与“本次输出”分别展示，断线后可以查询本页已知操作的结果，更早的原生历史按 Agent 的 load 能力读取。PTY 继续保留原生终端交互，delivered 和任务完成的区别见 [通信合同](agent-messaging.md)。
