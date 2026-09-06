# Dune 参考项目分析：OpenClaw

> 关联规范：[设计规范](../spec.md) · [传输协议](../runner-tunnel-protocol.md) · [调研索引](README.md)

## 调研基线

| 项 | 内容 |
|---|---|
| 来源 | [openclaw/openclaw](https://github.com/openclaw/openclaw) |
| 固定版本 | [`ea80657`](https://github.com/openclaw/openclaw/tree/ea806575e6450e4d1efdfc72c19f04be982a1b9b)，仓库版本 `v2026.8.1` |
| 许可证 | MIT |
| 关注点 | Agent 配置、gateway/node、设备配对、sandbox/tool policy、onboarding、doctor 和升级 |

## 来源事实

OpenClaw 是个人 Agent 产品和消息 gateway。`Gateway` 持有 channel、Agent、session、tool 和 node 状态；`Agent` 包含模型、workspace、工具和凭据配置；`Node` 与 gateway 配对并声明设备能力。来源 session 是对话状态，sandbox/tool policy 控制工具执行位置与权限，onboard/doctor/update 提供安装、检查和升级流程。

这些业务能力不因其名为 Gateway 而自动成为 Dune Gateway 的职责。

## 对 Dune 底层可复用

- 启动配置与实际进程实例分开：daemon 接收完整 `kind=agent` Profile payload，解析 executable、argv、env、cwd、setup 和 adapter；每次启动得到独立有效的进程 handle。
- 能力存在不代表有权调用；有效操作取授权范围、实际能力、执行身份和操作约束的交集。
- 调用方凭证、daemon 连接凭证和能力调用授权有各自 audience 与有效范围，不把高权限凭证传入 Agent 子进程环境。
- root 多用户 daemon 依受信任配置映射 OS 执行身份；non-root 单用户 daemon 受既有 OS 身份限制。两种模式均验证入口授权，不能让客户端自行选择任意高权限身份。
- doctor 可分别诊断配置、凭证、连接、协议兼容、能力和 Agent adapter；最小验证链路是部署、连接、发现能力、执行一次命令/PTY 操作。
- 标准能力和扩展能力都需声明接口、版本与约束，不能退化为无授权限制的任意远程方法调用。

## 上层参考

设备配对和身份登记、用户/租户、配置的命名/修订/保存、Agent 选择、业务默认值、升级编排及管理 UI 属于上层。上层可把 Profile 作为版本化资源保存，调用底层时提交完整 payload；修改上层配置不会改变已启动进程实际使用的配置。

GitHub、飞书、Cron、channel、聊天会话、历史读取和多 Agent 业务编排由上层产品组合。Agent 自身可把历史留在开发机，再通过其恢复命令启动。

## 不纳入核心

不复制 channel routing、聊天 session、中心 history、Agent loop、plugin marketplace 或业务 consent UI；不建立 Profile 配置库、设备注册数据库或 durable event store。不以个人可信域假设替代共享宿主机上的 OS 权限和执行隔离。

## 证据链接

- Agent runtime 组件和工具边界：[agent-runtime-architecture.md](https://github.com/openclaw/openclaw/blob/ea806575e6450e4d1efdfc72c19f04be982a1b9b/docs/agent-runtime-architecture.md)。
- Agent workspace 与配置职责：[agent-workspace.md](https://github.com/openclaw/openclaw/blob/ea806575e6450e4d1efdfc72c19f04be982a1b9b/docs/concepts/agent-workspace.md)。
- Node 能力和配对模型：[nodes/index.md](https://github.com/openclaw/openclaw/blob/ea806575e6450e4d1efdfc72c19f04be982a1b9b/docs/nodes/index.md) 与 [pairing.md](https://github.com/openclaw/openclaw/blob/ea806575e6450e4d1efdfc72c19f04be982a1b9b/docs/channels/pairing.md)。
- operator scope 设计：[operator-scopes.md](https://github.com/openclaw/openclaw/blob/ea806575e6450e4d1efdfc72c19f04be982a1b9b/docs/gateway/operator-scopes.md#L10-L177)。
- sandbox 与 tool policy 的边界：[sandbox-vs-tool-policy-vs-elevated.md](https://github.com/openclaw/openclaw/blob/ea806575e6450e4d1efdfc72c19f04be982a1b9b/docs/gateway/sandbox-vs-tool-policy-vs-elevated.md)。
- doctor、更新与迁移入口：[doctor.md](https://github.com/openclaw/openclaw/blob/ea806575e6450e4d1efdfc72c19f04be982a1b9b/docs/cli/doctor.md)、[updating.md](https://github.com/openclaw/openclaw/blob/ea806575e6450e4d1efdfc72c19f04be982a1b9b/docs/install/updating.md)。
