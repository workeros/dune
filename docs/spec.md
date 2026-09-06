# Dune 底层协议与 SDK 设计规范

> 状态：已确认的产品边界；接口与协议草案，尚无实现验收结论  
> 日期：2026-09-05  
> 范围：Dune v0.x  
> 定位：远程执行与实时交互的开源协议、SDK、Gateway 和 daemon

适用范围：本文约束底层协议与 SDK，并保留目标设计。仓库后续增加的个人 Web 产品层按 [Web 方案](personal-web-plan.md) 理解；本文的“无 UI / 不保存业务数据”不作为删除或禁止该产品层的依据。已实现的单机协议见 [实现说明](implementation.md)。

## 1. 定位与边界

Dune 提供远程开发环境的控制协议、SDK、Gateway 和环境内的 daemon，供个人工具、家庭共享产品、企业平台及其他上层系统集成执行与实时交互能力。

Dune 是可集成的开源底层组件。Dune 不管理 Runner、Tenant、Fabric 或云资源，不维护业务数据库，不持久化 Profile 和交互历史，不提供用户界面。

上层可以围绕 Runner 组织开发环境，也可以使用设备、项目或任务等产品对象。Dune 不要求调用方建立这些业务对象后才能使用协议。

Dune 定义标准能力接口和扩展能力接口；实际能力由 daemon 承载的实现、adapter 或插件提供，并如实声明支持情况。定义某项标准接口不意味着所有部署都必须实现它。

### 1.1 交付组成

| 组成 | 职责 |
|---|---|
| 协议与 schema | 执行上下文、能力发现、操作输入输出、实时流、错误及实例有效性约定 |
| SDK | 类型化调用、完整 Profile 传输、能力发现、实时流和明确的重连/错误处理 |
| Gateway | 认证授权接入、在线路由、可信上下文传递、转发、流控与背压 |
| daemon | Profile 解析执行、身份执行、能力调度、受控进程和流的内存状态 |
| 能力实现与插件 | 实现标准或带命名空间的扩展能力，声明实际功能、限制和约束 |

SDK 不包含 Runner/Fabric 的资源管理服务。上层可以基于 SDK 封装业务 OpenAPI；业务接口和 Dune 的底层操作协议必须分别命名与说明。

### 1.2 上层系统负责

- 用户、团队、Tenant、登录和业务权限策略。
- Runner 的创建、关联、停止、销毁、恢复和克隆。
- Fabric/Provider 接入、云凭据、资源申请、容量和回收。
- Profile 保存、命名、版本、选择和复用管理。
- 业务用户到 OS 账户的映射与账户准备。
- 任务状态、GitHub/IM 等事件、审批交互、Agent 编排和成果交付。
- 业务数据存储、工作台 UI、插件视图宿主和产品导航。

Dune 不实现配置 CRUD、资源生命周期 reconcile、调度器、业务队列、持久操作账本或用户内容审计。Profile 的解析执行属于 daemon，不能因配置存储在上层而要求上层先将其展开成命令。

## 2. 分层架构与部署

```text
上层产品 / 企业集成
  ├── 用户与授权策略、Runner/Tenant/Fabric
  ├── Profile 存储、资源生命周期、业务状态与 UI
  └── Dune SDK
          │ 经过认证授权的协议调用与实时流
          ▼
      Dune Gateway
          ▲
          │ daemon 主动建立的 outbound Tunnel
          ▼
      Dune daemon
          ├── Profile 解析与身份执行
          ├── 内存 runtime / scope / stream 状态
          └── 标准能力与扩展能力实现
                  │
        已有主机 / Sandbox / 容器 / Pod / VM
```

所有客户端操作使用统一的 SDK -> Gateway -> daemon 路径。Gateway 可以独立部署，也可以与 daemon 或上层服务同机合并部署；合并部署不取消认证授权和可信连接边界。

首版传输基线为 HTTPS/WSS。daemon 主动连接 Gateway，执行环境无需开放入站控制端口。当前不要求直连、P2P、多路径、QUIC 或多 Gateway HA；这些扩展不能改变身份、授权与副作用语义。

Gateway 转发 Profile、终端、ACP、文件和端口字节，不执行这些操作，也不解释 Agent 的业务对话。TLS 可以在 Gateway 终止；本版不承诺 Gateway 无法读取 payload 的端到端加密。

Gateway 的认证适配接口可以验证上层签发的短期凭证，或取得外部授权系统的可信结果。Dune 不因此建立本地用户、角色或 Tenant 数据库。部署所需信任根、凭证和静态配置由部署者提供及保管。

## 3. 底层协议对象

| 名称 | 含义与状态所有者 |
|---|---|
| target_id | 上层注入的不透明执行目标路由标识；Dune 不创建目标业务记录 |
| scope_id | 可信执行上下文标识；用于操作边界与内存绑定，不是 Runner 或 Tenant |
| execution_incarnation | 当前 daemon 执行实例的不可复用标识 |
| connection_generation | 同一 incarnation 内当前连接绑定的内存递增世代 |
| Runtime | daemon 创建的 Agent、后台进程或 PTY 的临时控制句柄 |
| runtime_id + runtime_generation | 当前 Runtime 及其具体进程实例 |
| Stream | 当前连接中的临时逻辑流 |
| Profile | 调用时完整传递、由 daemon 解析执行的配置 payload |
| Capability descriptor | 实现支持的操作、版本、状态、限制及约束 |

这些对象不构成持久资源数据库。上层可以保存业务关联，但必须在每次使用时重新确认底层目标、执行实例和句柄仍然有效。

### 3.1 执行实例与重连

- daemon 每次启动生成新的随机 execution_incarnation，不依赖磁盘计数器、PID 或主机名。
- 同一存活 daemon 的网络重连保留 incarnation，新绑定增加 connection_generation 并 fence 旧连接。
- daemon 重启、执行环境更换、克隆或 checkpoint 恢复时，必须重新绑定新的 incarnation。
- 外部环境实例标识可以参与绑定；恢复的内存镜像不能直接沿用旧凭证、连接和租约。上层及恢复集成必须触发重新登记。
- Runtime restart 增加 runtime_generation。旧进程的输入、信号和事件不得作用于替代进程。
- 上层 Runner 恢复可以保留 Runner ID，但不能据此认定底层 Runtime 或 PTY 已恢复。
- 无法确认当前实例时返回明确错误，不自动把旧请求改投新环境。

## 4. 授权与 OS 执行身份

### 4.1 可信调用链

```text
上层认证业务用户并准备身份映射
  -> 产生受限的调用凭证/可信授权上下文
  -> Gateway 校验调用方、目标和操作范围
  -> daemon 验证可信 Gateway 与执行上下文
  -> 在允许的 OS 身份下执行
```

Gateway 不允许匿名接入。target_id、scope_id、主机名、资源 ID 或可达 URL 都不能替代授权凭据。

可信上下文应限定受众、有效期、目标、当前 incarnation、允许的能力及操作、目录/端口等约束，以及允许的执行身份。操作现有 Runtime 时还应限定相应句柄和 generation。

上层负责权限策略和业务身份映射；Gateway 验证授权结果；daemon 执行可信上下文的边界。插件、Profile 和客户端提供的字段不能扩大授权。

凭证过期必须阻止后续未授权操作。即时撤销需上层授权适配或活跃连接通知支持；没有外部撤销信号时不得承诺永久离线凭证可以立即失效。访问终止与进程终止是两个操作。

### 4.2 root 多用户模式

root daemon 可以在同一宿主机上按授权使用不同 OS 用户执行。实际操作仍受所在 OS、namespace 和可用权限的限制。

- 上层负责将业务用户映射为 OS 账户；daemon 不保存用户映射数据库。
- 账户可以由上层预先准备，也可以通过获授权的 Profile 初始化。
- Profile 可以显式配置 root 执行初始化或其他操作，不需要额外的 Dune 人工确认流程。
- 可信调用上下文规定授权上限；Profile 在范围内选择执行身份，冲突时拒绝执行。
- Agent、PTY、Exec、Files 和 Git 等操作都须遵循目标 OS 身份的实际权限。
- 实现通过独立工作进程设置 UID、主 GID 和附加组，不对并发服务主进程随意全局切换用户。
- 身份不存在、身份切换失败或权限不足必须明确失败，不能回退为 root 执行。

### 4.3 非 root 单用户模式

非 root daemon 将所有调用作为同一 OS 用户执行，不区分上层业务用户，不建立用户级授权模型。

- Gateway 仍须认证授权，daemon 仍只接受受信 Gateway。
- daemon 仍校验目标、可信上下文、incarnation、generation、操作约束和句柄。
- 请求未指定其他执行身份时使用 daemon 自身身份。
- 明确要求另一 OS 身份的请求返回 IDENTITY_UNSUPPORTED，不能静默更改身份。
- 单用户模式不承诺上层用户之间的 OS 权限隔离。

### 4.4 多人共享宿主机

家庭或企业可以共享一台宿主机，由 root daemon 以多个 OS 身份执行；非 root 部署则共享一个 OS 身份。私有目录、共享目录和账户权限由宿主环境及上层配置。

Files 接口的路径边界与进程的 OS 权限是不同层。设置 cwd 或过滤 Files 请求不能隔离任意 Agent/Shell 命令；隔离要求必须由真实 OS 身份、容器或其他运行环境落实。

该分层参考 NAS 的用户/组、文件权限与应用运行身份模型，Dune 不引入 NAS 的用户目录或存储管理服务。参考：[Synology 权限模型](https://www.synology.com/en-us/dsm/7.3/software_spec/dsm)、[TrueNAS 应用存储](https://apps.truenas.com/getting-started/app-storage/)、[Linux 进程身份](https://man7.org/linux/man-pages/man7/credentials.7.html)。

## 5. Profile：上层保存，daemon 解析执行

### 5.1 输入模型

profile.v1 接受完整配置。上层保存配置，SDK 传输配置，Gateway 校验并转发，daemon 解析并执行。只传 Profile ID、要求 daemon 查询配置库，不符合该接口。

```yaml
schema_version: 1
kind: environment | agent
revision: caller-defined-revision
execution_user: optional-authorized-os-user
working_directory: /workspace/project
environment: {}
setup:
  - argv: [/opt/company/prepare-project]
    timeout_seconds: 300
start:
  argv: [/opt/company/start-agent]
adapter: pty | acp
```

以上命令为示意；实际命令、环境和 Agent adapter 由部署者配置。

- environment Profile 描述环境准备，使用 profile.prepare，不包含 Agent start。
- agent Profile 描述 Agent 的准备和启动，使用 profile.start，成功后返回 kind=agent 的 Runtime。
- revision 是上层关联信息，不构成 Dune 修订版库。
- execution_user 可省略，由可信上下文确定允许的默认身份；不存在可确定的执行身份时拒绝执行。
- Profile 可以选择 root，但必须落在本次调用授权范围内。
- 上层可以称配置为 RunnerProfile 或 AgentProfile；这些名称不引入底层 Runner 或配置资源。
- Secret 由上层或受信集成提供，不能要求 Dune 维护 Secret 数据库；连接凭证不得进入工作进程环境。

### 5.2 执行语义

daemon 校验完整 Profile、身份及路径后，顺序执行 setup；遇错停止，成功后按 kind 决定是否执行 start。

阶段至少区分 validating、setup_running、start_running、succeeded 和 failed。Agent 的 process_started、adapter_ready 与一次 Profile 调用完成也须能够区分。

setup 使用结构化 argv、明确 cwd/env 与超时，不做隐式 Shell 展开；使用 Shell 必须显式配置并满足授权。Profile 描述执行环境内的步骤，不包含 Runner 分配、云 API 调度、业务 DAG 或 GitHub 任务状态机。

上层无需先将 Profile 展开成 Exec 命令。SDK 可以做 schema 预检查，但不能替代 daemon 的解析、授权与执行校验。

初始化创建账户、目录和文件是被请求的环境变更。失败时可能存在部分副作用，Dune 不承诺自动 rollback，也不因此销毁环境。

### 5.3 缓存、幂等与恢复

Profile 输入、执行阶段、去重摘要和可选 setup single-flight 只保存在有界内存中。

缓存键必须区分 incarnation、scope、实际执行身份和完整 Profile 内容。缓存过期或 daemon 重启后，不承诺 setup 已执行一次；不得用旧 revision 冒充持久执行记录。

相同幂等键与不同内容冲突时拒绝；执行状态未知时不能自动重跑整份 Profile。调用方需要跨重启的初始化保证时，由上层协调或使初始化步骤本身具备相应幂等性。

Agent 的恢复命令可以在新的启动中读取开发环境里的原生会话数据；它产生新的底层进程句柄，不接管调用前已经运行的任意 Agent。

## 6. 标准与扩展能力

### 6.1 能力发现

能力 descriptor 至少描述版本、状态、操作、限制和约束；features 与带命名空间的 extensions 用于明确可选行为。

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

有效能力是实际实现、可信执行上下文和本次授权的交集。缺少能力、版本不符或约束不满足时明确拒绝，不能静默降级到权限更宽的 Shell。

插件可以实现标准接口或注册扩展 schema。扩展必须说明输入输出、所需权限、副作用、资源上限和恢复语义；不得跳过身份、路径、ownership 或 generation 校验。

能力声明可以动态变化，现有流失去必要条件时须降权或明确失败。Gateway 和 SDK 不能把 unavailable 改写为 ready。

### 6.2 标准操作面

| 能力 | 代表操作 | 基本约定 |
|---|---|---|
| profile.v1 | prepare、start | 完整 payload 由 daemon 解析执行 |
| runtime.v1 | list、get、attach、stop、restart | 只控制当前有效的已登记 Runtime |
| files.v1 | stat、list、read、write、mkdir、remove、rename | 按实际身份和授权路径执行 |
| exec.v1 | run、start | 短操作与持续进程明确区分 |
| process.v1 | list、get、attach、signal、wait | 只操作可证明归属的进程 |
| pty.v1 | open、attach、input、resize、close | 输入和观察分别控制 |
| ports.v1 | list、probe、connect | 按授权目标建立真实连接 |
| git.v1 | status、diff、log、show、fetch、checkout、commit、push | 结构化读写接口，服从相同身份与范围 |

该表定义公共语义，不要求每种部署安装全部实现。插件 UI、插件市场、Provider 管理和安装界面属于上层封装。

### 6.3 文件、进程和网络边界

- Files 的 read/write/remove/rename 独立授权；实现处理路径穿越、符号链接、挂载边界及 TOCTOU。
- 二进制文件和端口流量直接走有界 Stream，不转为内容日志。
- exec.run 是有时限的短操作，不创建可长期 attach 的 Runtime；exec.start 始终创建 Process Runtime。
- pty.open 创建 PTY Runtime；Agent 的 PTY adapter 不再额外创建第二个 PTY Runtime。
- 进程管理只能操作 daemon 自己创建并能证明归属的对象，不按同名进程或裸 PID 推断所有权。
- Ports 每条 Stream 对应一个真实 TCP 连接。默认目标受限；loopback 不等于用户隔离，多用户环境仍须限定允许访问的端口。
- 端口断线不承诺透明 TCP 恢复。公网服务发布、域名和业务入口由上层实现。
- Git 内部使用 Exec 不能绕过 Git 操作授权；同时，授予任意 Shell 可能赋予更宽的 OS 操作能力，上层不能把 typed Git 限制当作全面写入隔离。

## 7. Runtime 与 Agent 交互

一个 daemon 可以同时运行多个 Agent、后台进程和 PTY。Runtime 记录 scope、实际执行身份、当前 incarnation/generation、后端句柄与必要的内存状态。

单个 Runtime 退出、停止或失败不影响其他 Runtime，也不决定环境保留策略。客户端断线不等于 Agent 退出。

Agent 通过 pty 或 acp adapter 启动：

- PTY adapter 传输终端字节和终端控制，不从静默或屏幕文本猜测任务完成。
- ACP adapter 对接 Agent stdio。Gateway 转发 payload，不解释 prompt、response 或业务审批。
- 上层或对应协议客户端负责识别一轮结束；一轮结束不关闭进程。
- Agent 权限请求在当前有效实例和输入连接中往返，批准不能扩大原有执行上下文。
- 每个交互 Runtime 同时最多有一个输入租约，可有多个授权观察者；这是输入并发控制，不是持久业务用户系统。

daemon 重启后不依赖磁盘 ownership journal 找回旧进程。进程是否存活由 OS 或外部 supervisor 决定；Dune 不承诺原 PTY、输入租约或输出缓冲自动恢复。不能证明归属时不得清理可能复用 PID 的进程。

## 8. 状态与数据边界

### 8.1 允许的运行时内存状态

连接和在线路由、scope binding、当前 Runtime 句柄、操作阶段、Profile 输入、短期去重、输入租约和有界输出缓冲可以存在于内存。

这些状态必须有资源上限和适用的清理规则。运行中进程的控制记录不能仅因 replay TTL 到期而被删除；缓冲过期、访问过期和进程生命周期分别处理。

### 8.2 不提供自有持久化

Dune 不建立：

- Runner、Tenant、Fabric、用户、权限或 Profile 数据库。
- 持久 runtime 元数据、ownership journal 或恢复计数器。
- 命令输出、Agent/PTY 历史、文件内容或端口流量存储。
- 持久去重表、operation ledger、outbox、离线队列或事件 cursor。

上层注入的信任配置和凭证由部署者管理。Dune 可以输出不含内容的运行指标与规范化错误，但不能把日志或 diagnostics 当作隐藏数据库。

### 8.3 执行环境中的数据

Files、Git、Profile 等操作产生的代码、配置、目录和账户属于明确请求的环境变更。Agent 自身写入的会话文件也属于执行环境内容。这些行为不等于 Dune 提供存储服务。

上层可以读取开发机上的内容并呈现历史。保存、备份、快照及清理这些内容由上层与实际运行环境负责，Dune 不提供云端历史副本。

## 9. 流、重试与故障语义

各逻辑流有独立的有界队列和背压。控制消息及 Agent/PTY 交互不能被文件或端口大流量长期阻塞。

ACK、admission 和执行完成分别表达：

| 状态 | 含义 |
|---|---|
| not_admitted | 明确未接受操作，可以按策略重试 |
| admitted | 已接受，可能仍在执行；不等于成功完成 |
| unknown | 无法确认是否接受或完成，不能据此自动重放 |
| ACK | 当前流的传输/消费位置，不是业务成功 |

幂等键由调用方/SDK 提供；daemon 的去重只在声明的 incarnation 和内存窗口内成立。相同键不同内容返回 IDEMPOTENCY_CONFLICT。

Agent/PTY 输入、Profile、进程启动、文件追加、Git push 等副作用不能因超时或断线自动重放。查询结果已淘汰时应返回未知，不得把“查不到”当成“没有执行”。

断线处理：

- 网络断线不自动停止 Agent 或释放环境。
- 缓冲到期只丢弃缓冲，输入租约到期只撤销输入权；两者都不隐含杀进程。
- 短时 replay 只覆盖仍在 daemon 内存中的内容，超窗返回 RESUME_GAP。
- 断线期间不接受离线排队的用户命令。
- daemon 重启后缓存和句柄失效，SDK 不自动将旧请求投给新 incarnation。
- 任一局部能力错误优先影响当前 Stream；严重身份或 framing 错误可关闭连接。

完整 frame、握手和错误约定见 [Tunnel 协议](runner-tunnel-protocol.md)。

## 10. 上层产品与 Runner 模型

以下是已确认的上层设计，不是 Dune 核心资源模型：

- Runner 是独立开发环境的顶级对象，可包含多个项目目录和多个 Agent。
- Fabric 是 Runner 的运行后端；Attached 接入已有环境，Managed 分配环境。
- 用户直接选择 Fabric、环境配置和初始化配置；预设可基于上层 OpenAPI 二次开发。
- 长期远程开发优先，即时任务使用相同 Runner 模型。
- 可恢复的停止与最终销毁分开；恢复保留 Runner 身份，克隆创建新 Runner。
- save/load/checkpoint 由 Provider 声明；文件、进程、内存和 Agent 会话恢复能力分别表达。
- 标准能力可由上层提供通用界面；扩展插件可提供类似 MCP UI 的视图与受控交互。

六类上层产品为个人使用、个人生产力、家庭、开源项目管理、小企业和大公司版本，详见 [上层产品与 Runner 模型](stories/upper-layer-products.md)。

## 11. 开发者集成动线

企业可以只有传统云基础设施，不以已有 Agent 平台为前提。

```text
准备已有开发机或通过云 API 准备环境
  -> 部署 Gateway 与 daemon
  -> 安装所需能力实现 / adapter
  -> 接入上层认证、授权及 OS 身份映射
  -> 通过 SDK 连接并发现能力
  -> 完整提交 environment Profile
  -> daemon 解析执行环境初始化
  -> 完整提交 agent Profile，通过 ACP / PTY 启动 Agent
  -> 持续交互，独立使用文件、终端等能力
  -> 上层提供 Runner 管理、界面及业务入口
```

Profile 失败、Agent 退出或连接丢失由 Dune 返回执行事实；是否继续诊断、重试、保存或回收环境由上层决定。

参见 [企业平台工程师](stories/enterprise-platform-developer.md)、[SDK 集成](stories/openapi-agent-integration.md)、[个人开发](stories/personal-developer.md)、[飞书入口](stories/feishu-enterprise-developer.md) 和 [GitHub 集成](stories/github-open-source-contributor.md)。

## 12. 原设计迁移

| 原规范内容 | 当前归属与变化 |
|---|---|
| Agent Runtime Control Plane 产品定位 | 改为执行协议、SDK、Gateway 和 daemon |
| Runner/Tenant/Fabric CRUD 与关联 | 上层产品 |
| Managed/Attached 分配、回收及 reconcile | 上层 Fabric/Provider |
| manual/deadline/delayed/exit 资源保留策略 | 上层策略，不由 Dune 解释 Agent 完成 |
| Profile 配置库与修订管理 | 上层保存；完整 Profile 仍由 daemon 解析执行 |
| SQLite/PostgreSQL、runtime 元数据表 | 从 Dune 核心移除 |
| 持久 ownership journal 与跨重启去重 | 移除；以内存句柄和新 incarnation 明确边界 |
| Runner closed 后永不恢复 | 不再约束上层；停止/销毁分开，恢复同身份、克隆新身份 |
| Dune Web 与插件 UI 宿主 | 上层产品 |
| Files/Exec/Process/PTY/Ports/Git、ACP | 保留标准接口，由实际实现声明能力 |
| 动态能力、背压、输入租约、未知状态与 fencing | 保留底层协议职责 |

参考文档保留来源项目自己的术语和固定版本事实，Dune 的归属映射以本规范为准，见 [参考索引](references/README.md)。

## 13. 交付与验证边界

以下为实现计划和验收要求，不表示仓库已完成相应代码。

首个闭环聚焦 SDK 经 Gateway 连接开发机 daemon、执行完整 Profile、启动 PTY Agent 并持续交互，验证多 Runtime 与独立 Files/Exec。root 多用户与非 root 单用户使用相同协议及明确能力声明。

后续增加 ACP adapter、其余标准能力实现、扩展能力注册与完整 SDK 互操作覆盖；这些变化不引入上层平台对象或持久数据库。

发布前至少验证：

1. 无 Runner/Tenant/Fabric 服务和数据库时，协议、Gateway、daemon 与 SDK 仍可工作。
2. Gateway 拒绝匿名访问；daemon 拒绝不可信 Gateway 与伪造执行上下文。
3. root 并发操作使用正确 UID/GID/附加组；非 root 不切换用户，不能静默忽略身份冲突。
4. Profile 由 daemon 解析，root 配置不突破授权；失败、超时和部分副作用有明确结果。
5. Files、进程及端口操作符合真实身份和范围，插件不能绕过边界。
6. 多 Agent、PTY 和大文件并发时无队列饥饿；慢消费者不拖垮其他流。
7. 网络重连、Gateway 重启、daemon 重启、环境恢复和克隆分别满足 incarnation/generation 约定。
8. 缓冲或租约过期不杀进程；有副作用请求不会因断线盲目重放。
9. 旧句柄、PID 复用和无法证明归属时不误操作或清理进程。
10. Profile、ACP、PTY、argv/env、文件、Git、端口与凭证内容不进入日志、诊断包或隐藏持久存储。
11. 所有标准和扩展能力使用共享 schema 与稳定错误；缺失能力明确返回不支持。
12. 文档、SDK 示例和上层故事不再将业务 Runner API 冒充 Dune 底层接口。
