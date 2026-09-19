# PTY 的 tmux 后端

PTY 使用单一、私有的 tmux server。结构为：浏览器 xterm → HTTP/WS Gateway → fabricd → 临时 tmux attach 客户端 → tmux session 中的 shell/Agent。终端屏幕、模式和滚动历史只有 tmux 一份状态。ACP 继续使用 stdio 控制器，不通过 tmux。

## 边界

- `internal/tmux` 封装 create、restore、attach、capture、scrollback、copy-mode、destroy；不建立通用后端接口或兼容旧宿主协议。
- 每个 session directory 对应一个短路径 Unix socket，位于当前用户专有的 0700 目录；fabricd 文件锁防止同配置双开。不会连接用户默认 tmux server，也不加载用户 tmux 配置。
- 会话名字为随机 Runtime ID。tmux 的 `@dune-runtime` option 保存 Runtime 元数据；新 fabricd 从该私有 server 恢复列表。连接 incarnation 与 runtime incarnation 分别校验。
- 创建命令和环境逐项引用，使用 `env -i`，清除 TMUX/TMUX_PANE；不同会话的环境互不继承。工作目录与有效 PATH 中的命令在创建前检查；后台环境缺少 TERM 时补为 xterm-256color。
- 浏览器连接仅创建 viewer；离开、断开和 fabricd/Gateway 重启不结束 PTY。一个输入 owner，观察者使用只读 attach。
- 原始终端字节以 base64 帧传至 xterm，避免 UTF-8 在网络块边界损坏。resize 作用于 viewer PTY，尺寸再由 tmux 传至 pane。
- 默认历史上限 50,000 行，可配置 1..200,000。采用 tmux 原生批量淘汰；这是上限，并非总能精确保留 50,000 行。`runtime.history` 和远端鼠标滚轮使用 copy-mode；`runtime.scrollback` 提供有界只读快照供客户端本地滚动，不维护额外历史存储或游标。
- 程序自然退出保留 pane 和退出码，可继续查看历史；显式结束销毁 session、移除 Runtime 及历史。机器或 tmux server 退出不恢复进程/历史。

## 只读历史快照

`runtime.scrollback` 是普通 Runtime unary operation，沿用 `runtime.capture`
的目标、连接 incarnation/generation、Runtime incarnation/generation 和宿主访问策略校验。
不要求持有终端输入 owner；有 Runtime 读取权限的观察者也可读取。
请求为 `{"limit":5000}`，省略或 0 使用 5,000 行，允许 1..10,000，越界返回
`INVALID_ARGUMENT`。行数包含历史与当前屏幕，按 tmux 物理行计，包含空白行。

返回字段：

| 字段 | 含义 |
| --- | --- |
| `content` | 最新完整物理行，按时间顺序、LF 分隔且每行保留末尾 LF；无 ANSI、颜色或超链接控制序列，软换行不合并，行末空格由 tmux 去除 |
| `cols` / `rows` | 读取时 pane 的列数 / 屏幕行数 |
| `history_lines` | tmux 当前保留的历史行数，不含屏幕，不是进程累计输出行数 |
| `captured_lines` | 实际返回的完整物理行数，包含屏幕空白行，可小于 `limit` |
| `truncated` | 行数或字节限制丢弃了内容，或 tmux 可能已经淘汰过历史 |

内容最多 512 KiB UTF-8 字节；超限丢弃最旧的完整行，保证返回最新内容和有效
UTF-8。极端情况下单行超过字节上限，该行整体省略。API 常量在 `pkg/api`，
默认 SDK 与协议客户端均提供 `ScrollbackTerminal(ctx, runtime, request)`。
同一 request ID 不缓存大快照，再次使用返回 `RESULT_UNKNOWN`，需发起新的只读请求。

快照仅使用一组原生 `display-message` / `capture-pane` 命令；不会进入或退出
copy-mode、resize、附加 viewer 或改变进程。alternate screen 活跃时返回仍保留的
历史加当前 alternate screen，不包含被替换的主屏幕；不会切换屏幕。程序自然退出、
浏览器重连以及 fabricd/Gateway 重启后仍可读取 tmux 保留的内容。

`truncated` 对原生淘汰采用保守判断：tmux 没有累计淘汰计数，达到 history limit
会批量淘汰最旧的 10%，因此进入 `history_limit - max(1, history_limit / 10)`
区间就标记（阈值至少 1 行），可能在第一次淘汰前提前标记。应用清屏、清除历史或
resize 重排后，tmux 元数据无法证明进程的完整历史；`false` 也不代表完整运行日志。
客户端应将其展示为“仅最近保留内容”，不能承诺被 tmux 淘汰的内容仍可找回。

## tmux 能力查询与输入通道

浏览器 viewer 的输出与 Runtime 常驻 input viewer 是不同的 tmux client。浏览器
收到 DA1 (`ESC [ c`) / DA2 (`ESC [ > c`) 查询后，自动应答如果经普通输入通道
发送，会到达 input viewer。tmux 只消费首次能力应答，重复应答会作为按键进入
子进程，因此重连可能出现 `0;276;0c` 残留。使用 xterm 的宿主应在 parser 层消费
DA1/DA2 查询，避免自动生成输入应答；固定 `xterm-256color` 能力不依赖这些应答，
pane 内应用自身的 DA 查询由 tmux 回答。Dune 的普通输入仍原样传输。

2026-09-19 在本地固定 tmux 3.7c 上，用 `pkg/fabricd/pty_input_test.go` 的
`ptyInputFixture`（raw、关闭 echo 的字节记录进程）复现：经同一 queue 连续调用
两次 `writePTY(viewer, []byte("\x1b[>0;276;0c"))`，再写入 `END`。子进程记录
精确为 `"\x1b[>0;276;0cEND"`：第一次应答被消费，第二次泄漏。检查固定版本
`tty-keys.c` 的 `tty_keys_device_attributes2`，其 `TTY_HAVEDA2` 分支与结果一致。
这项诊断使用私有临时 tmux server，未接触用户终端；修复由 SandDance 的 xterm
parser 完成，不在 Dune 输入层添加 CSI 分流或改变 Escape/滚轮处理。

## PTY 超时

只有设置 `start.timeout_seconds` 的 PTY 会通过 `RunHelper` 启动私有超时 helper。
期限从启动命令开始，setup 不占用它的预算。helper 不依赖 fabricd 的存活管道，
关闭 fabricd 后仍继续计时；Linux 使用 CLOCK_BOOTTIME，macOS 使用
CLOCK_MONOTONIC_RAW，计入休眠且不受日历时间调整影响。机器休眠时不能执行信号，
唤醒后由短周期检查处理到期。

命令持有独立前台进程组，交互 shell 保留自己的作业控制和 Ctrl-C 行为。到期向
受管组与当前前台任务发送 TERM，最多等待 2 秒后 KILL；保留 dead pane 和历史。
helper 不在这个阶段执行 dune 或 tmux，因此删除旧发行目录不会使已有期限失效。
主动脱离受管组的后代进程仍不在清理保证内。

SessionDir 的 `pty-timeouts/` 只保存 helper 写入的启动时间、期限和终止结果，
按 Runtime ID/incarnation 关联；fabricd 只读取。Runtime 的 `started_at`、
`deadline_at` 用于展示，`stop_reason` 区分 `exited` 与 `timed_out`，不会根据特殊
退出码推断超时。意外丢失 helper 结果时报告 `unknown`。显式 stop/forget 销毁
session 并删除该记录，删除目录与 helper 最后的原子写入不会互相重建状态。

## 分发

`make build` 生成 `bin/dune`、`bin/tmux` 与 `bin/rg`。固定 tmux 3.7c 的官方构建和 SHA-256 记录在 `third_party/tmux/manifest.json`；下载器只提取所需可执行文件和授权说明。`make release` 生成 Linux/macOS × amd64/arm64 的 tar.gz，每份含 dune、tmux、rg 和 licenses，详见[发行说明](releases.md)。安装脚本将它们安装在用户私有目录，不要求目标机器预装 tmux。

运行时首先使用 `DUNE_TMUX`（开发测试覆盖），否则使用 dune 相邻的 tmux，最后才查询 PATH。独立 tmux server 不随用户服务停止而终止；systemd 使用 KillMode=process，launchd 使用 AbandonProcessGroup。

## 参考与验证

参考 [Botmux tmux-backend.ts](https://github.com/deepcoldy/botmux/blob/8d2986c71574bb799c8ffa89cc7631e6d8ddeec2/src/adapters/backend/tmux-backend.ts) 的轻量封装：临时 PTY attach 与持久 session 分离，detach 和显式 destroy 分离。仅借鉴结构，没有引入 Botmux 的应用层、代理或多后端架构。上游二进制来源为 [tmux-builds](https://github.com/tmux/tmux-builds)。

`internal/tmux` 验证环境引用/隔离、Unicode、50,000 行上限和淘汰、自然退出、alternate screen、resize、快照行/字节限制和读取无副作用。`TestScrollbackProtocolAndAuthorization` 验证 SDK、权限与 Runtime 句柄。`TestTmuxSurvivesFabricdAndGateway` 经完整网络与真实进程验证浏览器断线、fabricd SIGKILL/正常重启和 Gateway 重启后的 Runtime、PID、当前画面及历史快照。
