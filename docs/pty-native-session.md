# PTY 原生会话确认

原生身份与画面活动独立。内部 `__agent-session` 回调只处理 Claude / Codex 的 `SessionStart`，从官方事件读取 UUID 和 cwd，不猜测最新会话，不保存 prompt 或 transcript 内容。

回调使用本 Runtime 的私有绑定校验来源。子 Agent 的事件、其他生命周期事件以及 Codex 中与 `CODEX_THREAD_ID` 不匹配的事件不会改变主会话身份。确认写入 Runtime 的私有运行目录，供 fabricd 读取；它不建立应用数据库恢复索引。Runtime 的完整引用仍需经过 Gateway 和宿主授权。

直接 `argv: ["claude"]` / `["codex"]` 启动时注入 SessionStart hook，Claude 使用 `--settings`，Codex 使用本次 `-c` 配置，不修改全局或项目配置、不绕过原生信任。MCP 注入见 [MCP 接入](agent-mcp.md)。内部 helper 由 `fabricd.RunHelper` 分派，保留的可执行文件链接使已启动进程不受安装器清理旧 release 影响。

fabricd 启动、轮询和 runtime.get 读取最后确认。连接服务重启时存活 tmux 会话及其元数据保留；stop / forget 清理对应私有目录。工作台不支持进程退出后的原生恢复。

PTY prompt / keys 的 session ID 与 cwd 固定原生目标，省略时在受理时绑定当前确认。出队及粘贴后的 Enter 前再次核验；已知会话变化拒绝旧输入，文本已粘贴后变化则返回 unknown，不把屏幕内容当作身份。

本地回归覆盖生成配置、实际回调子进程、fabricd 离线期间切换与重读、旧引用失效以及队列目标校验。厂商的模型执行与目标部署验证不由这些协议测试替代。
