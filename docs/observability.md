# 结构化观测

完整宿主可通过 `host.Options.Observer` 接收 `pkg/observe.Event`。宿主使用 256 项队列异步串行调用 Sink，业务路径只做非阻塞入队；`App.ObservationStatus()` 返回当前排队数和生命周期内累计丢弃数。队列从满转为可用后会补发 `observe.dropped` 及此前尚未报告的数量。应用关闭时未投递的队列项也计入丢弃。

每次 Sink 调用都带 100 ms context。Sink 必须响应取消、尽快返回，且不能从回调中等待同一个 App 关闭。Sink 由调用方持有，Dune 不关闭它。回调 panic 被隔离；慢或故障 Sink 不改变访问决定、协议结果或生命周期事务。这些事件是尽力而为的观测与审计素材；要求“审计持久化后才能执行”时，企业集成必须另行实现与业务事务一致的 outbox，不能把本出口当成可靠提交。

当前事件如下：

| name | outcome 与用途 |
| --- | --- |
| `access.check` | `allowed`、`denied`、`unavailable`；覆盖产品 API、凭据消费、首条流请求、持续操作及授权复核 |
| `gateway.connection` | `opened`、`closed`；按实际 daemon/SDK/peer 角色记录连接生命周期 |
| `gateway.route` | `online`、`replaced`、`offline`；记录本机确认路由及绑定代次变化 |
| `gateway.route_renewal` | `failed`；本地 owner term 续租失败或已过期，携带原 owner/epoch 与调用耗时 |
| `gateway.peer_dial` | `connected`、`failed`；记录目录解析后的一跳 owner 连接结果 |
| `gateway.stream` | `completed`、`rejected`、`interrupted`、`result_unknown`；`route` 区分 `local` 与 `peer` |
| `gateway.backpressure` | `session_limit`、`connection_stream_limit`、`gateway_stream_limit`；记录固定容量拒绝 |
| `managed.provider_call` | Availability 的 `read`，Create/Bootstrap/Renew/Destroy 的 `dispatch` 或 `reconcile`、Inspect 的 `read`、候选核验的 `verify`；Availability 结果为 `available`、`unavailable`，其余结果为 provider 返回的 `succeeded`、`failed`、`unknown`、`confirmed`，错误或无效值统一归一化 |
| `host.admission_renewal` | `failed` 或 `expired`；PostgreSQL 配置准入续租失败后、宿主取消前发出，`suboperation` 区分 `store`、`deadline`、`local_lease` |
| `observe.dropped` | `dropped`；`count` 是最近一批未投递事件数 |

事件类型只提供固定字段，不接受任意属性。访问事件可包含 Dune principal/namespace、权威 Runner/machine/Fabric、operation/suboperation、请求和决定 ID；协议事件可包含 target、角色、owner boot、incarnation、generation、route epoch 与请求 ID。Managed 动作事件包含调用模式、耗时、Fabric、Runner、action/operation ID 和 execution revision，Inspect 的 `revision` 表示 binding revision；Availability 事件只有 Fabric、固定结果和耗时。`resource_ref` 只来自调用前已经持久化的动作或资源，Create 返回值和人工提交的候选引用不会写入事件。它们不包含外部 subject、内部地址、错误正文、命令、环境变量、文件/Git 内容、终端字节、Agent prompt、凭据、Bootstrap token、enrollment 或提供方私有配置。

`managed.provider_call` 在适配器返回后、Dune 校验全部结果字段及提交 SQL 之前发出。它的 outcome 表示原始 provider 枚举或调用错误的保守归一化，不代表动作已经持久提交；例如 provider 报告 `succeeded` 后仍可能因字段契约或执行租约失效而被拒绝。普通错误统一为 `unknown`，deadline 为 `timed_out`，未知枚举为 `invalid`。Availability 的有效公开结果归一为 `available` 或 `unavailable`，不会记录具体固定原因或 SDK 错误。确认后的权威状态仍以 Managed Operation、action 和 resource 记录为准。

RenewalPolicy 的权威结果保存在资源维护计划中，并通过已授权的 Operation/Runner 状态返回策略版本、固定原因、观测时间、下次检查和冻结续期目标。策略错误正文不进入状态或事件；错误与非法决定分别归一为 `POLICY_ERROR`、`POLICY_INVALID` 并等待重新巡检。自定义原因属于公开状态字段，策略实现必须只返回稳定、非敏感的短代码。

租约故障事件只在业务 context 仍有效时发送，正常 `Shutdown`、`Close` 或父 context 取消不会制造失败。owner 事件包含 target、原 owner boot、incarnation、generation 和 epoch；实例准入事件只包含本次 boot ID 和固定失败阶段。两者都不附带 SQL/目录错误正文、数据库地址或配置指纹。事件是失败前的尽力投递，不能阻止既定的保守关闭。

`health/ready` 的 Gateway 数量和 `App.Readiness()` 适合读取当前本机快照；结构化事件适合计算连接/流变化、跨实例比例、耗时和容量拒绝。可信运维宿主还可调用 `App.ManagedStatusSnapshot(ctx)`：它用一个数据库时钟快照返回 Managed Runner、已知/可访问资源、unknown/timed-out Operation、当前策略下的续期积压、配置提前量内的到期风险和残留资源数量，不返回具体主体或引用。没有当前维护决定、策略版本变化、检查已到期或已有冻结续期决定都计入 backlog；访问已关闭但未 Gone 的资源，以及没有 machine 且生命周期 unknown/timed-out/failed 的资源计入 residual。调用方须自行鉴权，此方法没有普通用户 HTTP 路由。

就绪探针和 Managed 运维快照不主动探测外部身份、权限、数据库高可用或 Managed provider。经过授权的模板查询与创建会单独读取当前 Provider Availability，只用于门控新创建；它不证明已有资源健康或任何外部动作已提交。一次运维快照成功只证明该次共享数据库读取完成；既有资源状态仍由 Inspect/Reconcile 和持久结果确认。
