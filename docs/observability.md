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
| `observe.dropped` | `dropped`；`count` 是最近一批未投递事件数 |

事件类型只提供固定字段，不接受任意属性。访问事件可包含 Dune principal/namespace、权威 Runner/machine/Fabric、operation/suboperation、请求和决定 ID；协议事件可包含 target、角色、owner boot、incarnation、generation、route epoch 与请求 ID。它们不包含外部 subject、内部地址、错误正文、命令、环境变量、文件/Git 内容、终端字节、Agent prompt、凭据或 enrollment。

Managed lifecycle、provider 调用、续期、reconcile 和运维快照均由注入的 `pkg/managed.Service` 实现方负责，Dune 不为这些外部事实产生内建事件或持久状态。企业宿主需要把其 Managed 服务的观测接入自己的可靠性与审计系统；`host.Options.Observer` 只覆盖上述 Dune 访问与传输事件。

租约故障事件只在业务 context 仍有效时发送，正常 `Shutdown`、`Close` 或父 context 取消不会制造失败。owner 事件包含 target、原 owner boot、incarnation、generation 和 epoch，不附带 SQL/目录错误正文、数据库地址或配置指纹。事件是失败前的尽力投递，不能阻止既定的保守关闭。

`health/ready` 的 Gateway 数量和 `App.Readiness()` 适合读取当前本机快照；结构化事件适合计算连接/流变化、跨实例比例、耗时和容量拒绝。就绪探针不主动探测外部身份、权限、数据库高可用或 Managed 服务；外部系统的健康和业务完成状态必须由其所有者单独提供。
