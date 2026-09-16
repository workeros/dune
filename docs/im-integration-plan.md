# Dune 可选 IM 模块与飞书接入方案

> 状态：目标架构已定，分阶段实现中，尚未交付。首个 Provider 为飞书；SandDance 只是未来接入方，其装配不属于本次交付。当前代码尚未提交，按目标架构直接实现，不保留旧表或接口的迁移兼容层。本文不把 IM 能力写入 Dune Gateway/fabricd 的基础协议；具体完成度以代码和测试为准。

## 目标与边界

- 在 Dune 仓库中提供可单独引入的 Go module，实现 IM Provider 接口、统一消息处理、会话路由和飞书 Provider。Dune 主 module 不反向依赖 IM module。
- 一个 Tenant 可绑定多个机器人；每个机器人绑定一个指定 Runner 上保存的 AgentConfig，并指定工作目录。不同机器人即使指向同一 AgentConfig，也不共享会话。
- 飞书机器人同时支持 WebSocket 长连接和公网事件回调，由每个机器人绑定选择接收方式；两种入口汇入同一处理链。
- 同一机器人的私聊按发送者分别保持 Agent session。群聊按话题保持 Agent session；同一话题内的不同成员共享该 session，机器人始终在话题中回复。
- 首版处理文本入站消息，出站支持可配置的流式卡片、最终消息卡片和最终文本回复。流式卡片仅支持 ACP；首版不实现 `allowed_open_ids` 白名单、卡片按钮交互、媒体、主动推送或跨平台身份合并。后续 Provider 可以增加企业微信、钉钉、QQ、微信、Telegram 等。

SandDance 负责 Tenant 与 BotBinding 管理、绑定的授权、凭据存储、持久化实现、后台运行实例编排和公网路由装配；Dune IM module 负责渠道行为和会话规则。飞书 `open_id` 只是外部发送者标识，不被转换为 Dune `identity.User`。BotBinding 只能使用 SandDance 已授权的 Runner 目标，消息内容不能选择 Runner、工作目录或执行身份。

## 来源取舍

- 借鉴 Botmux 对飞书 `root_id`、`thread_id`、首条话题消息及 `reply_in_thread` 的处理；不引入其机器人管理、卡片和权限体系。
- 借鉴 Octop/harness-gateway 的 Channel 生命周期、统一入站消息、每会话串行处理；不复制把 App Secret 放进 `config_json`、固定平台枚举或把 `tenant_id` 当 `agent_id` 使用的实现。
- 借鉴 OpenClaw 的多机器人账号和会话作用域，但**明确固定**为每机器人每私聊用户一个 session、每机器人每群话题一个 session，而不采用其默认的共享私聊/群会话。
- 参考飞书 Channel SDK 的连接生命周期、入站规范化和发送能力，但不直接把其高层 `Channel` 作为 Dune 的会话路由或回复实现：当前独立 Go Channel SDK 只提供 WebSocket 入站；默认按 chat 合并消息；其卡片 `Stream` 是普通消息 patch，不是 CardKit 原生流式模式。

参考：[Botmux 飞书路由](https://github.com/deepcoldy/botmux/blob/fed664e4ab47d5a2b5032723e950014362375f2b/src/im/lark/event-dispatcher.ts)、[Octop Channel 注册](https://github.com/TencentCloud/Octop/blob/6d6ee70deb48f6870e89dbf9ca820f7862eb6bcf/src/octop/infra/gateway/gateway.py)、[OpenClaw 会话](https://docs.openclaw.ai/concepts/session)、[OpenClaw 飞书话题](https://docs.openclaw.ai/channels/feishu/advanced-configuration)、[飞书 Channel SDK 概述](https://open.larkoffice.com/document/mcp_open_tools/integrating-agents-with-feishu/integrate-feishu-channel)、[Go Channel SDK](https://github.com/larksuite/channel-sdk-go)、[CardKit 流式更新](https://open.larkoffice.com/document/cardkit-v1/streaming-updates-openapi-overview)。飞书一键建应用和飞书 CLI 分属凭据创建、工具执行层，均不纳入本模块的 MVP。

## Go module 与接口

建议在仓库内建立独立的 `im/go.mod`（模块路径 `github.com/aiomni/dune/im`），包含 `im/channel/`、`im/feishu/` 和 `im/duneagent/`。发布时与 Dune 主 module 使用兼容版本；SandDance 显式引入，未启用 IM 时无需启动连接或 HTTP 路由。

公共绑定模型只保留跨平台字段：

```go
type BotBinding struct {
    ID, TenantID, Name string
    Provider          string
    ConfigVersion     int
    Config            json.RawMessage // 非敏感、Provider 专属配置
    CredentialRef     string          // 指向 Provider 专属 secret bundle
    Target            AgentTarget
    Enabled           bool
    Revision          int64
}

type AgentTarget struct {
    RunnerID         string
    AgentConfigID    string
    WorkingDirectory string
}
```

每个 Binding 只选择一个 AgentTarget；不要求 AgentConfigID 在不同 Binding 间唯一。`WorkingDirectory` 必须单列，因为当前 `api.AgentConfig` 不含工作目录，转换为 `api.Profile` 时才补入。更新 Binding/AgentConfig 对新建 session 生效；运行中的 session 保留创建时的目标快照，变更目标时须显式停用或迁移，不能悄悄把已有会话转给新 Agent。

Provider 的最小扩展点：

```go
type Provider interface {
    Kind() string
    Validate(config json.RawMessage, credentials []byte) error
    Open(ctx context.Context, binding BotBinding, credentials []byte, sink EventSink) (Channel, error)
}

type Channel interface {
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Send(ctx context.Context, address ReplyAddress, message OutboundMessage) (json.RawMessage, error) // Provider 专属回执
}

type StreamingChannel interface {
    OpenStream(ctx context.Context, address ReplyAddress, initial OutboundMessage) (ReplyStream, error)
}

type ReplyStream interface {
    Update(ctx context.Context, message OutboundMessage) error
    Complete(ctx context.Context, message OutboundMessage) error
}

type EventSink interface {
    Accept(ctx context.Context, message InboundMessage) error
}
```

Registry 按 `Provider.Kind()` 注册实现，不把未来平台写进封闭枚举。`InboundMessage` 包含 `BindingID`、`EventID`、`MessageID`、`SenderID`、`ChatID`、`ChatKind`、文本内容和回复地址；飞书 `root_id`/`thread_id` 只在 Provider 专属、有版本的 `ReplyAddress.Data` 中，不进入公共事件字段。`ReplyAddress` 由 Provider 编解码和校验，公共层只保存非敏感载荷。公共层只有一个入站事件接口 `EventSink.Accept`；飞书 WebSocket 与 HTTP 回调的生命周期、验真、解密及协议解析都封装在 `im/feishu`，两种入口最终调用同一入站事件处理函数，再触发 `EventSink.Accept`。HTTP `CallbackHandler()` 是飞书 Channel 的 Provider 专属能力，不在公共 `channel` 包增加回调接口。

`Send` 用于最终回复；可选 `StreamingChannel` 表示 Provider 能在同一条回复上连续更新。`OutboundMessage` 携带稳定 `DeliveryID` 和已路由的 `SessionKey`；`ReplyStream.Update` 接收当前**累计全文**，由单个会话处理器串行调用。公共 `DeliveryStore` 为三种回复模式原子预留 `(BindingID, DeliveryID)` 并对版本做 CAS，记录会话、回复地址、模式、阶段、待处理操作和结果未知状态；公共 `DeliveryManager` 与存储边界共同约束 `reserved → pending → active/complete/unknown`，不可变更会话、回复地址、模式或 Provider 状态版本，也不能跳过持久意图。Provider 自己定义带版本的 `provider_state`：飞书保存 CardKit card ID、message ID、sequence 与已确认正文，其他平台不承担 CardKit 字段或流式能力。任何外部投递操作先持久化意图；结果未知时先核查，不自动重建或重发。公共 AgentBackend 将输出归一为 `Delta`、`Final`、`Error` 事件；是否有可靠增量输出是 AgentBackend 的能力，不由飞书 Provider 推断。

公共 Registry 按 Provider Kind 解析实现；飞书 `Service` 提供 Tenant 多 Binding 启停、配置同步与只读 `Status`、`Issues`。状态区分 callback handler 已就绪、WebSocket 连接中/已连接/重连中/失败和持久队列的 queued/claimed/submitting/failed/unknown 数量；`callback_ready` 不证明公网可达。`Issues` 按 Tenant 限定 Binding，分别有界列出待核查的入站事件、会话和投递，包含稳定 ID、阶段、操作与错误，但不含原始入站正文或凭据；它不负责确认结果或重放。对于投递记录已确认为 `complete`、但 Inbox 与会话都留在 `unknown` 的同一回合，`ReconcileConfirmedDelivery` 会核对 `SessionKey`、`EventID` 和稳定 `DeliveryID`，在同一事务中完成 Inbox 并解锁会话，绝不再次调用 Agent 或平台 API；其余不确定结果继续保持 `unknown`。SQLite Inbox 对每个 Binding 的待处理事件设容量上限（默认 10000）、单事件大小上限（默认 256 KiB）和领取次数上限（默认 5 次）；领取失败或领取租约过期达到上限时转为可观测的 `failed`，不再立即重试。队列满时已存事件仍幂等确认，新事件返回错误供平台重投。入站队列与同 session 串行性由持久存储和租约保证，跨 session 可并行；多实例部署时需要共享存储，不能只依赖本进程 mutex。

## 飞书配置与两种入口

```go
type FeishuConfig struct {
    AppID       string // cli_...
    Site        string // feishu 或 lark
    ReceiveMode string // websocket 或 callback
    ReplyMode   string // streaming_card、final_card 或 final_text
}

type FeishuCredentials struct {
    AppSecret         string
    EncryptKey        string
    VerificationToken string
}
```

App ID 属于可展示配置；其余字段通过 `CredentialRef` 解析，API 不回传明文。`AppSecret` 两种方式均必需；回调方式按飞书应用的加密/校验配置要求校验 `EncryptKey` 和 `VerificationToken`，首版配置回调时要求提供两者。可用简单的 SandDance secret store 实现 `CredentialResolver`，不为 MVP 建立额外密钥管理平台。

- WebSocket：每个启用的 Binding 建一个飞书底层 Go SDK `ws.Client` 和事件分发器，负责重连和状态上报。成功持久接收事件后才向 SDK 返回成功；无法保存时返回错误/断开以促使重投，不在内存中静默丢弃。
- 公网回调：SandDance 将该 Binding 的 handler 挂到独立路径，使用底层 Go SDK 的 HTTP 事件能力，限制 body 大小，完成 URL verification、签名/时间窗口校验、token 校验和解密；仅在事件已可靠写入后返回成功，不等待 Agent 运行完成。未验证的请求不可入队。路径不是身份凭据。独立 `channel-sdk-go` 当前不提供 webhook adapter，不以它承诺双接入。
- 两个入口只差传输适配，均处理 `im.message.receive_v1`，在 Dune 飞书 Provider 中共用规范化函数并调用同一个 `EventSink.Accept`。去重、会话路由、Agent 和回复编排位于 Provider 之外的公共处理链。一个 Binding 同时只能选择一种入口。必须逐条保留原始 `root_id`/`thread_id` 与发送者；不得启用按 chat 合并消息的高层默认批处理，否则不同话题可能被合并。
- 发送调用飞书底层消息 API。私聊回复到对应 `open_id`；群聊调用消息 reply API 并设置 `reply_in_thread=true`，锚定原话题。两种消息 API 均用 `BindingID + DeliveryID + 操作名` 派生稳定 `uuid`；[飞书底层 Go SDK](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/service/im/v1/model.go)记录的消息去重窗口只有一小时，因此它是额外防重保护，**不是**结果未知后跨时段自动重发的依据。若话题回复失败，报告投递失败，**不得自动退化为群顶层发送**。高层 Channel `Send` 在回复目标失效时会自动改发新消息，且其 `SendInput` 不暴露 `reply_in_thread`，故不用于群话题出站。

### 回复模式

`ReplyMode` 是每个 BotBinding 的飞书配置，默认 `final_text`；配置加载时验证合法值与 AgentBackend 能力，不按失败情况悄悄切换模式。三种模式使用相同的私聊/群话题 `ReplyAddress`：

| 模式 | 发送时机与行为 | Agent 要求 |
| --- | --- | --- |
| `streaming_card` 流式卡片 | 回合开始时创建启用 CardKit `streaming_mode` 的卡片实体，将其作为消息发到目标私聊或群话题；只消费 ACP 的 assistant 正文增量，对**同一卡片实体**持续写入累计正文；完成时写入完整正文和状态，并显式关闭流式模式，不再另发一遍答案。失败时尽可能在原卡片标明失败并关闭流式模式。 | **仅 ACP**，且必须提供可识别增量和完成的事件流；PTY 配置此模式应直接报配置错误。 |
| `final_card` 最终消息卡片 | 等 Agent 回合完成后，将完整答案渲染为一张最终卡片发送；过程中不创建占位卡片。 | 首版 ACP；未来其他后端只有能可靠识别回合完成时才可使用。 |
| `final_text` 最终文本回复 | 等 Agent 回合完成后，发送普通文本消息；过程中不创建占位消息。 | 同上。 |

流式卡片的 ACP 事件过滤必须区分 assistant 正文、思考/工具事件和最终结果；不得把思考内容或重复的最终全文直接追加到已累计的 delta。CardKit 文本更新每次提交的是**当前累计全文**，增量期间仅接受前缀扩展以保持打字效果；若 ACP 最终正文修订了先前增量，应在同一卡片上以最终正文覆盖，而不是判定失败或另发消息。更新按消息顺序串行、节流合并并控制在卡片实体更新限制内。完成和失败均应显式关闭 `streaming_mode`，不能将 SDK `StreamController.Close` 当成 CardKit 关闭操作。处理卡片大小及限流约束；超限时只能在**同一会话和话题内**继续投递，不能转发到群顶层。创建卡片实体及发送消息分别记录状态，持久保存 card ID、message ID、已确认累计正文/更新进度和完成状态；断线后先核查卡片/投递状态，更新或完成结果未知时不盲目重建卡片。若最初创建或发送失败，则记录投递失败，不自动改为其他回复模式。

协议实现以[飞书官方底层 Go SDK](https://github.com/larksuite/oapi-sdk-go)及 CardKit OpenAPI 为基础；高层 [Channel `Stream`/`UpdateCard`/`Close`](https://github.com/larksuite/channel-sdk-go/blob/main/docs/streaming.md) 使用普通消息 patch，**不是** CardKit 原生流式更新，不用于 `streaming_card`。CardKit 原生流程见[流式更新文档](https://open.larkoffice.com/document/cardkit-v1/streaming-updates-openapi-overview)：创建卡片实体、发送引用该实体的互动消息、更新卡片元素内容、关闭流式模式。回调 handler 可参考[底层 SDK 的 HTTP 事件示例](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/sample/event/event.go)。群话题中引用 CardKit 卡片实体并以 `reply_in_thread=true` 回复的组合，须在真实飞书环境验证；验证不通过不能声称群话题流式卡片已交付，也不能悄悄改为顶层发送。

## 会话路由与回复规则

内部 `SessionKey` 严格由 `TenantID + BindingID + ChatID + SubjectID` 生成，不含平台概念或消息类型；由结构化字段编码或哈希生成，不能用无转义字符串拼接。私聊 `SubjectID` 是用户标识，群聊是 Provider 解析的稳定话题锚点。

| 飞书消息 | session key 的业务字段 | 触发与回复 |
| --- | --- | --- |
| 用户 A 与机器人私聊 | `ChatID + SubjectID=sender_open_id(A)` | 每条消息进入 A 的同一 session，回复 A 私聊 |
| 用户 B 与同一机器人私聊 | `ChatID + SubjectID=sender_open_id(B)` | 与 A 完全独立，回复 B 私聊 |
| 群 G 话题 T，任意成员发言 | `ChatID=G + SubjectID=canonical_root(T)` | 同一话题共用 session，回复该话题 |
| 群 G 新话题 U | `ChatID=G + SubjectID=canonical_root(U)` | 新 session，回复 U 话题 |

顶层群消息须 `@` 当前机器人才触发；机器人已经参与的话题中，后续真人消息不必反复 `@`。忽略机器人自己的消息和无法确定来源的事件，避免回复环。MVP 不设发送者白名单。群话题的 session key **不包含 sender**，而私聊 key 必须包含发送者。

飞书 Provider 采用根消息 ID 作为群会话锚点，按如下次序解析：

1. 首条顶层群消息无 `thread_id` 时，以其 `message_id` 为 `canonical_root`，首次回复使用 `reply_in_thread=true` 创建/进入话题。
2. 已确认的话题回复以 `root_id` 定位；不因为本地没有记录就查询飞书。若同时有 `thread_id`，在确保或确认会话时原子绑定 `provider_thread_ref`。
3. 只有 `thread_id` 时先按 `(BindingID, ChatID, provider_thread_ref)` 查本地会话；未命中则以当前 `message_id` 查询飞书并核实根消息。若核实当前消息本身就是无 parent 的话题根，可用其 `message_id`；仍无法确认时拒绝路由，不创建猜测性的 session。
4. 仅有 `root_id`、没有 `thread_id` 的普通引用/快捷回复，不直接判定为话题；由飞书 Provider 查询消息确认。

`im_conversations.provider_thread_ref` 可空，在 `(binding_id, chat_id, provider_thread_ref)` 上有唯一索引；它是辅助查找字段，不参与 `SessionKey`。同一会话重复绑定幂等，指向其他会话则报冲突。其他 Provider 可使用，也可留空。不存在独立的飞书别名表或公共别名接口。

自然群话题的 `thread_id`（通常 `omt_*`）和普通群消息根 `message_id`（通常 `om_*`）可能不同，不得直接把首次消息和后续消息路由到不同 session。两位用户在同一群话题发言时会共享上下文；这符合话题语义，但不代表他们共享私聊上下文。

## Agent 生命周期与恢复

现状：[RunnerExecutor](../pkg/host/executor.go)只支持 Environment 的 `Prepare/Status`；[托管 ACP 控制器](../pkg/fabricd/acp.go)在一个 Runtime 中只持有一个当前 ACP session。现在新增的 `host.AgentExecutor` 为 IM 提供经过 `BackgroundRunner` 授权、仍经 Gateway 的后台连接；每个 conversation 仍须有独立 Runtime，不复用单一 Runtime 服务所有用户。

`im/duneagent` 通过一个窄 `AgentBackend` 接口获取目标 AgentConfig、生成 `Profile(kind=agent, adapter=acp, managed_acp=true)`、启动/附着 Runtime，并执行 ACP `new/load/prompt/state`。一个 IM conversation 对应一个独立的 Runtime/ACP session；该 session 的消息串行提交。Profile 合法性在提交屏障前检查；可选 `AgentInputValidator` 在同一边界预检确定性的输入错误，Dune ACP 适配器按 fabricd 的 64 KiB prompt 上限检查，避免把已知拒绝误记成结果未知。ACP 的更新事件只将 `agent_message_chunk`/`agent_message` 文本映射为有序正文 delta，思考、工具和用户消息被过滤；`stopReason`/完成状态结束本回合，流式卡片不能仅凭 PTY 静默判断“已完成”。`Start`、`Attach`、`Input/Prompt`、`Stop` 分别表示启动、重连观察/控制、提交新回合和显式销毁；断开连接不等于 Stop。当前适配器对仍存活且身份匹配的 Runtime 执行 Attach；仅在旧 Runtime 明确返回 `STALE_RUNTIME` 或已退出时新启 Runtime，若 Agent 广告 `loadSession` 则加载原 ACP session；否则新建 session，并在同一回复提示上下文未恢复。网络错误等不确定查询不触发自动重建。

后台入口由 SandDance 以已授权的 Tenant/Runner 作用域调用；不要求飞书发送者拥有 Dune Web 身份，也不伪造 `identity.User`。宿主提供的是绑定所属的受信任 actor，绝非事件发送者身份；Dune host 根据 RunnerID 读取当前 Runner Binding，再检查 Runner 归属、当前 binding/incarnation/generation 和操作权限。当前 `host.AgentExecutor` 已按此路径实现并通过本地假 ACP 子进程穿过 Gateway 的测试；SandDance 提供实际受信任 actor 的装配尚未完成。

会话记录保存 session key、目标快照、当前 Runtime 身份、ACP session ID、回复地址版本及处理状态。进程仍存活时优先 Attach；进程已失效时，只有 Agent 声明并成功执行 ACP `session/load` 才能恢复原对话。否则明确建立新 session 并告知用户上下文未恢复，不假定 Dune 内存状态可跨重启恢复。AgentConfig 共享不等于文件隔离；若 SandDance 需要用户间文件隔离，应给不同用户分配独立工作目录或 Runner。

## 可靠性与状态归属

SandDance 提供 `BindingStore`、`CredentialResolver`、`ConversationStore` 和 `InboxStore` 的持久实现；Dune IM module 定义最小接口并使用它们。首版单实例可用 SQLite，集群接入时应使用共享数据库和跨实例 session 租约。至少持久化：

- BotBinding 及配置版本；
- ConversationSession、Runtime/ACP 标识和可空的 Provider 话题引用；
- InboundEvent 的唯一 `(BindingID, EventID)` 或消息 ID、处理状态与错误；
- 三种模式共用的 `im_deliveries` 投递状态；飞书 CardKit 细节存于带版本的 `provider_state`，防止重复建卡、重复回复或遗留未关闭卡片。

入站流程为：验真/解密 → 去重并可靠入队 → 快速 ACK → 按 session 串行领取 → Agent prompt → 按 ReplyMode 更新流式卡片或发送最终回复 → 记录结果。公共 `channel.Ingress` 实现唯一的 `EventSink.Accept`，只负责调用宿主的持久 `InboxStore.Insert`，不在飞书长连接/回调处理函数内运行 Agent。飞书 `Service.LoadTenant` 同步该 Tenant 的多个机器人，停用的 Binding 必须关闭连接；`Service.Run` 启动有界数量的工作者，同一个 Service 不得重复启动工作循环，外部 `Stop` 会取消它们；已挂载的回调路由每次请求都解析当前 Channel，凭据轮换后不得继续由旧处理器验签。对同一机器人、同一群，尚在 `claimed` 入场判定阶段的早先消息会暂时挡住后续消息领取，避免后续未再次 `@` 的话题消息抢先被误忽略；首条消息进入 `submitting` 后，各话题仍可独立处理。事件尚未提交到 Agent 前可按状态重试；`WorkQueue.BeginSubmission` 必须在调用 Agent 前落库，之后若连接中断且执行结果未知，标为 `unknown` 并核查状态，**不自动重放 prompt**。飞书发送结果未知时也不盲目再发。队列容量、超时和重试次数均需有界，并暴露 Binding 连接、积压、失败和结果未知状态。

独立 module 现在提供 `im/sqlite` 作为单宿主持久化基础，最终仅有 `im_bindings`、`im_inbox`、`im_conversations`、`im_deliveries` 四张表：Tenant 作用域内多 BotBinding 的版本化存储、Inbox 事件去重/领取/提交边界、会话及其 Agent 目标快照/Runtime 标识、跨 worker 的会话租约、可空话题引用和通用投递状态 CAS。闲置租约过期可以接管；`running` 或 `unknown` 回合不会自动接管。公共 `channel.Processor` 已通过假 Agent/Channel 测试验证私聊隔离、群话题续聊、ACP 增量驱动卡片和未知 prompt 不重放；飞书新话题入场通过 bot/v3/info 查询当前机器人 Open ID 后匹配结构化 @。飞书 Service 的本地假 API 整链测试覆盖签名回调、持久入队、Run worker、两个私聊用户隔离与同用户 Attach 续聊，也覆盖群顶层 `@` 入场、另一成员同 thread 续聊、新话题隔离和各轮 `reply_in_thread` 回复；私聊测试还核实每轮投递持久完成。`im/duneagent` 已接到受信任 host 入口并通过模拟 ACP 协议事件测试覆盖 Attach 与失效 Runtime 恢复；host 入口另以本地假 ACP 子进程验证 Gateway 上的 new/prompt/load。已实现“投递持久完成而收尾未知”这一确定结果的核查恢复；其他未知结果、真实 Agent 行为和真实飞书验收仍未完成。后续接入方可复用 SQLite 或提供满足相同接口的共享存储。

## 交付顺序与验收

1. 定义独立 Go module、Provider/Store/AgentBackend 接口和标准事件；用假 Provider/Backend 验证 session key、跨 Binding 隔离及会话串行。
2. 增加受信任后台 Agent 执行入口；用 mock ACP 验证每个 conversation 独立 Runtime、Attach、`session/load` 支持判断、目标变更与未知结果处理。
3. 基于底层飞书 SDK 实现 WebSocket、三种回复模式和群 `reply_in_thread`；验证两人私聊互不串上下文、同群不同话题不被按 chat 合并、同话题多人续聊、首轮回复后不跳 session，以及 ACP 流式卡片只更新原 CardKit 实体、关闭流式模式、不重复发送最终答案。真实飞书环境必须验证 CardKit 卡片实体在 `reply_in_thread` 下的发送和更新。
4. 实现公网回调的校验/解密、可靠 ACK 和与 WebSocket 共用的处理链；验证重复事件、回调重投、重启和凭据轮换。

SandDance 的 Tenant/BotBinding 管理、secret/store、HTTP 路由和运营状态装配是后续接入工作，不是本次 Dune IM module 的完成条件。本次仍须验证飞书 Provider 的 WebSocket 和公网回调行为；有真实飞书环境时再做平台端到端验收。

三种回复模式都要分别覆盖私聊和群话题；`streaming_card` 配 PTY 必须在启动前报错。不可把 mock ACP 或本地假 ACP 测试称为真实 Agent 验收；跨进程重启、飞书回调重试、真实话题和真实卡片更新必须单独验证。本文定义 Dune 模块的目标；当前代码已覆盖部分公共接口、飞书双入口的入站适配、话题根解析、普通最终回复、CardKit 流式控制器、会话租约、受信任 Dune ACP 入口、受限的失效 Runtime 恢复和单宿主 SQLite 持久化基础，但真实飞书验收仍未完成。
