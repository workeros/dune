# Dune 单机 MVP 实现范围

日期：2026-09-05。

适用范围：本文保留单机 MVP 的阶段约束。后续 [个人 Web 方案](personal-web-plan.md) 已扩展 UI、托管会话和 PTY 历史；本阶段的排除项不构成这些功能的禁止规则。

状态：单机实现已交付，工程语义见 [实现说明](implementation.md)，验证入口见 [开发流程](workflow.md)。本文保留原始产品范围；Gateway 支持 0.0.0.0，远端启动后本机直连，使用 HTTP/WebSocket token 鉴权而非 TLS。

关联：[完整设计规范](spec.md)、[DTP 传输协议](runner-tunnel-protocol.md)。

本文定义单机 MVP 的交付范围。与完整规范不同的裁剪集中记录在第 9 节，仅适用于本 MVP；不能据此宣称完整 DTP/1 或多用户隔离能力已实现。未明确裁剪的接口语义参考完整规范，具体 schema 以实现时提交的协议定义为准。

## 1. 目标与完成条件

在一台个人电脑上，以当前 OS 用户运行 Dune。用户通过 CLI 或 Go SDK，经 Gateway 调用 daemon，提交完整 Profile，启动并交互控制真实 Agent，同时操作文件、Git 和本机端口。

MVP 完成必须同时满足：

1. `dune` 拉起 Gateway 和 daemon，形成真实网络通信链路。
2. Profile 能执行准备步骤，启动 PTY 或 ACP Agent，并支持交互、查询和停止。
3. Files 完整上传、已确认的 Git 操作和 Ports 转发均可实际使用。
4. 所有交付能力具有自动化 e2e，包含关键失败场景。
5. 使用本机已配置的真实 Agent 完成一个可验证的实际任务。

只通过 shell/echo fixture、只完成 PTY 首条链路或只产出协议文档，均不代表整个 MVP 完成。

## 2. 组件与进程

实现语言为 Go。一个 `dune` 二进制在不指定子命令时作为 supervisor，另提供 `gateway`、`fabricd` 和面向用户的操作命令。`fabricd` 是执行端 daemon 的命令名称。

```text
dune (supervisor)
  +-- dune gateway
  +-- dune fabricd (daemon)

CLI -> Go SDK -> Gateway <== daemon outbound WebSocket tunnel == daemon
                                                               |
                                                   Agent / OS / Files / Git / TCP
```

图中的进程父子关系与请求路径是两种关系。CLI/SDK 操作统一经过 Gateway；同机运行也不改为内存 channel 或直接调用 daemon。

| 组件 | 职责 |
| --- | --- |
| `dune`（无子命令） | 启动和监督 Gateway、daemon；异常退出后自动重启服务 |
| Gateway（`dune gateway`） | dev token 鉴权、在线路由、协议帧和实时流转发 |
| daemon（`dune fabricd`） | 接受受信 Gateway，解析 Profile，执行能力，管理自身创建的进程及内存句柄 |
| Go SDK | 提交完整输入、关联调用结果、读写实时流、暴露明确错误 |
| CLI | 使用 SDK 提供本机启动、交互及各能力操作入口 |

`fabricd` 指执行端 daemon；supervisor 使用裸命令 `dune`。命名不引入云 Fabric、Runner 或资源编排模型。此前讨论中的 `dune local` 和 `dune daemon` 均为历史命令名称，MVP 不要求保留旧命令别名。

### 2.1 传输与身份

- fabricd 主动连接 Gateway，每个 fabricd 保持一条有效的长期 WebSocket 隧道；SDK 经 Gateway 寻址目标 fabricd。
- WebSocket 适配为有序字节流，由 Yamux 提供多路复用；Dune protobuf 消息在各 Yamux stream 内传输，具体分层见第 2.3 节。
- protobuf schema 位于 `proto/dune/dtp/v1/*.proto`，使用 buf 管理生成。
- dev token 鉴权，拒绝空或无效 token；daemon 只接受受信的本机 Gateway。
- 操作以 daemon 当前 OS 用户执行，不切换 UID，不建立多用户权限模型。
- 保留目标、执行实例和 Runtime 句柄的有效性校验，不能用“单用户”替代旧句柄失效判断。
- 不实现企业身份系统；本地 TLS/证书配置和具体握手子集见第 10 节。

### 2.2 服务重启

supervisor `dune` 对 Gateway、daemon 的异常退出无限重试，重试延迟退避并设上限；不自动重新执行 Profile 或重新启动 Agent。

| 事件 | MVP 行为 |
| --- | --- |
| Gateway 退出 | supervisor 重启 Gateway；daemon 重新注册；存活 daemon 中的 Agent 保留；旧流中断，不回放 |
| daemon 退出 | supervisor 重启 daemon；产生新 incarnation；旧 Runtime 和上传句柄失效；不重跑 Profile/Agent |
| Agent 自身退出 | 返回退出状态，不因为服务自动重启策略而重新启动 Agent |
| 客户端流断开 | 报告流中断，不将断线当作任务完成，不自动重放输入 |

daemon 重启后的旧进程清理和正常关闭顺序属于必须落实的工程事项。句柄失效不等于进程已被清理，也不允许新 daemon 仅凭裸 PID 接管或杀死进程。

### 2.3 MVP 传输协议：WebSocket + Yamux

已确认采用 `hashicorp/yamux` 承担传输层多路复用，替代 DTP 草案中的自定义 stream 多路复用状态机。MVP 保留 Dune 操作协议，不把旧 protobuf Frame 再完整套在 Yamux 上重复实现同一层功能。

```text
Dune protobuf：注册 / 操作请求 / 结果 / 事件 / 数据消息
                    | 长度前缀，单个 stream 内有序
Yamux：双向 stream / stream ID / 流控窗口 / 开关流
                    | 有序字节流
WebSocket binary messages
TLS / TCP
```

建议使用 `coder/websocket.NetConn` 将 WebSocket 适配为字节流。WebSocket 消息边界不作为 Dune protobuf 消息边界；接收方按长度前缀读满消息，并在分配消息内存前检查上限。长度编码、消息 schema 和依赖版本在实现时固定。

参考：[Yamux](https://github.com/hashicorp/yamux)、[Yamux 协议](https://github.com/hashicorp/yamux/blob/master/spec.md)、[WebSocket NetConn](https://pkg.go.dev/github.com/coder/websocket#NetConn)。

#### 连接与 stream

- fabricd 到 Gateway 使用一条 WS、一份 Yamux session；增加 Agent/PTY/ACP 不新增物理隧道连接。
- Go SDK 到 Gateway 的连接独立计算，可以有多个客户端。MVP 实现采用相同的 WS + Yamux 适配，复用编解码和连接管理；不承诺浏览器原生兼容。
- Yamux client/server 角色与 stream ID 奇偶分配遵循库协议，不能沿用旧 DTP 的自定义奇偶分配规则。实现中主动连接的 SDK/fabricd 为 Yamux client，Gateway 为 server。
- 每个 session 建立专用 Dune 控制 stream，完成应用层版本协商、注册/绑定及能力发现；控制 stream 是普通 Yamux stream，不能占用 Yamux 保留的 stream ID 0。
- 控制握手完成前不执行业务操作；Yamux 接受 stream 本身不授予调用权限。

| 通道 | 承载方式 |
| --- | --- |
| 注册、能力与连接状态 | 专用控制 stream |
| PTY/ACP Agent 交互 | 每个交互订阅独立业务 stream，关联有效 Runtime |
| Exec/Git/Profile 调用 | 独立业务 stream，返回结果或进度事件 |
| 文件上传 | 独立传输 stream，关联上传句柄；续传可建立新 stream |
| Ports | 每条真实 TCP 连接对应独立业务 stream |

业务 stream 的首条 Dune 消息声明操作及目标上下文，Gateway 和 fabricd 校验后返回应用层接受/拒绝结果。该结果与 Yamux 的开流确认分开，也不代表任务已经完成。

Gateway 终止两侧 Yamux session，维护“客户端 session/stream -> fabricd session/stream”的映射并转发 Dune 消息。两侧 stream ID 不要求相等；Runtime ID、请求 ID、上传句柄和 ACP JSON-RPC ID 都不被 Yamux stream ID 替代。

#### Yamux 与 Dune 的职责

| Yamux 负责 | Dune 保留 |
| --- | --- |
| stream ID、开关流、逐流窗口、传输层存活探测 | 鉴权、应用版本、目标绑定、能力发现、incarnation/generation |
| 单流有序双向字节传输 | 长度前缀 protobuf 消息、操作类型、参数、结果、错误及事件 |
| 传输级流控和关闭信号 | admission、结果未知、有限去重、上传 offset/hash、进程退出状态 |

删除旧 DTP Frame 中用于重复传输分流的 `stream_id` 和回放用途的 `seq/ack_seq`，不再实现独立的传输窗口及 OPEN/ACK/RESET 状态机。应用层“操作已接受”“已写入 stdin”“文件已提交”等结果仍须明确返回，不能由 Yamux ACK 或正常 EOF 推断。

#### 并发、背压与故障

- 各 stream 独立处理，可用 goroutine 并发执行；同一 stream 的 protobuf 写入必须串行化，避免长度前缀和消息内容交错。WS 底层读写交给 Yamux 及适配层管理，业务处理器不直接并发写 WS。
- 每个 Runtime 的 stdin/PTY 输入串行写入；同一 Runtime 最多一个输入所有者，其他订阅可观察。多个 Agent 各自独立，ACP 业务消息调度仍由调用方负责。
- Yamux 提供逐流窗口，没有 session 总流控窗口。Dune 仍需限制活跃 stream 数量、待握手 stream 数量、消息大小、应用队列及总内存；单流有界不代表整个进程内存有界。
- Gateway 转发使用有界缓冲，让逐流背压沿两侧连接传递。慢消费者达到期限后关闭对应 stream 并返回可观察错误；不能无限堆积，也不能静默丢弃后继续假装 ACP 消息完整。
- 大数据分块传输并限制 bulk 并发，验证交互延迟。专用控制 stream 不等于传输优先级；Yamux 不提供旧草案所设想的 P0-P3 业务优先级保证，MVP 不额外实现一套自定义优先级多路复用器。
- TCP 队头阻塞、WS 中断或 Yamux session 级错误可能影响所有 stream，不能承诺物理连接级故障隔离。可局部处理的业务错误只结束对应 stream。
- 单流关闭不隐含 Agent 退出；隧道重连重新握手，旧 stream 不恢复，输入不重放。上传仅在同一 fabricd incarnation 内凭有效上传句柄查询 offset 后显式续传。

流 EOF、半关闭与取消的应用语义需在接口中明确，尤其是 Ports 的双向关闭和流式结果结束；实现时核对所选 Yamux 版本的 API 能力，不把底层正常关闭当成业务成功。

## 3. 配置与 Profile

只保留机器配置与运行输入 Profile，不引入额外项目配置层。

| 输入 | 用途 | 保存与读取 |
| --- | --- | --- |
| `~/.config/dune/config.yaml` | dev token、Gateway 地址、目标标识、日志等级等机器设置 | 本机文件，由启动组件读取 |
| Profile YAML / 完整 SDK payload | 本次准备与启动所需的命令、cwd、env、adapter | 调用方保存；CLI 显式读取文件或 SDK 直接提交；daemon 解析执行 |

Profile 可放在任意位置，不要求项目根目录存在 `dune.yaml`，不自动查找项目配置，不要求先注册 Profile ID。机器配置不保存最近 Runtime、任务历史、输出或恢复台账。

Profile 借鉴 GitHub Actions 的步骤表达和 Dockerfile 的环境描述，不实现 CI jobs/DAG、Dockerfile 解释器或镜像构建系统。

已确认字段方向：`kind`、`working_directory`、`env`、`setup.steps[]`、`start`，Agent 使用 `pty` 或 `acp` adapter。step 支持 `name`、`argv`、`run`、`timeout_seconds`。

以下为字段形状示例，版本字段、shell 字段及完整校验规则在实现 schema 时定版：

```yaml
kind: agent
working_directory: /absolute/path/to/workspace
env:
  EXAMPLE_MODE: development
setup:
  steps:
    - name: prepare
      argv: ["/bin/sh", "-c", "printf 'ready\\n'"]
      timeout_seconds: 30
start:
  argv: ["/bin/sh"]
adapter: pty
```

daemon 先校验 Profile，再顺序执行 setup，成功后启动 Agent；步骤失败停止后续执行并返回阶段与错误。支持结构化 `argv` 和显式 shell `run`，`argv` 不隐式进行 shell 展开。失败可能已有部分文件或进程副作用，不承诺自动回滚。

不检查 cwd、Files 或 Git 路径是否在某个 workspace root 内，也不增加路径白名单或护栏开关。不存在的路径和 OS 权限不足仍按实际操作返回错误。

## 4. Runtime、执行与 Agent

首批能力包括能力发现、Profile、Runtime、Exec、PTY、ACP。Runtime 状态只在 daemon 内存中保存，操作对象限于当前有效且由 daemon 创建的进程。

| 操作面 | 已确认范围 |
| --- | --- |
| 能力发现 | 返回当前可用能力与实际支持范围，不宣告未实现的恢复能力 |
| Profile | 接收完整输入，执行 setup，启动 Agent，返回执行结果及 Runtime |
| Exec | 执行普通命令并返回结果，支持明确的超时和失败反馈 |
| Runtime | 启动、查询、attach 交互、stop；CLI 包括 `runtime list/attach/stop` |
| PTY | 启动终端进程，双向字节流交互和终端控制 |
| ACP | 启动 stdio AgentServer，逐行 JSON-RPC 校验和双向透传 |

Process 的低层接口、PTY resize/signal、输入并发控制等按现有规范在实现时细化，不能据此默认加入任意宿主进程接管。

### 4.1 Agent 接入

由用户配置任意启动命令、工作目录和环境变量，支持多个独立 Runtime，不绑定某个内置 Agent 厂商。

- PTY adapter 把 Agent 当终端进程交互，不从屏幕文本或静默时间推断任务完成。
- ACP adapter 校验逐行 JSON-RPC envelope，不理解 method、prompt、session、permission 或任务结束语义。
- ACP 初始化、会话与权限应答由调用方或 Agent 客户端负责；双向消息均经同一传输链路。
- ACP 的 stdout 协议消息与 stderr 诊断需要分离，具体限制和错误处理在实现时定义。
- 本 MVP 交付 stdio bridge，不宣称 ACP Streamable HTTP 兼容。

PTY 生命周期参考 [BotMux 分析](references/reference-project-analysis-botmux.md) 的实例配置和进程能力。ACP stdio bridge 按公开 ACP JSON-RPC 协议实现，不引入外部产品的 UI、历史或业务资源模型。

### 4.2 输出与断线

不做输出历史、ring buffer 或断线回放。普通 attach 只接收订阅建立后的新输出，Gateway 重启后重新连接不恢复旧流。

实现仍须使用 Yamux 逐流窗口、有界的在途队列和明确的背压策略；这些是当前连接的数据传输设施，不是历史回放缓冲。并发和背压分工见第 2.3 节；无订阅和启动竞态的细节见第 10 节。

传输 ACK 不表示 Agent 已完成业务请求。输入已写入与否无法确认时返回明确的未知结果，不因重连自动重发。ACP 客户端不能把新流建立当作会话或丢失的请求结果已恢复。

## 5. Files、Git 与 Ports

### 5.1 完整上传

采用 DTP 原生上传协议，参考 [tus 协议](https://tus.io/protocols/resumable-upload) 和 [tusd](https://github.com/tus/tusd)。不要求现成 tus HTTP 客户端直接连接，也不要求部署独立 tus 服务。

完整流程包含：

1. 创建上传，确定目标文件、长度和校验信息，返回上传句柄。
2. 按 offset 分块传输，返回实际接受的位置。
3. 查询已接受的 offset；网络中断后显式续传。
4. 验证数据大小和 hash，完成提交。
5. 支持取消和过期，释放关联资源。

上传元数据保存在有界内存中；同一 daemon 实例存活期间支持网络断线续传，daemon 重启后旧句柄失效。不建立持久上传台账。

按既有 DTP 设计，文件先进入同文件系统临时文件，校验成功后原子提交。失败不得被报告为完整目标文件已提交。offset 冲突、hash 失败、重复请求、覆盖策略和临时文件清理的精确规则在实现时固定。

用户明确要求 Files write 和完整上传。配套基础接口建议沿用现有 `stat/list/read/write/mkdir/rename/remove`；这份基础接口集合属于实现建议，不是新增的一轮用户确认。

### 5.2 Git

调用本机 Git，向 CLI/SDK 提供结构化操作，覆盖已确认的编辑器常用能力：

| 类别 | 操作 |
| --- | --- |
| 查看 | `status`、`diff`、`log`、`show` |
| 工作区与暂存区 | 文件及 hunk 级暂存、取消暂存、丢弃 |
| 提交 | `commit`、`amend` |
| 分支 | `branch`、`checkout` |
| 暂存工作 | `stash` |
| 远端 | `fetch`、`pull`、`push` |
| 合并与变基 | merge/rebase 的开始、继续、中止，以及冲突文件查询 |

保留 hunk 和 merge/rebase，不退回只读 Git，也不将接口退化为任意 Git shell 字符串透传。具体操作参数、输出模型和错误处理在实现时定义。

Git 使用当前 OS 用户的仓库与本机 Git 环境；不建立 Dune 凭证数据库。远端凭证交互、冲突处理和写操作结果未知的行为须纳入接口设计和验证。

### 5.3 Ports

允许连接任意 `127.0.0.1:port`，不要求端口白名单。通过 DTP stream 转发真实 TCP 字节，每条连接独立处理关闭和错误。

不提供公网 publish、preview URL、域名或云入口。流断开后 TCP 连接失败，需要调用方建立新连接，不承诺透明连接迁移。`list/probe` 可参考完整规范细化；本轮明确确认的核心范围是 `connect`。

## 6. 数据边界与排除项

| 数据 | MVP 处理 |
| --- | --- |
| 机器配置 | `~/.config/dune/config.yaml` |
| Profile 源文件 | 调用方自行保存，daemon 接收完整内容 |
| 在线路由、Runtime、操作状态、上传元数据 | 有界内存，进程重启不承诺恢复 |
| 文件内容、上传临时文件、Git 工作区 | 实际写入用户文件系统 |
| Agent 原生数据 | 由用户配置的 Agent 自行管理，Dune 不接管其会话存储 |
| PTY/ACP 历史、输出回放、持久去重与 Runtime 台账 | 不建立 |

排除 root 多用户、OS 用户切换、云 Runner、Fabric 资源管理、数据库、UI、完整 ACP 业务语义、持久历史和输出恢复。不引入额外项目配置层、路径白名单或端口白名单。

“不落库”不等于“不写文件”；Files、Git 和 Agent 的正常工作仍会产生磁盘内容。

## 7. 交付物与实施顺序

已确认交付目录：

```text
cmd/dune/
internal/gateway/
internal/daemon/
pkg/sdk/
proto/dune/dtp/v1/
samples/
docs/mvp.md
```

supervisor 的内部目录可在编码时按职责安排，不另建资源编排子系统。

推荐按以下阶段实现，阶段通过不替代全部 MVP 验收：

| 阶段 | 交付 | 阶段验证 |
| --- | --- | --- |
| M1 通信底座 | Go 工程、buf/protobuf、WS + Yamux、SDK、Gateway、fabricd 执行端、dune supervisor、鉴权 | 真实子进程注册和调用；鉴权失败；多路 stream；服务自动重启 |
| M2 Agent 闭环 | Profile、Exec/Runtime、PTY、ACP | 准备、启动、双向交互、查询、停止；两种 adapter 均通过 |
| M3 开发能力 | 完整上传、完整 Git 清单、Ports | 真实文件、仓库和 TCP 操作及错误路径 |
| M4 完整验收 | 全能力 e2e、samples、运行说明、真实 Agent 任务 | 第 8 节全部验收项有结果记录 |

已交付的用户操作路径如下，参数与环境准备见仓库 README：

```sh
dune --config ~/.config/dune/config.yaml
# 在另一个终端启动交互。
dune profile start samples/agent-pty.yaml
dune runtime list
dune runtime attach <runtime-id>
dune runtime stop <runtime-id>
```

ACP 使用相应 Profile 和支持 ACP 的调用方交互；普通 TTY 不自动成为 ACP 业务客户端。

## 8. 验收矩阵

以下矩阵保留已确认范围的验证要求；自动化命令和外部验收边界见 [开发流程](workflow.md)。

| 范围 | 自动化验证要求 |
| --- | --- |
| 进程与链路 | 裸命令 `dune` 启动 `dune gateway` 和 `dune fabricd` 两个独立子进程；SDK 经 Gateway 到 daemon；空/错误 token 被拒绝 |
| Yamux 与编解码 | 多客户端映射不串流；双向开流；长度前缀消息跨 WS 消息和分次读取正确；拒绝超长/截断消息；握手前不执行操作 |
| 并发与背压 | 多个 PTY/ACP 同时交互并进行大文件上传/Ports 传输，记录交互延迟；慢消费者和单流失败不造成其他业务流停滞；限制 stream 数和总内存 |
| 重启 | 分别终止 Gateway/daemon，验证退避重启、重新注册、旧流中断、旧句柄失效；Agent/ setup 不自动重跑 |
| Profile/Exec | 正常完成、步骤失败、超时、cwd/env 传递、显式 shell 与 argv 行为 |
| Runtime/PTY | 启动、输入输出、查询、attach、stop、退出状态；attach 不回放历史 |
| ACP | JSON-RPC 请求、响应、通知双向转发；未知 method 保持透明；坏消息报错；stderr 不混入 stdout；权限请求往返 |
| Files | 已交付基础操作；分块上传、offset 查询、断线续传、hash/offset 错误、提交、取消、过期和 daemon 重启失效 |
| Git | 第 5.2 节所有操作，包括 hunk、远端同步、冲突及 merge/rebase 继续/中止 |
| Ports | 无白名单连接可用 loopback 端口；双向字节一致；连接拒绝、关闭和断线处理 |
| 数据与资源 | 重启不恢复 Runtime/输出；大流量不依赖全量内存缓存；临时资源和进程清理行为明确 |

推荐使用可控 PTY/ACP fixture、临时工作目录、本地 bare Git remote 和 loopback 测试服务，使自动化不依赖外部账号。测试目录只用于隔离测试副作用，不代表产品加入路径限制。

另行执行真实 Agent 验收：使用用户配置且已具备运行条件的真实 Agent，完成一个有明确结果的任务，例如修改文件并运行对应检查。记录启动命令/Profile、任务目标、结果及实际验证。两种 adapter 都要有真实进程接入验证；fake fixture 不能替代真实 Agent 任务。

外部 Agent 账号或环境缺失时，该项应记录为未完成，不能以其他测试通过宣告整个 MVP 完成。

## 9. 与完整规范的差异

| 主题 | 完整规范或早期讨论 | 本 MVP 决定 |
| --- | --- | --- |
| 传输多路复用 | WS 直接承载 DTP Frame，自定义 stream ID/OPEN/ACK/窗口 | WS 字节流 + Yamux，stream 内使用长度前缀 protobuf；不保持旧 Frame 线格式兼容 |
| 流量调度 | 草案建议 P0-P3 业务优先级 | Yamux 逐流流控，Dune 限制 bulk 并发和总资源；以并发测试验证，不承诺严格优先级 |
| 路径 | 授权 roots、路径边界 | 不做路径范围检查，仅受实际 OS 权限约束 |
| 输出 | ring buffer、短时恢复、replay | 无 ring buffer，无输出恢复，attach 只看后续输出 |
| Ports | 默认受限 loopback 目标 | 任意 `127.0.0.1:port`，无白名单 |
| Profile 字段 | 示例中为 `environment` 和 `setup` 数组 | 采用讨论确认的 `env` 和 `setup.steps[]` 方向，schema 实现时统一 |
| supervisor | 早期名称 `dune local`，曾误记为 `dune fabricd` | 裸命令 `dune` |
| daemon 命令 | 早期名称 `dune daemon` | `dune fabricd` |
| 服务恢复 | 曾建议子进程失败后整体退出 | 自动重启 Gateway/daemon，有上限退避，不自动重跑 Agent |
| 上传恢复 | 曾建议后置上传 resume | DTP 原生完整上传，支持同一 daemon 实例内断线续传 |
| Git 范围 | 曾建议只读或仅本地写 | 第 5.2 节完整清单 |

能力声明应如实反映裁剪结果，不宣称路径隔离、输出 replay、持久恢复或 tus/ACP HTTP 兼容。

## 10. 实现时落实的工程事项

以下事项不是继续大范围 grill 的前置条件。编码时由实现者给出具体设计与测试；只有改变已确认范围或用户可见语义时才重新讨论。

| 事项 | 需要落定的内容 |
| --- | --- |
| 平台与工具链 | 首个验收 OS、PTY/进程依赖、Go/buf 版本及安装步骤；不提前宣称全平台支持 |
| 本地网络与 TLS | 监听地址、端口、证书/信任配置、token 初始化方式；讨论以 WSS 为背景，不隐式关闭证书校验 |
| Yamux 接入 | 固定库版本、WS 字节流适配和 deadline 语义；应用协议版本标识、控制握手、stream 关闭/半关闭/取消映射；不与旧 Frame 格式静默互通 |
| schema | Profile version/shell 字段、argv/run 互斥、cwd/env 规则、操作 payload 和稳定错误模型 |
| 启动输出竞态 | 建议交互启动先订阅再放行进程，避免第一条 PTY/ACP 输出在订阅前丢失 |
| 背压与断线 | Yamux 窗口、stream 数、总内存、bulk 并发与写入块大小上限；慢消费者超时和无订阅时的 stdout 处理；确定并发测试的交互延迟门槛 |
| 生命周期 | 正常停机与异常重启区分；daemon 崩溃后的旧进程清理、所有权和避免误杀 PID 复用进程 |
| 上传 | chunk/整文件 hash 规则、offset 冲突、提交与覆盖语义、大小限制、TTL 和残留临时文件清理 |
| Git | 结构化输出解析、hunk 失效、冲突状态、凭证调用、写操作结果未知和并发写行为 |
| 操作与句柄 | 请求关联、incarnation/generation 校验、有限内存去重及结果未知；不自动重试不确定副作用 |

M1–M3 已实现；M4 的自动化检查与可选真实 Agent/远端验收按 [开发流程](workflow.md) 分开执行。历史环境结果不作为当前环境仍可用的保证，也不宣称完整 DTP/1 兼容。
