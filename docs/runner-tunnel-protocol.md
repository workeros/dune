# Dune Tunnel Protocol 设计

> 状态：Draft  
> 日期：2026-09-05  
> 面向版本：Dune v0.x  
> 上位规范：[Dune 协议与 SDK 设计规范](spec.md)

适用范围：本文是底层 DTP/1 草案；当前单机线协议见 [实现说明](implementation.md)，后续托管会话与 Web ACP 扩展见 [Web 方案](personal-web-plan.md)。不要以草案中的旧 Frame 或生命周期描述覆盖现有实现。

## 1. 文档定位

本文定义 SDK、Gateway 和 daemon 之间的实时控制与数据传输协议，工作名为 `DTP/1`。

文件保留 `runner-tunnel-protocol.md` 的历史路径，不表示 Dune 底层管理 Runner。

Dune 是底层开源组件，交付协议、SDK、Gateway 和 daemon。上层系统负责用户、授权策略、Runner、Tenant、Fabric、资源生命周期、业务编排、配置保存和界面。

Tunnel 接收已授权的执行上下文，将操作送到当前 daemon，并返回实时结果。它不创建 Runner，不维护业务资源关系，不提供离线任务队列、配置数据库或持久化事件流。

本文中的 target、scope、binding、runtime 和 stream 都用于寻址、执行或 fencing，不能被解释为 Dune 管理的业务资源。与本文冲突时，以 `docs/spec.md` 为准。

本文是目标设计，不表示对应能力已经实现；首版范围、验证门槛和建议参数需要在实现时逐项落实。

单机 MVP 已选择 [WebSocket + Yamux 传输方案](mvp.md#23-mvp-传输协议websocket--yamux)。本文第 4 节的自定义 Frame/stream 状态机和第 12 节的优先级调度是原始草案，不作为该 MVP 的线格式或调度保证；MVP 由 Yamux 承担传输多路复用，在 stream 内传输 Dune protobuf 消息，保留鉴权、执行身份、admission 与操作结果语义。其他明确裁剪见 [MVP 差异表](mvp.md#9-与完整规范的差异)。

## 2. 组件与通信路径

```text
上层产品 / 业务服务
  ├── 身份与授权、资源管理、完整 Profile、业务数据、界面
  └── Dune SDK
          │ HTTPS / WSS
          ▼
      Dune Gateway
          ▲
          │ daemon outbound WSS Tunnel
          │
      Dune daemon
          ├── 执行上下文与内存句柄
          ├── Profile 解析与执行
          └── 标准 / 扩展能力实现
                  │
            OS 用户 / 进程 / 文件
```

所有客户端操作统一经过 Gateway。Gateway 与 daemon 可以同机或随上层服务合并部署，SDK 的协议路径不变。

Gateway 负责验证调用凭证、获取可信授权上下文、在线寻址、帧转发、流量限制和背压。它不执行 Agent、文件或终端操作，也不解析 ACP 业务语义。

daemon 负责验证可信 Gateway、执行上下文与句柄，解析完整 Profile，调度能力实现，管理自己创建的进程，以及维护有界的内存缓冲和去重状态。

环境创建、停止、销毁、save/load、checkpoint 和上层 Runner 身份恢复均由上层及其 Provider 实现。Dune 不根据连接断开、Agent 回复结束或进程退出自动释放环境。

## 3. 身份、作用域与 fencing

| 字段 | 协议含义 |
|---|---|
| `target_id` | 上层提供的不透明路由标识，由受信部署或 enrollment 配置绑定 |
| `scope_id` | 可信执行上下文的标识，不是 Runner 或 Tenant |
| `execution_incarnation` | 当前 daemon 执行实例标识，每次启动生成新的随机值 |
| `connection_generation` | 同一 incarnation 内当前有效连接绑定的内存递增整数 |
| `runtime_id` | daemon 分配的临时进程控制句柄 |
| `runtime_generation` | 同一 runtime 句柄当前对应的具体进程实例世代 |
| `stream_id` | 当前物理连接内的临时逻辑流标识 |

有效访问必须匹配：

```text
target_id + scope_id + execution_incarnation
  + connection_generation
  + [runtime_id + runtime_generation]
```

- target 和 scope 的字符串本身不授予权限；它们必须来自经过认证的调用上下文。
- `execution_incarnation` 可以结合外部环境实例标识，但不能只使用会重复的 PID、主机名或磁盘文件。
- daemon 网络重连保留 incarnation；daemon 重启必须更换 incarnation。
- 换环境、从保存结果克隆或恢复 checkpoint 时必须重新绑定新的 incarnation，不能复用镜像内的旧连接和凭证上下文。
- `connection_generation` 仅在同一 incarnation 内比较；新 incarnation 无需继承旧计数器。
- runtime restart 或进程替换必须增加 `runtime_generation`；旧 generation 的输入、信号和回调无效。
- stream、lease 和幂等缓存都受 incarnation 约束。
- 上层保留原 Runner ID 进行恢复，不意味着底层 runtime 或 PTY 身份仍有效。

## 4. Transport 与 frame

首版以 WSS 为兼容基线：

- daemon 主动建立到 Gateway 的 outbound WebSocket，无需环境开放入站控制端口。
- SDK 向 Gateway 建立按需连接。
- WebSocket binary message 承载 DTP frame，一个连接可以复用多个 stream。
- 连接必须验证 TLS 对端；开发部署可显式配置信任本地 CA。
- TLS 在 Gateway 终止；DTP/1 不承诺 Gateway 无法读取 payload。

建议 envelope：

```proto
message Frame {
  uint32 protocol_major = 1;
  uint32 protocol_minor = 2;
  FrameType type = 3;
  uint64 stream_id = 4;
  uint64 seq = 5;
  uint64 ack_seq = 6;
  bytes payload = 7;
}
```

连接级 frame 包括 `HELLO`、`HELLO_ACK`、`BIND_SCOPE`、`BIND_ACK`、`UNBIND_SCOPE`、`HEARTBEAT`、`CAPABILITIES_CHANGED` 和 `GOAWAY`。

流级 frame 包括 `OPEN`、`OPEN_ACK`、`ACCESS_STATE`、`DATA`、`ACK`、`HALF_CLOSE`、`CLOSE` 和 `RESET`。

- `stream_id=0` 保留给连接控制；Gateway 发起的流使用奇数，daemon 发起的流使用偶数。
- 同一连接内不得复用 stream ID；流级错误只 RESET 对应流。
- major 不兼容或未知 required feature 必须拒绝；未知 optional field 可以忽略。
- 两端协商 frame 大小、流数量、队列容量和 replay 上限，并在分配 payload 内存前检查长度。
- 首版建议上限：control payload 1 MiB、DATA payload 256 KiB、每连接 256 个 stream、64 个 pending OPEN。
- heartbeat 建议间隔 10 秒、超时 35 秒；这些都是可协商配置，不是永久 API 合约。
- 后续 transport 可以替换为 QUIC 等实现，但不得改变授权、fencing、admission 和恢复语义。

## 5. HELLO、绑定与在线路由

daemon 的凭证、信任根和允许使用的 target 由外部部署配置提供。Dune 不保存设备注册数据库，也不承担 enrollment token 的持久化单次消费。

上层若使用一次性 enrollment，必须自行完成 token 消费、凭证签发、轮换和撤销，再把受信配置交给 Gateway 与 daemon。

HELLO 至少包含：

```yaml
protocol_versions: [DTP/1]
target_id: opaque-target
execution_incarnation: random-per-daemon-start
credential: opaque
proof_of_possession: bytes
daemon_version: string
platform: {os: linux, arch: amd64}
execution_mode: multi_user | single_user
features: [wss-mux, bounded-replay]
limits: {max_frame_bytes: integer, max_streams: integer}
```

Gateway 验证 daemon 凭证与 target 绑定。daemon 同时验证 Gateway 身份；仅能联网不构成可信连接。

可信 Gateway 请求建立 scope binding，提供 `target_id`、`scope_id`、当前 incarnation 和受限的执行上下文。daemon 验证上下文并返回能力描述与 `connection_generation`。

- scope binding 是内存中的授权和执行关联，不创建目录、OS 用户、容器或业务对象。
- 同一 daemon 可以接受多个 scope；它们是否共享 OS 用户或目录由外部上下文决定。
- 路由至少按 `target_id + scope_id + execution_incarnation` 区分。
- 同一 scope/incarnation 默认只有一个有效 Gateway binding。
- daemon 接受新 binding 时先 fence 旧 generation，再允许新操作。
- 新 incarnation 的发现不能使 SDK 自动把旧操作改投新实例；调用方必须显式采用新上下文。
- 路由不存在立即返回 `ROUTE_NOT_PRESENT`，不进入离线队列。
- Gateway 重启后 daemon 重新认证和绑定；内存路由不通过持久化日志恢复。
- 多 Gateway 的旧连接 fencing 最终由 daemon 裁决；DTP/1 首版不承诺集群 HA 或跨 Gateway 一致路由。

HEARTBEAT 仅证明连接活跃和当前健康状态。它不是用户活动，也不触发上层资源保留策略。`UNBIND_SCOPE` 撤销绑定和操作入口，不能隐含停止 Agent 或销毁环境。

## 6. 授权上下文

Gateway 必须认证调用方并验证授权，不能因 daemon 采用单用户模式而开放匿名远程控制。

授权可以通过上层签名的短期凭证或明确的外部认证适配接口提供。Dune 不保存用户、组、角色或 Tenant 数据库。

可信上下文至少约束：

```yaml
audience: [dune-gateway, dune-daemon]
target_id: opaque-target
scope_id: opaque-scope
execution_incarnation: current-incarnation
expires_at: timestamp
capabilities:
  files.read: {roots: [/workspace]}
  pty.read: {}
  pty.write: {}
execution_identity: externally-authorized-user-or-identity-bound
runtime: {runtime_id: optional, runtime_generation: optional}
```

实际 schema 以主规范和公共协议定义为准；本例表达需要签名或受信传递的约束，不建立持久对象。

- Gateway 验证签名或外部认证结果、受众、时间、目标和操作范围，再进行路由。
- daemon 验证可信连接以及完整上下文，不能信任客户端自行填写的 identity、scope 或 runtime 字段。
- 只观察不能隐式获得 write、resize、signal、stop 或 close 权限。
- runtime 定向授权必须同时绑定 runtime ID 和 generation。
- 授权上限只能收窄，插件、Profile 和客户端参数都不能扩大。
- 凭证过期、收到撤销信号或 lease 失效后拒绝后续操作；终止访问不等于终止进程。
- 撤销可通过外部授权适配或活跃连接的撤销通知实现；Dune 不维护持久撤销库。
- 没有外部撤销信号时，只能依靠短期凭证或 lease 过期收敛，不能承诺策略变化立即使现有访问失效。
- 凭证不得通过长期 URL query 传递，也不得进入 Agent 子进程环境。
- Gateway 可使用自身签名的短期上下文传给 daemon，但必须来自已经完成的上层鉴权，不能自行授予额外权限。

## 7. root 与非 root 执行模式

`multi_user` 模式要求 daemon 具有执行所需的 root 权限，并声明实际可用的 OS 身份切换能力。

- 上层负责把产品用户映射到 OS 账户；账户可以预先准备，也可以通过获授权的 environment Profile 初始化。
- 可信调用上下文规定允许使用的 OS 身份；Profile 可以在该上限内选择身份，包括显式选择 root，冲突必须拒绝。获授权的 root 配置不引入额外的 Dune 人工确认流程。
- Agent、PTY、Exec、Files、Git 等操作均必须使用目标 OS 身份对应的权限。
- root daemon 在隔离的子 worker 中设置 UID、主 GID 和附加组；不能对并发服务主进程进行全局用户切换。
- 身份切换失败必须拒绝执行，不能回退到 root。
- runtime ownership 必须记录实际执行身份；访问另一用户的句柄仍需满足授权范围。
- Linux 的 capability、namespace 等限制可能使 UID 0 仍缺少实际操作权限，能力描述必须反映事实。

`single_user` 模式下 daemon 不区分上层用户，不建立用户级授权模型，所有操作使用 daemon 当前 OS 身份。

- 它仍验证可信 Gateway、scope、incarnation、generation、句柄和操作上下文。
- 请求或 Profile 明确要求其他 OS 身份时返回 `IDENTITY_UNSUPPORTED`，不能静默改成当前用户。
- 同一 OS 身份不提供多人隔离承诺；上层必须据此决定是否把多个用户路由到该 daemon。
- 文件接口的路径限制不能自动约束 Agent 或 PTY 中所有命令；执行隔离强度由 OS 环境及能力实现如实声明。

## 8. 标准与扩展能力声明

Dune 定义标准能力接口，插件或适配实现声明支持哪些能力；扩展能力必须使用命名空间标识和显式版本。

```yaml
capabilities:
  files.v1:
    status: ready
    operations: [stat, list, read, write]
    features: [atomic_replace]
    limits: {max_transfer_bytes: 1073741824}
    constraints: {roots: [/workspace], symlink_policy: confined}
  profile.v1:
    status: ready
    operations: [prepare, start]
    features: [environment, agent, pty, acp]
  example.vendor.inspect.v1:
    status: unavailable
```

每项声明必须包含版本、状态、操作、限制和执行约束；features 与 extensions 可选。`ready`、`degraded`、`unavailable` 的含义不得由 Gateway 静默改写。

有效能力是实际实现、scope 上下文和本次授权的交集。需求无法满足时拒绝操作，而不是假装支持或扩大权限。

daemon 可以通过 `CAPABILITIES_CHANGED` 动态更新声明。更新后的限制立即约束新操作；已有流若不再满足必要条件，应降权或返回明确错误，不得继续扩大权限。

插件界面、插件市场、Provider 配置和安装管理属于上层产品；Tunnel 只承载已注册 schema 的能力调用，不执行任意未登记 RPC。

## 9. 打开操作流

SDK 先取得上层授权与当前执行上下文，再通过 Gateway 发送 OPEN：

```yaml
request_id: request-for-correlation
target_id: opaque-target
scope_id: opaque-scope
execution_incarnation: current-incarnation
capability: profile.v1
operation: start
credential: short-lived-credential
runtime_id: optional
runtime_generation: optional
idempotency_key: required-for-side-effects
resume_after_seq: optional
input: typed-operation-input
```

- `request_id` 仅关联请求，不负责去重。
- Gateway 将已验证的可信上下文及当前 binding generation 传给 daemon。
- 访问已有 runtime 必须提供 ID 和 generation；创建时由 daemon 在成功登记后返回。
- input 必须匹配能力与操作的 typed schema，不能成为无界 JSON 或任意 shell RPC。
- daemon 在副作用发生前校验身份、scope、incarnation、generation、能力、限制和输入。
- `OPEN_ACK` 表示流建立或请求获得 admission；必须明确标注阶段，不能被解释为业务完成。
- 对交互流，`ACCESS_STATE` 必须先于 DATA，说明实际取得的读写权限与 input lease。
- 拒绝 OPEN 不得影响其他 stream；操作状态通过同一流或明确的状态查询接口返回。

## 10. 完整 Profile 的执行

`profile.v1` 接收完整配置，`kind` 为 `environment` 或 `agent`。Profile 是一次调用的输入，不是 Dune 保存和引用的配置资源。

```yaml
kind: agent
schema_version: 1
revision: caller-defined-revision
execution_user: optional-authorized-os-user
working_directory: /workspace/project
environment: {}
setup: []
start:
  argv: [agent-command]
adapter: pty | acp
```

实际字段遵循主规范；上层可以把配置称为 RunnerProfile 或 AgentProfile，但必须传递完整内容，不能要求上层先将 setup/start 展开成若干 Exec 命令。

- SDK 负责 schema 校验和传输，daemon 负责解析、校验身份与路径、顺序执行 setup，并在成功后执行 start。
- `environment` 只执行环境准备；`agent` 执行 Agent 准备与启动，并创建 `kind=agent` runtime。
- revision 由上层保存；daemon 计算完整执行输入的摘要用于本次校验和内存去重。
- Dune 不实现 Profile CRUD、修订版数据库、Secret 数据库或跨重启的 setup 台账。
- 上层解析和授权所需 Secret 后随本次调用提供；Secret 只在当前执行所需内存和明确的子进程环境内使用。
- Profile 不得覆盖 Gateway、Tunnel 或 daemon 凭证，不能扩大受信执行身份或能力上限。
- setup 顺序执行、遇错停止；实时返回 `validating`、`setup_running`、`start_running`、`succeeded`、`failed` 等明确阶段和结果。
- Agent 的 `process_started`、`adapter_ready` 与整次 Profile 调用完成必须能够分别识别，不能只用一个 ACK 表达。
- setup stdout/stderr 实时流式返回，受输出缓冲和背压限制。
- 失败只描述本次执行结果；是否停止、保存或销毁环境由上层决定。
- Profile 不承诺自动 rollback；部分 setup 副作用可能已经留在环境中。
- Agent 命令可以读取其自身的会话文件恢复上下文，但此次启动仍创建新的 runtime。
- Dune 不接管调用前已经运行的任意 Agent 进程。

setup single-flight 和已完成标记只能保存在有界内存中，并按 incarnation、scope、有效执行身份和完整 Profile 摘要区分。缓存过期或 daemon 重启后，不能继续声称 setup 已执行一次或 exactly-once。

## 11. runtime ownership 与进程控制

daemon 在内存中登记自己创建的进程：

```yaml
runtime_id: random-runtime-handle
runtime_generation: 1
execution_incarnation: current-incarnation
scope_id: opaque-scope
kind: agent | process | pty
execution_identity: effective-os-identity
backend_target: opaque-local-handle
ownership_nonce: random-per-created-runtime
```

- ownership 不能仅通过 PID、名称或工作目录推断；须结合可靠的进程句柄、身份和创建事实。
- 进程探测返回 `exists`、`missing` 或 `unknown`；超时和无法证明不能当作 missing。
- signal、stop、restart、attach 和 cleanup 只能作用于当前 incarnation、scope 与 generation 下可证明归属的目标。
- 无法证明归属时返回 `RUNTIME_OWNERSHIP_UNPROVEN`，不得尝试清理同名或复用 PID 的进程。
- `exec.start` 创建 process runtime；`pty.open` 创建 pty runtime；Agent 的 PTY adapter 不额外创建一个 pty runtime。
- daemon 重启后内存句柄失效，不能凭旧 runtime ID 恢复管理或承诺原 PTY 可连接。
- 旧进程是否存活由 OS 或外部 supervisor 决定；Dune 不靠持久 ownership journal 重新接管它们。

Agent 与 PTY 每个 runtime 最多有一个 input owner，由 daemon 依据短期 lease 裁决。其他授权连接可以观察；观察者的输入、控制序列和 resize 不得绕过权限进入进程。

## 12. 多路复用与背压

每个 stream 必须有独立、有界的收发队列。Gateway 转发队列和 daemon replay buffer 分开计量，不能使用一个全局无界队列。

建议优先级：

```text
P0  heartbeat / access state / fencing / connection control
P1  Agent 与 PTY 交互、signal
P2  Exec 输出、Git 进度、Profile 阶段状态
P3  文件与端口大流量
```

- P0 保留独立的有界容量，其余流采用公平调度。
- 文件传输不能占满整个物理连接或阻止控制消息。
- 慢消费者只 RESET 对应 stream，返回 `SLOW_CONSUMER`。
- 连接过载先拒绝新 OPEN，再处理 bulk stream，最后才关闭整个 Tunnel。
- 交互流断开后，daemon 按能力声明继续有界排空或丢弃输出，并报告 gap；不能通过无限缓存维持进程。
- 背压、输出丢弃和流关闭都不能被隐式解释为 Agent 完成或环境回收。

## 13. ACK、断线与有界恢复

ACK 表示当前 stream 的消费位置，不表示命令完成、文件提交、ACP 请求完成、PR 创建或业务成功。

daemon 可以保留有界输出 ring buffer：

```text
runtime_id + runtime_generation + incarnation
next_seq / min_available_seq / last_ack
chunks[] / max_bytes / expires_at
```

建议初始上限为每 runtime 32 MiB、最大保留 30 秒；具体值必须声明并协商。

- sequence 针对明确的输出流和 runtime generation；重启或新 generation 不得延续旧流假装无缝恢复。
- 超出窗口返回 `RESUME_GAP`，并给出当前可用起点。
- replay 只包含 daemon 内存仍持有的输出，不能宣称完整历史。
- buffer 过期只丢弃缓冲，lease 过期只撤销输入权限；两者都不隐含终止 Agent。
- Client 或 Tunnel 断开不自动停止 runtime；断线期间不接收新用户操作。
- 网络重连可以恢复同一 incarnation 下仍可验证的句柄和输出窗口。
- daemon 重启后输出缓冲、输入租约、setup 状态和去重缓存都可丢失。
- checkpoint 恢复必须重新建立外部实例关联和新 incarnation，旧连接、lease、缓存及句柄不可继续有效。

PTY 恢复可以返回有界 raw ANSI 输出，但不能保证完整屏幕状态。无法还原终端状态时必须显式标记 gap 或 degraded。

ACP 消息作为 Agent 流 payload 转发。Gateway 不解析 method 或业务结果；输入消息不得因为 transport retry 自动重放。

- transport request ID、输入幂等键与 ACP JSON-RPC ID 分别用于调用关联、有限去重和 Agent 请求/响应匹配，不能相互替代。
- ACP 权限请求的响应须保留原 JSON-RPC ID，并限定到当前 scope、incarnation、runtime generation 和 Agent session；只有当前授权输入方可以写入。
- 调用方根据匹配的 ACP result/error 判断一轮结束，不能把传输 ACK 或输入 admission 当成一轮完成。
- 权限响应投递结果未知时不得自动重放；用户对 Agent 请求的批准不能扩大可信执行上下文。

## 14. 副作用、admission 与去重

每个有副作用操作须表达：

```text
not_admitted  明确尚未接受，可以按策略重试
admitted      已接受；可能仍在执行，不能因断线直接重放
unknown       无法确认是否接受或完成，需查询或由调用方决策
```

- admission 与执行状态独立；`admitted` 不等于 succeeded。
- 超时、socket 关闭或 ACK 缺失不能证明没有发生副作用。
- Gateway 不生成幂等键，也不替 SDK 自动重放副作用。
- daemon 的有界缓存以 incarnation、scope、有效身份、能力、操作和幂等键为 key，并保存请求摘要。
- 同 key 同摘要返回当前已知结果；同 key 不同摘要返回 `IDEMPOTENCY_CONFLICT`。
- 查询结果已淘汰时返回 unknown 或明确的不可查询状态，不能把它当作 not_admitted。
- 缓存 TTL 之外或 daemon 重启后的相同 key 不具备去重保证。
- Profile、Agent 启动、PTY/ACP input、文件 append、Git push 等不能 blanket retry。
- 对 setup 结果未知的 Profile，调用方必须先查询可用状态或作出显式重试决策；daemon 不自动从头执行。

DTP/1 不提供永久事件回放、durable admission、离线队列或 exactly-once delivery。

## 15. 各类数据操作的约束

**Files：** 使用授权 roots 和实际 OS 身份执行。路径字符串规范化不是完整边界检查；实现须处理 traversal、symlink、mount 与 TOCTOU，read/write/remove/rename 独立授权。

下载支持 offset 和文件 identity 校验；源文件变化返回 `SOURCE_CHANGED`。

上传可使用同文件系统的受控临时文件，按 upload handle、offset、大小和 hash 校验，完成后 atomic rename。上传进度仅保存在有界内存；daemon 重启后不承诺旧 upload handle 可恢复。过期或孤立临时文件按显式实现或宿主策略清理，不建立持久上传台账。

**Exec / Process：** 默认使用结构化 argv，无隐式 shell interpolation。显式 shell 也必须被授权。同步 Exec 必须有超时和输出上限；持续执行通过 runtime 句柄管理。

**PTY：** read、write、resize、signal 和 close 分别校验。PTY input 不进入输出 replay，也不能作为幂等消息自动重放。

**Ports：** 每个 stream 对应一个真实 TCP connection，默认只允许受限 loopback 目标。连接断开后需重新建立应用层连接，不能伪装为透明 TCP 迁移。实现必须限制未授权目标和宿主控制接口。

loopback 不等于用户隔离。多用户共享宿主机时，上层必须明确授权可以访问的端口或服务目标；daemon 不能仅因连接地址是 loopback，就允许一个 OS 用户访问另一用户的服务。底层无法满足要求的网络隔离约束时必须明确拒绝或声明不支持。

**Git：** read/write 分别授权；仓库必须在允许的目录范围内。内部使用 Exec 不得绕过 Git scope，remote credential 不得进入 Gateway 日志或错误。

**扩展能力：** 必须声明 schema、权限、资源上限、副作用与恢复语义。扩展不得跳过可信上下文、OS 身份、ownership 或 generation 校验。

## 16. 错误模型

Tunnel 与执行上下文使用稳定错误码：

```text
AUTH_REQUIRED / AUTH_INVALID / AUTH_EXPIRED
CONTEXT_INVALID / SCOPE_DENIED / IDENTITY_UNSUPPORTED
PROFILE_INVALID / PROFILE_IDENTITY_CONFLICT
ROUTE_NOT_PRESENT / INCARNATION_MISMATCH / GENERATION_MISMATCH
PROTOCOL_UNSUPPORTED / FRAME_TOO_LARGE / STREAM_LIMIT_EXCEEDED
CAPABILITY_UNAVAILABLE / REQUIREMENT_UNSATISFIED
IDEMPOTENCY_CONFLICT / RESULT_UNKNOWN / RESUME_GAP / SLOW_CONSUMER
RUNTIME_PROBE_UNKNOWN / RUNTIME_OWNERSHIP_UNPROVEN
SOURCE_CHANGED / TARGET_NOT_ALLOWED
```

能力错误优先 RESET 当前流；身份伪造、framing 破坏或协议不兼容可以关闭连接。错误不得回显 Secret、凭证、完整 Profile、环境变量或未授权目录内容。

Gateway 不把路由错误转成 Runner 生命周期状态；这属于上层的解释。

## 17. 数据、日志与清理边界

Dune 不保存业务数据、Profile 数据库、配置台账、ownership journal 或内容历史。

当前执行所需的内存状态可以包括连接表、作用域绑定、进程句柄、租约、Profile 输入、短期去重和有界 replay；必须有容量限制及适用的过期策略。

通过 Files、Git 或 Profile 明确写入执行环境的文件是被请求的操作结果；Agent 自己写入的会话文件也属于环境内容，不成为 Dune 的存储服务。

- 日志和 tracing 不记录 ACP、PTY、prompt、文件内容、argv/env、完整 Profile、Port payload 或凭证。
- 可观测性只输出必要的请求关联、能力、耗时、字节数和规范化错误码；避免目标或路径成为高基数 metrics label。
- 外部部署注入的信任配置和凭证由部署者保管，不能进入工作进程环境。
- 运行中进程的 ownership 与控制记录不能仅因 replay TTL 到期而删除；输出缓冲、访问租约和进程生命周期分别管理。
- `CLOSE`、断线、路由丢失和缓存过期不能触发任意进程清理。
- 显式 stop/close 操作必须使用当前可证明归属的句柄；环境销毁由上层和 Provider 决定。
- 不因需要“恢复”而偷偷增加 SQLite、磁盘日志或持久计数器。

## 18. 协议验证与首版边界

发布前至少验证：

1. Gateway、daemon 与 SDK 使用同一 schema；覆盖版本协商、未知字段、畸形帧、大小限制和稳定错误码。
2. Gateway 拒绝匿名调用；daemon 拒绝不可信 Gateway、伪造上下文、越界身份及旧 incarnation/generation。
3. root 并发请求按子 worker 身份执行；非 root 不切换用户，身份冲突明确失败。
4. 完整 Profile 由 daemon 解析执行；setup/start 阶段、失败、超时、single-flight 和重启后未知状态符合协议。
5. runtime ownership 防止跨 scope、跨身份、PID 复用和旧句柄操作；unknown 不被当作 missing。
6. Agent、PTY、文件与 Port 并发时无队列饥饿；慢消费者只影响自己的流。
7. 短断线恢复、有界 gap、Gateway 重启和 daemon 重启分别符合保证；ACK 不被当作完成。
8. 已 admission 或结果未知的副作用不自动重放；缓存过期不承诺去重。
9. 断线、buffer/lease 过期不隐含杀进程；Port 断线不宣称透明恢复，多用户 loopback 访问遵循目标授权。
10. 内容 canary 不出现在日志、tracing、诊断包或未授权子进程环境中。
11. 没有外部撤销信号时，授权收敛不超过已声明的凭证或 lease 边界；不得宣称立即撤销。

首版不要求多 Gateway HA、QUIC/WebTransport、P2P、端到端 payload 加密、永久 replay 或跨 daemon 重启的 PTY 恢复。

> Tunnel 的不变量是：可信上下文约束操作，当前 incarnation 与 generation 约束实例，有界内存限定恢复窗口，明确 admission 阻止盲目重放。业务对象、持久状态和环境生命周期始终由上层系统负责。
