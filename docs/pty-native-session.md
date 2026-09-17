# PTY 原生会话确认

原生身份与画面活动独立。内部 `__agent-session` 回调只处理 Claude / Codex 的 `SessionStart`，读取官方事件中的 UUID、cwd 和 transcript path，不读取或猜测“最新会话文件”。回调不保存 prompt / transcript path，也不向 Agent 输出事件内容。

Runner 预先创建私有目录和不可替换的 Runtime 绑定；回调从环境变量 `DUNE_AGENT_SESSION_DIR` 找到绑定，事件本身不能指定 Runtime。确认采用本机文件锁和原子替换：同 ID / cwd 的重复通知不推进序号，切换时递增；损坏或错误绑定的记录拒绝覆盖。序号和最后确认保留在 tmux 的运行目录内，fabricd 重启不会重置。最终产品恢复索引仍保存于应用数据库，此文件仅用于本机原生回调与 fabricd 之间传递最后确认。

子 Agent 的 `agent_id`、其他生命周期事件，以及 Codex 中与继承的 `CODEX_THREAD_ID` 不匹配的事件不会修改主会话身份。回调由宿主的 `fabricd.RunHelper` 分派，无需 machine config 或 HTTP 服务。整个子进程最多等待一秒，失败保持静默，避免妨碍 Agent 自己的生命周期；没有确认就不能声明恢复已就绪。

直接以 `argv: ["claude"]` 或 `argv: ["codex"]`（也可为完整可执行文件路径）启动的 PTY Agent 会自动注入本次 SessionStart hook。Claude 使用 `--settings` JSON，Codex 使用 `-c hooks.SessionStart=…`，不修改用户或项目配置；shell 启动、子命令或额外参数暂不适配，以免重写单次任务或自定义配置语义。这里的注入只提供身份采集，PTY MCP 和原生恢复编排另行交付。

Runtime 的私有目录保存 hook helper 的硬链接，跨文件系统时才复制可执行文件。安装器清理旧 release 后，已启动 Agent 的 hook 仍可调用；hook 定义通过环境定位 helper，定义本身不随 Runtime 路径改变。tmux 元数据保存集成标记，fabricd 启动、轮询和 `runtime.get` 读取最后确认，经过共享 Agent Directory 保存到应用数据库。停止 / forget Runtime 清理原生目录，fabricd 重启则保留。

PTY prompt / keys 的 `session_id` 与 `cwd` 固定目标；省略时在 fabricd 受理时绑定当前确认。排队和文本发送后的 Enter 前再次检查，已知会话切换会拒绝旧输入。文本已粘贴后发现会话变化时返回 unknown，不能声称整个请求从未投递。此检查依赖原生回调已报告的状态，不把画面内容当作会话身份。

回归覆盖生成配置、实际回调子进程、fabricd 离线期间切换及重新读取、应用数据库索引 / 历史记录、旧引用失效、队列内目标检查。安装的 Codex 0.140 已接受生成的内联 hook 配置；这只证明配置解析，不能代替真实 Agent 会话 / 任务验收。

合同参考：[Codex hooks](https://developers.openai.com/codex/hooks/)、[herdr Claude integration](https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/integration/assets/claude/herdr-agent-state.sh) 和 [Codex integration](https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/integration/assets/codex/herdr-agent-state.sh)。Codex 的原生 hook 信任流程仍适用，启动适配器不得通过全局 bypass 开启其他尚未信任的 hook。
