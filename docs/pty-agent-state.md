# PTY Agent 活动

fabricd 识别前台 Claude / Codex 后，每个 Runtime 最多每秒读取一次 tmux 实时画面。两产品的 Agent 列表直接复用 `Runtime.activity`，不需要为后台 Agent 建立完整输出订阅。

`source=screen` 表示根据原生 UI 控件观察；`state` 为 `working`、`idle`、`blocked` 或 `unknown`。前台进程识别本身仍为 `source=foreground`，不推断任务是否结束。暂未适配的 CLI 和无法识别的画面保持 unknown。

检测只读取实时屏幕，不包含 scrollback、ANSI 样式或用户 copy-mode 的位置。Claude 以输入框和底部控件为范围，Codex 以当前 prompt / 最近响应标记为范围，排除历史批准提示和输入草稿。明确的计时 / 中断提示表示工作中，授权 / 问答控件表示需要回应，明确的输入区表示空闲；转录浏览器、未识别菜单和不完整画面不推断为空闲。

自动 prompt 在 fabricd 输入队列内重新采样，明确 blocked 时拒绝，返回 `AGENT_BLOCKED`。用户可通过原生终端或显式 `send_keys` 回应。画面识别不是权限边界，也不能证明特定任务完成；PTY 操作仍只确认字节投递，Agent 等待返回的是活动观察。

规则实现位于 `internal/agentdetect`，采用针对两种 CLI 的小函数，没有引入通用 manifest 解释器。设计参考 [herdr 固定版本的状态说明](https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/docs/next/website/src/content/docs/agents.mdx) 和 [检测规则](https://github.com/herdrdev/herdr/tree/e7e3dfa60e359404def46503dd105c165d02a561/src/detect/manifests)；没有复制其规则库。CLI 界面变化需要重新验证。原生会话 ID、继续能力和 MCP 注入另由启动适配器提供，禁止从画面或“最近会话文件”猜测 ID。

回归覆盖实时屏幕 / 历史分离、状态变化与已读序号、历史提示 / 草稿排除、排队 prompt 的即时 blocked 检查，以及原有 PTY 输入顺序和重连。真实厂商交互仍需单独验收。
