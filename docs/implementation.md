# Dune 单机实现说明

本文件固定 `dune-mvp/1` 的工程语义。实现以 `pkg/api/types.go`、`proto/dune/dtp/v1/message.proto` 为准；不与早期 DTP Frame 兼容。

适用范围：下文记录单机 MVP；后续 tmux PTY 后端、Web ACP 控制器和账号层的扩展见 [Web 方案](personal-web-plan.md)。涉及生命周期和历史能力时按对应入口区分，不能把 MVP 限制套用到 Web 扩展。

## 网络和协议

Gateway HTTP 服务使用 fasthttp，WebSocket 升级使用 fasthttp/websocket。默认使用 HTTP/WebSocket（ws），支持配置 0.0.0.0 对外监听，dev token 通过 Authorization Bearer 头传递；空/错误 token 在升级前拒绝。fabricd/SDK 都是 Yamux client，Gateway 为 server。WebSocket binary 消息适配为字节流，不依赖 WS 消息边界。

每个 Yamux stream 的应用消息为 **4 字节 big-endian uint32 长度 + protobuf Message**。读取长度后先验证 `1..1MiB`，再分配缓冲。protobuf 承载上下文、操作、消息类型、错误和原始数据；`payload` 是 API Go 类型定义的 UTF-8 JSON。这是明确固定的混合编码，不使用旧协议的 `stream_id/seq/ack_seq`。

第一条 stream 必须在 5s 内发送 hello（版本、角色、target）。fabricd 额外提交随机 incarnation、递增 connection generation 和能力清单；SDK 收到当前 daemon 的绑定。之后每条业务 stream 的首消息是 request，含 request_id、operation、target、incarnation、connection_generation；Runtime 操作还需 runtime_id、runtime_incarnation 和 runtime_generation。Gateway 和 daemon 均校验绑定。连接重新建立需新的 SDK Client；旧 stream 不恢复。

业务流返回 `accepted` 后才执行已受理操作；`result` 或 `exit` 才是明确完成。参数校验可能在 admission 后失败，错误码会明确返回。profile.start 依次发送 accepted、setup progress、Runtime result，随后成为交互 stream。attach/ports.connect 发送 accepted 后进入交互。输入带独立 request_id，`written` 表示 OS 接受写入或控制操作；并不表示 Agent 完成任务。

Yamux 0.1.2 没有单独的 CloseWrite API。Ports 在应用协议发送 `eof`，daemon 调用 TCP CloseWrite，两方向都完成后返回 result。Yamux Close 用于取消/结束 stream。其他业务的 EOF 若未见 result/exit，SDK 返回 STREAM_INTERRUPTED；已提交 unary 调用丢失结果时为 RESULT_UNKNOWN，不自动重试。断开已受理的 Exec/Git 不回滚，也不保证立即取消；其超时仍有效。交互订阅断开不会停止 Agent。

## 机器配置和 Profile

配置字段：`gateway`、`listen`、`token`、`target`、`log_level`；仅显式 WSS 配置使用 `certificate` 和 `key`。SDK/CLI 客户端无需 `listen` 或私钥。log_level 目前保存设置，日志使用 Go 标准 logger，只有运行信息/错误；不记录 token 或输入内容。默认配置路径见 README。默认 ws 不配置 TLS，HTTP Upgrade 请求必须通过 Bearer token 鉴权。listen 可使用通配 IP，gateway 必须为具体 IP/DNS 地址。为兼容原有配置，显式选择 wss 时仍验证证书，init 会把连接地址加入证书 SAN。

Profile 要求 `version: 1`、`kind: agent`、绝对 `working_directory`、`adapter: pty|acp`。`env` 合并当前 OS 环境；`setup.steps` 至多 64 步。Command 必须且只能提供 argv 或 run：argv 不展开 shell；run 必须同时给绝对 shell 路径（例如 `/bin/sh`）。`timeout_seconds` 范围 0..86400；setup/Exec 的 0 默认 300s，start 的 0 表示 Runtime 无时间限制。步骤顺序执行，失败返回步骤编号、名称、退出状态与有界输出，之后步骤不执行。没有自动回滚。

## Runtime 与进程

Runtime ID、incarnation 随机生成，generation 为 1；本版不提供 Runtime restart/replacement，因此不存在复用 ID 的路径。最多 64 条 Runtime；达到上限明确拒绝。PTY 元数据随 tmux 会话跨 fabricd 重启恢复；ACP 状态随 fabricd 生命周期结束。

ACP/Exec 使用短生命周期 guardian，daemon 所有权管道断开时清理普通进程组。PTY 已改用私有 tmux server；它独立于 fabricd 生命周期，详见 [tmux 后端](tmux-backend.md)。Runtime 元数据与终端历史由开发机 tmux 保存，不根据裸 PID 接管进程。

PTY 支持原始 bytes、resize（1..200 行、1..400 列）、INT/QUIT 输入及 TERM/HUP 显式销毁。一个输入 owner，其他连接为只读 tmux viewer。重新 attach 恢复当前画面，tmux copy-mode 浏览有界历史；`runtime.capture` 仅提供诊断快照，网页不使用快照绘制。

ACP stdout 逐行验证 JSON-RPC 2.0 对象、字符串/数字 ID、request/notification/response envelope；unknown method 原样传递。stderr 单独发送。单行 stdout 最大 256KiB，单次输入最大 32KiB 且只有一条 JSON 消息；无效 stdout 终止 Runtime 并报告 INVALID_ACP，无效输入关闭该订阅并报告 INPUT_FAILED。daemon 不解释 ACP 会话、权限或任务完成。mock 示例中的会话/权限逻辑属于示例 Agent，不属于 daemon。

## 资源与背压

| 资源 | 限制 |
| --- | --- |
| Gateway 会话（含待握手） | 16 |
| 每会话待请求/活跃业务 stream | 64；首条请求超时 5s |
| Gateway 全局活跃转发 | 128 |
| daemon 活跃处理器 | 每条有效 tunnel 64 |
| Yamux 单流接收窗口 | 256KiB |
| protobuf 单消息 | 1MiB |
| PTY/Ports/upload chunk | 32KiB |
| 单 Runtime 订阅 | 8；每订阅 16 条在途消息 |
| upload/Ports bulk 并发 | 4（Ports 持续占一个） |
| Runtime / upload 内存记录 | 各 64 |
| unary 结果缓存 | 256 条，至多 60s，先到限制先淘汰 |
| Exec/Git stdout/stderr | 每项 128KiB，超出持续排空并标记 truncated |

Gateway 两方向逐条读取、转发，无无界队列。protobuf 写入按 stream 加锁，写超时 5s。订阅队列满允许至多 5s 等待，之后关闭并返回 SLOW_CONSUMER；无法再写错误帧时客户端得到 STREAM_INTERRUPTED。队列只存在于当前订阅，不是历史缓冲。

这些上限共同约束并发缓冲总量；没有按文件大小缓存整份上传。包括 Yamux 窗口的最大内存随已限制的 session/stream 数有上界，但不是操作系统 RSS 硬配额。并发 e2e 使用 2 PTY + 2 ACP + 16MiB 上传 + 4MiB TCP，p95 验收阈值为 2s。不承诺协议级优先级或物理 TCP 故障隔离。

request_id 由 SDK 随机生成。`CallID` 可显式复用 ID：缓存有效时同输入返回同结果，不同输入返回 IDEMPOTENCY_CONFLICT，尚在运行返回 RESULT_UNKNOWN。超过 60s 或 256 条缓存淘汰后，不承诺去重。Profile stream 不能回放，重复已受理 ID 报告未知且不重跑（缓存保留期间）；调用方查询 Runtime。不会自动重放输入或因服务重启重跑 Profile。

## Files 和完整上传

所有路径按当前 OS 用户实际访问，不做 workspace root 检查。

| Files action | 参数/结果 |
| --- | --- |
| stat | path → name/size/mode/is_dir |
| list | path → 至多 4096 项；超限报错 |
| read | path、offset≥0、length 1..32768 → data、下一 offset |
| write | path、data≤32KiB、overwrite；临时文件 + 原子提交 |
| mkdir | path；创建父级 |
| rename | path、destination、overwrite=true；POSIX rename 语义 |
| remove | path、recursive=false；true 时递归删除 |

写入的临时文件默认 0600。不覆盖提交使用同文件系统 hard link + unlink，目标已存在时报错；显式覆盖使用 rename。不会先删除已有目标。

Upload action 都使用结构化 `api.Upload`：

1. create：path、size（0..10GiB）、小写 SHA256、overwrite、ttl_seconds（默认 1800，1..86400），返回 ID/incarnation/offset/size/expires_at。
2. chunk：ID、精确 offset、1..32768 字节 data，可带 chunk_sha256。offset 不一致为 OFFSET_CONFLICT；chunk hash 失败不改变 offset。磁盘部分写失败需 query 实际 offset。
3. query：返回当前 offset；仅同 incarnation 内有效。网络断线后由调用方显式 query/chunk 续传。
4. commit：必须完整 size，流式校验整文件 SHA256、sync 后原子提交。失败不宣告完成，hash 错误需 cancel 后重新创建。成功重复 commit 返回 committed=true（在句柄 TTL 内）。
5. cancel/TTL：关闭并移除临时文件。cancel 后句柄失效，至下一轮 1s 清理释放元数据。

fabricd 的一个小型 cleanup 子进程仅在内存记录其创建的上传临时路径，所有权管道断开后删除残留，覆盖 daemon SIGKILL。它不建立持久台账、不扫描其他目录。整机断电或同时杀掉 cleanup 进程不在此清理保证内。

## Git

使用本机 Git 和当前用户凭证环境，所有 Dune Git 调用串行，避免同 daemon 内并发修改 index。外部编辑器仍可修改同一仓库；Git 锁和冲突错误原样保留。禁止交互凭证提示/askpass，SSH BatchMode，120s 超时；使用已经配置的 helper/agent。失败返回 exit_code、stderr；CLI 返回非零状态。传输丢失时不自动重试写操作。

| action | 参数 |
| --- | --- |
| status | directory；返回 porcelain 原文及解析后的 entries |
| diff | directory、可选 ref/paths/staged |
| log | directory、可选 ref/limit（默认50，最大1000） |
| show | directory、可选 ref |
| stage / unstage / discard | paths 或 patch 二选一；discard 作用于工作区 |
| commit / amend | message；amend 可空 message 保留原文 |
| branch | 无 name 时列分支；name 创建，可带起点 ref |
| checkout | ref；或 create=true + name，可带起点 ref |
| stash | mode=push/list/apply/pop/drop，可带 message/ref/paths（按操作） |
| fetch / pull / push | 可选 remote/ref；pull 使用 merge、不开编辑器 |
| merge / rebase | mode=start + ref，或 mode=continue/abort |
| conflicts | 返回冲突文件名列表 |

hunk 使用完整 Git patch，先 `git apply --check` 再应用；stage 使用 cached，unstage 使用 cached/reverse，discard 使用 reverse。过期 patch 返回 Git 错误。引用/remote/name 不允许以 `-` 开头；文件参数用 `--` 分隔。没有任意 Git shell 字符串接口。

## 验证与后续边界

`make test` 用独立 supervisor/Gateway/fabricd 子进程以及本机真实 Files/Git/TCP，服务二进制开启 Go race 检查。mock ACP 可用 `go build -o /tmp/dune-mock-acp ./samples/mock-acp` 后，通过 `samples/mock-acp.yaml` 启动；初始化示例在 `samples/acp-initialize.json`。

真实 Agent 账号验证与自动化 fixture 分开记录；用户已明确真实 ACP 任务可在后续环境执行。本版不宣称 tus HTTP、ACP HTTP、多用户隔离或完整 DTP/1 兼容。
