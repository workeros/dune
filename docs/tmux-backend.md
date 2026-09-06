# PTY 的 tmux 后端

PTY 使用单一、私有的 tmux server。结构为：浏览器 xterm → HTTP/WS Gateway → fabricd → 临时 tmux attach 客户端 → tmux session 中的 shell/Agent。终端屏幕、模式和滚动历史只有 tmux 一份状态。ACP 继续使用 stdio 控制器，不通过 tmux。

## 边界

- `internal/tmux` 封装 create、restore、attach、capture、copy-mode、destroy；不建立通用后端接口或兼容旧宿主协议。
- 每个 session directory 对应一个短路径 Unix socket，位于当前用户专有的 0700 目录；fabricd 文件锁防止同配置双开。不会连接用户默认 tmux server，也不加载用户 tmux 配置。
- 会话名字为随机 Runtime ID。tmux 的 `@dune-runtime` option 保存 Runtime 元数据；新 fabricd 从该私有 server 恢复列表。连接 incarnation 与 runtime incarnation 分别校验。
- 创建命令和环境逐项引用，使用 `env -i`，清除 TMUX/TMUX_PANE；不同会话的环境互不继承。工作目录与有效 PATH 中的命令在创建前检查；后台环境缺少 TERM 时补为 xterm-256color。
- 浏览器连接仅创建 viewer；离开、断开和 fabricd/Gateway 重启不结束 PTY。一个输入 owner，观察者使用只读 attach。
- 原始终端字节以 base64 帧传至 xterm，避免 UTF-8 在网络块边界损坏。resize 作用于 viewer PTY，尺寸再由 tmux 传至 pane。
- 默认历史上限 50,000 行，可配置 1..200,000。采用 tmux 原生批量淘汰；这是上限，并非总能精确保留 50,000 行。历史按钮、鼠标滚轮使用 copy-mode；不传输整份历史、不维护额外历史游标。
- 程序自然退出保留 pane 和退出码，可继续查看历史；显式结束销毁 session、移除 Runtime 及历史。机器或 tmux server 退出不恢复进程/历史。

## 分发

`make build` 生成 `bin/dune` 与 `bin/tmux`。固定 tmux 3.7c 的官方构建和 SHA-256 记录在 `third_party/tmux/manifest.json`；下载器只提取所需可执行文件和授权说明。`make release` 生成 Linux/macOS × amd64/arm64 的 tar.gz，每份含 dune、tmux 和 licenses。安装脚本将它们安装在用户私有目录，不要求目标机器预装 tmux。

运行时首先使用 `DUNE_TMUX`（开发测试覆盖），否则使用 dune 相邻的 tmux，最后才查询 PATH。独立 tmux server 不随用户服务停止而终止；systemd 使用 KillMode=process，launchd 使用 AbandonProcessGroup。

## 参考与验证

参考 [Botmux tmux-backend.ts](https://github.com/deepcoldy/botmux/blob/8d2986c71574bb799c8ffa89cc7631e6d8ddeec2/src/adapters/backend/tmux-backend.ts) 的轻量封装：临时 PTY attach 与持久 session 分离，detach 和显式 destroy 分离。仅借鉴结构，没有引入 Botmux 的应用层、代理或多后端架构。上游二进制来源为 [tmux-builds](https://github.com/tmux/tmux-builds)。

`internal/tmux` 验证环境引用/隔离、Unicode、50,000 行上限和淘汰、自然退出、alternate screen、resize 和重连。`TestTmuxSurvivesFabricdAndGateway` 经完整网络与真实进程验证浏览器断线、fabricd SIGKILL/正常重启和 Gateway 重启后的 Runtime、PID、当前画面及历史。
