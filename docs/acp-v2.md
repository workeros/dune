# Managed ACP v2 草案

默认使用 ACP v1。在 managed ACP Agent Profile 中显式设置 `acp_v2_draft: true`，初始化才请求 v2。首次协商允许 Agent 选择仍受支持的 v1；同一 Runtime 的后续隔离连接不能改变已协商版本。

协议基线固定为 [agent-client-protocol `b9d6aca6757d0f5b6e435cad54f9f04657aa9802` 的 schema/v2/schema.json](https://github.com/agentclientprotocol/agent-client-protocol/blob/b9d6aca6757d0f5b6e435cad54f9f04657aa9802/schema/v2/schema.json)。2026-09-22 读取的原文件共 289442 bytes，SHA-256 为 `3df13661962bf9ed3162a3e50d75fab3d768247995bfb1754b0d844ae139ce4c`。这是 v2 草案中的基线，未启用 unstable schema。网站协议说明用于解释生命周期，固定 schema 用于核对字段。

## 当前行为

- 初始化按版本解析 `info`/`capabilities`，v2 session 对象声明基础会话方法；媒体与 MCP 扩展只认各自的对象能力。stdio MCP 在 v2 中带 `type: stdio`。
- Managed 队列仍串行。v2 prompt 的 messageId 只确认插入；同时收到插入确认和结束前台工作的 idle 才完成本地 operation、释放后续请求。响应丢失或非法结果保留 unknown，不重放。
- 用户消息通过 RPC 对应的 messageId 关联，原生更新可以先于回应。相同文本仍是不同提交。确认后尚未收到内容时保留空占位，不复制本地输入导致后续 chunk 重复。
- 普通消息、思考与工具按会话内 ID upsert。完整内容替换、null 清除、省略不变、chunk 追加。工具 ID 不以工作段作为身份。前台 running/requires_action 期间的首次对象保留观测工作段；已有对象更新保留原位置。idle 期间的新活动不归到刚结束的任务。
- 前台结束不改写仍运行的工具。取消写入只表示请求已发送，最终取消等 idle/cancelled。现有任务恢复或无 prompt 的前台工作有独立 operation 和预留取消身份。
- v2 配置读取 configId，设置 select 使用 type:id，boolean 使用 type:boolean。配置回应完整替换。v2 不调用 modes 方法。
- 计划按 planId 完整替换，未知计划类型原样保留。权限申请标题与工具标题独立，支持 command、tool_call 及无主体；会话内背景审批不依赖 prompt RPC 的存活。
- `resume` 是独立宿主 action。默认不请求历史；调用方显式传 `replay:true` 才发送 `replayFrom:{type:start}`。打开仍创建隔离连接及新的 conversation generation。浏览器重连只读取现有模型，不能触发 resume。回放缺失不能用于判定某次未知插入失败。

## 验证与剩余范围

`TestACPV2*` 覆盖协商、不同消息/回应/idle 顺序、相同文本提交、内容替换与清空、后续工具输出、审批/取消、显式恢复/回放以及未知插入。现有 v1 队列、历史与提交回归继续运行。SandDance 通过宿主读模型展示，并有对应浏览器交互用例。

只读终端字节流和跨仓库发布接入仍在后续切片。没有配置真实 Agent 的环境，协议 fixture 不计为真实 Agent 验收。
