# PTY 原生会话确认

原生身份与画面活动独立。内部 `__agent-session` 回调只处理 Claude / Codex 的 `SessionStart`，读取官方事件中的 UUID、cwd 和 transcript path，不读取或猜测“最新会话文件”。回调不保存 prompt / transcript path，也不向 Agent 输出事件内容。

Runner 预先创建私有目录和不可替换的 Runtime 绑定；回调从环境变量 `DUNE_AGENT_SESSION_DIR` 找到绑定，事件本身不能指定 Runtime。确认采用本机文件锁和原子替换：同 ID / cwd 的重复通知不推进序号，切换时递增；损坏或错误绑定的记录拒绝覆盖。序号和最后确认保留在 tmux 的运行目录内，fabricd 重启不会重置。最终产品恢复索引仍保存于应用数据库，此文件仅用于本机原生回调与 fabricd 之间传递最后确认。

子 Agent 的 `agent_id`、其他生命周期事件，以及 Codex 中与继承的 `CODEX_THREAD_ID` 不匹配的事件不会修改主会话身份。回调由宿主的 `fabricd.RunHelper` 分派，无需 machine config 或 HTTP 服务。整个子进程最多等待一秒，失败保持静默，避免妨碍 Agent 自己的生命周期；没有确认就不能声明恢复已就绪。

当前提交提供确认收集器与进程入口。启动参数注入、tmux Runtime 摘要采集及 PTY 恢复编排仍需后续接入，不能以收集器测试代替真实 Agent 验收。

合同参考：[Codex hooks](https://developers.openai.com/codex/hooks/)、[herdr Claude integration](https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/integration/assets/claude/herdr-agent-state.sh) 和 [Codex integration](https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/integration/assets/codex/herdr-agent-state.sh)。Codex 的原生 hook 信任流程仍适用，启动适配器不得通过全局 bypass 开启其他尚未信任的 hook。
