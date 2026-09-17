# Dune 可选 IM 模块与飞书接入方案

> 状态：本文范围内的模块实现、自动化回归和当前环境可进行的本地端到端验收已完成（2026-09-17）。真实 Agent 和飞书平台验收因未配置验收环境而未执行，不能据此声称平台上的群话题 CardKit 已验收；版本发布也尚未进行。首个 Provider 为飞书；SandDance 装配不属于本次交付。按目标架构直接实现，不保留旧表或接口的迁移兼容层，也不扩展 Dune Gateway/fabricd 基础协议。接入、生命周期与平台验收步骤见 [IM README](../im/README.md)。

## 目标与边界

- 在 Dune 仓库中提供可单独引入的 Go module，实现 IM Provider 接口、统一消息处理、会话路由和飞书 Provider。Dune 主 module 不反向依赖 IM module。
- 一个 Tenant 可绑定多个机器人；每个机器人绑定一个指定 Runner 和宿主管理的 ACP Agent Profile 修订，并指定工作目录。不同机器人即使指向同一 Agent Profile，也不共享会话。
- 飞书机器人同时支持 WebSocket 长连接和公网事件回调，由每个机器人绑定选择接收方式；两种入口汇入同一处理链。
- 同一机器人的私聊按发送者分别保持 Agent session。群聊按话题保持 Agent session；同一话题内的不同成员共享该 session，机器人始终在话题中回复。
- 首版处理文本入站消息，出站支持可配置的流式卡片、最终消息卡片和最终文本回复。流式卡片仅支持 ACP；首版不实现 `allowed_open_ids` 白名单、卡片按钮交互、媒体、主动推送或跨平台身份合并。后续 Provider 可以增加企业微信、钉钉、QQ、微信、Telegram 等。

接入方负责 Tenant 与 BotBinding 管理、绑定的授权、凭据存储、后台运行实例编排和公网路由装配；Dune IM module 提供渠道行为、会话规则、持久化接口及可直接复用的单宿主 SQLite 实现。本次不实现 SandDance 装配。飞书 `open_id` 只是外部发送者标识，不被转换为 Dune `identity.User`。BotBinding 只能使用接入方已授权的 Runner 目标，消息内容不能选择 Runner、工作目录或执行身份。

## 来源取舍

- 借鉴 Botmux 对飞书 `root_id`、`thread_id`、首条话题消息及 `reply_in_thread` 的处理；不引入其机器人管理、卡片和权限体系。
- 借鉴 Octop/harness-gateway 的 Channel 生命周期、统一入站消息、每会话串行处理；不复制把 App Secret 放进 `config_json`、固定平台枚举或把 `tenant_id` 当 `agent_id` 使用的实现。
- 借鉴 OpenClaw 的多机器人账号和会话作用域，但**明确固定**为每机器人每私聊用户一个 session、每机器人每群话题一个 session，而不采用其默认的共享私聊/群会话。
- 参考飞书 Channel SDK 的连接生命周期、入站规范化和发送能力，但不直接把其高层 `Channel` 作为 Dune 的会话路由或回复实现：当前独立 Go Channel SDK 只提供 WebSocket 入站；默认按 chat 合并消息；其卡片 `Stream` 是普通消息 patch，不是 CardKit 原生流式模式。

参考：[Botmux 飞书路由](https://github.com/deepcoldy/botmux/blob/fed664e4ab47d5a2b5032723e950014362375f2b/src/im/lark/event-dispatcher.ts)、[Octop Channel 注册](https://github.com/TencentCloud/Octop/blob/6d6ee70deb48f6870e89dbf9ca820f7862eb6bcf/src/octop/infra/gateway/gateway.py)、[OpenClaw 会话](https://docs.openclaw.ai/concepts/session)、[OpenClaw 飞书话题](https://docs.openclaw.ai/channels/feishu/advanced-configuration)、[飞书 Channel SDK 概述](https://open.larkoffice.com/document/mcp_open_tools/integrating-agents-with-feishu/integrate-feishu-channel)、[Go Channel SDK](https://github.com/larksuite/channel-sdk-go)、[CardKit 流式更新](https://open.larkoffice.com/document/cardkit-v1/streaming-updates-openapi-overview)。飞书一键建应用和飞书 CLI 分属凭据创建、工具执行层，均不纳入本模块的 MVP。

## Go module 与接口

在仓库内建立独立的 `im/go.mod`（模块路径 `github.com/aiomni/dune/im`），包含 `im/channel/`、`im/feishu/`、`im/duneagent/` 和 `im/sqlite/`。发布时与 Dune 主 module 使用兼容版本；接入方显式引入，未启用 IM 时无需启动连接或 HTTP 路由。

当前 `im/go.mod` 为仓库内开发使用 `replace github.com/aiomni/dune => ..`，并以占位版本依赖主 module；Go 不会将依赖模块的 `replace` 传递给外部接入方。正式对外引入前，必须先发布包含后台 Agent 入口的 Dune 主 module 版本，再让 IM module 依赖该实际版本并发布匹配的子模块版本；当前测试通过只证明同仓库开发状态，不证明外部可直接 `go get`。

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
    ProfileID        string
    ProfileRevision  int64
    WorkingDirectory string
}
```

每个 Binding 选择一个 AgentTarget，固定宿主空间中的 Profile ID、正整数修订和执行目录。完整 Profile 由宿主按 Tenant 授权解析；工作目录允许覆盖 Profile 默认值。更新 Profile 不改变已接受的绑定修订；尚未启动 Runtime 的空会话可在下一次路由时采用当前绑定目标，已有 Runtime/ACP session 的会话保留原目标。

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

Registry 按 `Provider.Kind()` 注册实现，不把未来平台写进封闭枚举。`InboundMessage` 包含 `BindingID`、接收时的 `BindingRevision`、`EventID`、`MessageID`、`SenderID`、`ChatID`、`ChatKind`、Provider 标识空间内的 `MentionedIDs`、文本内容和回复地址；飞书 `root_id`/`thread_id` 只在 Provider 专属、有版本的 `ReplyAddress.Data` 中，不进入公共事件字段。`ReplyAddress` 由 Provider 编解码和校验，公共层只保存非敏感载荷。公共层只有一个入站事件接口 `EventSink.Accept`；飞书 WebSocket 与 HTTP 回调的生命周期、验真、解密及协议解析都封装在 `im/feishu`，两种入口最终调用同一入站事件处理函数，再触发 `EventSink.Accept`。HTTP `CallbackHandler()` 是飞书 Channel 的 Provider 专属能力，不在公共 `channel` 包增加回调接口。

`Send` 用于最终回复；可选 `StreamingChannel` 表示 Provider 能在同一条回复上连续更新。`OutboundMessage` 携带稳定 `DeliveryID` 和已路由的 `SessionKey`；`ReplyStream.Update` 接收当前**累计全文**，由单个会话处理器串行调用。公共 `DeliveryStore` 为三种回复模式原子预留 `(BindingID, DeliveryID)` 并对版本做 CAS，记录会话、回复地址、模式、阶段、待处理操作和结果未知状态；公共 `DeliveryManager` 与存储边界共同约束 `reserved → pending → active/complete/unknown/failed`，不可变更会话、回复地址、模式或 Provider 状态版本，也不能跳过持久意图。`failed` 仅用于 Provider 明确证明没有发起平台请求的本地拒绝；已发出请求而结果不明仍必须是 `unknown`。投递记录另存 `AgentTurnCompleted`：流式卡片即使在 Agent 结果未知时成功关闭并送达失败提示，也不能被视为 Agent 回合完成；恢复会话须同时具备可靠 Agent 最终结果和已确认的完整投递。Provider 自己定义带版本的 `provider_state`：飞书保存 CardKit card ID、message ID、sequence 与已确认正文，其他平台不承担 CardKit 字段或流式能力。任何外部投递操作先持久化意图；结果未知时先核查，不自动重建或重发。公共 AgentBackend 将输出归一为 `Delta`、`Final`、`Error` 事件；是否有可靠增量输出是 AgentBackend 的能力，不由飞书 Provider 推断。

公共 Registry 按 Provider Kind 解析实现；飞书 `Service` 提供 Tenant 多 Binding 启停、配置同步与只读 `Status`、`Issues`。状态区分 callback handler 已就绪、WebSocket 连接中/已连接/重连中/失败和持久队列的 queued/claimed/submitting/failed/unknown 数量；`callback_ready` 不证明公网可达。`Issues` 按 Tenant 限定 Binding，分别有界列出待核查的入站事件、会话和投递，包含稳定 ID、阶段、操作和脱敏错误摘要，但不含原始入站正文、Agent 回复正文、回复地址、Provider 私有状态或凭据；它不负责确认结果或重放。对于同一回合的投递记录已确认为 `complete` **且** `AgentTurnCompleted=true`，但 Inbox/会话收尾未完成的情况，`ReconcileConfirmedDelivery` 核对 `SessionKey`、`EventID` 与稳定 `DeliveryID`，在事务中完成 `submitting` 或 `unknown` Inbox 并解锁会话；如果会话仍为 `running`，还要求 worker 租约已过期。这样可恢复“平台已确认送达、进程在 Inbox 收尾前退出”的窗口。它绝不再次调用 Agent 或平台 API；没有完成证据的结果继续保持隔离。SQLite Inbox 对每个 Binding 的待处理事件设容量上限（默认 10000）、单事件大小上限（默认 256 KiB）和领取次数上限（默认 5 次）；领取失败或领取租约过期达到上限时转为可观测的 `failed`，不再立即重试。队列满时已存事件仍幂等确认，新事件返回错误供平台重投。入站队列与同 session 串行性由持久存储和租约保证，跨 session 可并行；多实例部署时需要共享存储，不能只依赖本进程 mutex。

## 飞书配置与两种入口

最终文本或最终卡片超出[飞书发送消息 API](https://open.feishu.cn/document/server-docs/im-v1/message/create) 的单条请求体限制（文本 150 KB、卡片 30 KB）时，飞书 Provider 在发出请求前返回 `ErrOutboundRejected`；校验对象是包含转义后 `content`、接收者或话题参数、`uuid` 的**完整序列化请求体**，不是内层卡片/文本 JSON。公共层将 Delivery 与 Inbox 记为 `failed`，释放已完成的 Agent 会话并在 `Status`/`Issues` 中保留失败证据，不把它误记为飞书结果未知。若进程在持久化拒绝后、收尾前退出，`ReconcileRejectedDelivery` 只核对同一 `SessionKey`、`EventID`、稳定 `DeliveryID`、`AgentTurnCompleted=true` 和过期的活动租约，不重放 Agent 或发送。当前最终两种模式仍是单条消息：不会静默截断、拆分或切换模式；超长答案的后续产品策略尚待确认。

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

飞书配置目前只支持 `ConfigVersion=1`，未知版本在 Provider 打开前拒绝，不能按旧结构猜测解析。App ID 属于可展示配置；其余字段通过 `CredentialRef` 解析，API 不回传明文。`AppSecret` 两种方式均必需；回调方式按飞书应用的加密/校验配置要求校验 `EncryptKey` 和 `VerificationToken`，首版配置回调时要求提供两者。接入方可用简单的 secret store 实现 `CredentialResolver`，不为 MVP 建立额外密钥管理平台。

- WebSocket：每个启用的 Binding 建一个飞书底层 Go SDK `ws.Client` 和事件分发器，负责重连和状态上报。成功持久接收事件后才向 SDK 返回成功；无法保存时返回错误/断开以促使重投，不在内存中静默丢弃。
- 公网回调：接入方将该 Binding 的 handler 挂到独立路径；飞书 Provider 使用底层 Go SDK 的签名与解密能力实现 HTTP 事件入口，限制 body 大小，完成 URL verification、签名/时间窗口校验、token 校验和解密；仅在事件已可靠写入后返回成功，不等待 Agent 运行完成。未验证的请求不可入队。路径不是身份凭据。独立 `channel-sdk-go` 当前不提供 webhook adapter，不以它承诺双接入。
- 两个入口只差传输适配，均处理 `im.message.receive_v1`，在 Dune 飞书 Provider 中共用规范化函数并调用同一个 `EventSink.Accept`。去重、会话路由、Agent 和回复编排位于 Provider 之外的公共处理链。一个 Binding 同时只能选择一种入口。必须逐条保留原始 `root_id`/`thread_id` 与发送者；不得启用按 chat 合并消息的高层默认批处理，否则不同话题可能被合并。
- 发送调用飞书底层消息 API。私聊回复到对应 `open_id`；群聊调用消息 reply API 并设置 `reply_in_thread=true`，锚定原话题。两种消息 API 均用 `BindingID + DeliveryID + 操作名` 派生稳定 `uuid`；[飞书底层 Go SDK](https://github.com/larksuite/oapi-sdk-go/blob/v3_main/service/im/v1/model.go)记录的消息去重窗口只有一小时，因此它是额外防重保护，**不是**结果未知后跨时段自动重发的依据。若话题回复失败，报告投递失败，**不得自动退化为群顶层发送**。高层 Channel `Send` 在回复目标失效时会自动改发新消息，且其 `SendInput` 不暴露 `reply_in_thread`，故不用于群话题出站。

### 回复模式

`ReplyMode` 是每个 BotBinding 的飞书配置，默认 `final_text`；配置加载时验证合法值。首版三种模式在激活前都验证 AgentBackend 支持 ACP 和可靠最终状态，`streaming_card` 另需有序 assistant delta；最终回复模式不要求增量。若所选 Profile 修订不可访问或不是 ACP，下一次同步必须拒绝激活机器人；不能先接收事件再失败，也不按失败情况悄悄切换模式。三种模式使用相同的私聊/群话题 `ReplyAddress`：

| 模式 | 发送时机与行为 | Agent 要求 |
| --- | --- | --- |
| `streaming_card` 流式卡片 | 回合开始时创建启用 CardKit `streaming_mode` 且允许共享更新（`update_multi=true`）的卡片实体，将其作为消息发到目标私聊或群话题；只消费 ACP 的 assistant 正文增量，在单卡容量内对**同一卡片实体**持续写入累计正文；完成时写入最终正文并显式关闭流式模式，不再重复发送答案。最终正文超过单卡容量时，先关闭当前卡片，再在同一私聊或群话题内发送续卡，各卡仅包含对应片段。 | **仅 ACP**，且必须提供可识别增量和完成的事件流；PTY 配置此模式应直接报配置错误。 |
| `final_card` 最终消息卡片 | 等 Agent 回合完成后，将完整答案渲染为一张最终卡片发送；过程中不创建占位卡片。 | 首版 ACP；未来其他后端只有能可靠识别回合完成时才可使用。 |
| `final_text` 最终文本回复 | 等 Agent 回合完成后，发送普通文本消息；过程中不创建占位消息。 | 同上。 |

流式卡片的 ACP 事件过滤必须区分 assistant 正文、思考/工具事件和最终结果；不得把思考内容或重复的最终全文直接追加到已累计的 delta。CardKit 文本更新每次提交的是**当前卡片的累计全文**，增量期间仅接受前缀扩展以保持打字效果；若 ACP 最终正文修订了先前增量，应以最终正文覆盖。更新按消息顺序串行、节流合并并控制在卡片实体更新限制内。当前实现对**完整序列化更新请求体**采用保守的单次 28 KiB 本地预算（不是声称 CardKit 官方上限）；实时增量只显示首卡容量内的前缀，最终正文按 JSON 转义后的请求大小和 UTF-8 字符边界拆分，超限续卡仍发往同一私聊或群话题，不能转发到群顶层。每张卡片都显式关闭 `streaming_mode`，不能将 SDK `StreamController.Close` 当成 CardKit 关闭操作。创建卡片实体及发送消息分别记录状态，持久保存各卡 card ID、message ID、已确认正文/更新进度和完成状态；断线后先核查卡片/投递状态，更新或完成结果未知时不盲目重建卡片。若最初创建或发送的远端结果无法确认，则记录 `unknown` 并要求核查，不自动改为其他回复模式。

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
2. 已确认的话题回复以 `root_id` 定位；不因为本地没有记录就查询飞书。若同时有 `thread_id`，先核对已有本地话题引用是否指向同一根消息，冲突则拒绝路由；在确保或确认会话时原子绑定 `provider_thread_ref`。
3. 只有 `thread_id` 时先按 `(BindingID, ChatID, provider_thread_ref)` 查本地会话；未命中则以当前 `message_id` 查询飞书并核实根消息。若核实当前消息本身就是无 parent 的话题根，可用其 `message_id`；仍无法确认时拒绝路由，不创建猜测性的 session。
4. 仅有 `root_id`、没有 `thread_id` 的普通引用/快捷回复，不直接判定为话题；由飞书 Provider 查询消息确认。

`im_conversations.provider_thread_ref` 可空，在 `(binding_id, chat_id, provider_thread_ref)` 上有唯一索引；它是辅助查找字段，不参与 `SessionKey`。同一会话重复绑定幂等，指向其他会话则报冲突。其他 Provider 可使用，也可留空。不存在独立的飞书别名表或公共别名接口。

自然群话题的 `thread_id`（通常 `omt_*`）和普通群消息根 `message_id`（通常 `om_*`）可能不同，不得直接把首次消息和后续消息路由到不同 session。两位用户在同一群话题发言时会共享上下文；这符合话题语义，但不代表他们共享私聊上下文。

## Agent 生命周期与恢复

现状：[RunnerExecutor](../pkg/host/executor.go)只支持 Environment 的 `Prepare/Status`；[托管 ACP 控制器](../pkg/fabricd/acp.go)在一个 Runtime 中只持有一个当前 ACP session。现在新增的 `host.AgentExecutor` 为 IM 提供经过 `BackgroundRunner` 授权、仍经 Gateway 的后台连接；每个 conversation 仍须有独立 Runtime，不复用单一 Runtime 服务所有用户。

`im/duneagent` 通过一个窄 `AgentBackend` 接口通过宿主 `ProfileResolver` 获取目标的不可变完整 Agent Profile、校验 `kind=agent, adapter=acp` 并设置 `managed_acp=true`、启动/附着 Runtime，并执行 ACP `new/load/prompt/state`。一个 IM conversation 对应一个独立的 Runtime/ACP session；该 session 的消息串行提交。Profile 合法性在提交屏障前检查；可选 `AgentInputValidator` 在同一边界预检确定性的输入错误，Dune ACP 适配器按 fabricd 的 64 KiB prompt 上限检查，避免把已知拒绝误记成结果未知。ACP 的更新事件只将 `agent_message_chunk`/`agent_message` 文本映射为有序正文 delta，思考、工具和用户消息被过滤；`stopReason`/完成状态结束本回合，流式卡片不能仅凭 PTY 静默判断“已完成”。回合上下文取消时主动关闭 ACP 观察订阅，使阻塞中的 `Recv` 退出；这只中断本地观察，不等于取消或确认 Agent 的远端执行结果。`Start`、`Attach`、`Input/Prompt`、`Stop` 分别表示启动、重连观察/控制、提交新回合和显式销毁；断开连接不等于 Stop。Dune 的 `runtime.stop` 响应先确认停止请求，ACP 子进程的退出状态随后发布；`duneagent.Stop` 因此在单次停止请求后，最多等待 15 秒通过只读 Get 确认精确 Runtime 已退出或明确失效，才返回成功。查询失败或超时保持结果未知，不重复发出 Stop。当前适配器对仍存活且身份匹配的 Runtime 执行 Attach；仅在旧 Runtime 明确返回 `STALE_RUNTIME` 或已退出时新启 Runtime，若 Agent 广告 `loadSession` 则加载原 ACP session；否则新建 session，并在同一回复提示上下文未恢复。网络错误等不确定查询不触发自动重建。

后台入口由接入方以已授权的 Tenant/Runner 作用域调用；不要求飞书发送者拥有 Dune Web 身份，也不伪造 `identity.User`。宿主提供的是绑定所属的受信任 actor，绝非事件发送者身份；Dune host 根据 RunnerID 读取当前 Runner Binding，再检查 Runner 归属、当前 binding/incarnation/generation 和操作权限。当前 `host.AgentExecutor` 已按此路径实现并通过本地假 ACP 子进程穿过 Gateway 的测试；接入方提供实际受信任 actor 的装配不属于本次交付。

IM 的 `TenantID` 与 Dune 的 `OwnerID` 是不同边界的标识，不要求字符串相同。接入方实现的受信任 `ScopeResolver` 必须根据持久 BotBinding/会话目标核验 Tenant 对 Runner 的授权，并返回映射后的 Dune `OwnerID`、同一个 `RunnerID` 与受信任 actor；`duneagent` 拒绝空 Owner 或不匹配的 Runner，Dune host 继续校验 Runner 实际归属及 actor 权限。飞书消息发送者不能提供或改写这些值。

会话记录保存 session key、目标快照、当前 Runtime 身份、ACP session ID、回复地址版本及处理状态。进程仍存活时优先 Attach；进程已失效时，只有 Agent 声明并成功执行 ACP `session/load` 才能恢复原对话。否则明确建立新 session 并告知用户上下文未恢复，不假定 Dune 内存状态可跨重启恢复。Agent Profile 共享不等于文件隔离；若 SandDance 需要用户间文件隔离，应给不同用户分配独立工作目录或 Runner。

## 可靠性与状态归属

Dune IM module 定义 `BindingStore`、`CredentialResolver`、`ConversationStore` 和 `InboxStore` 等最小接口，并提供除凭据解析外的单宿主 SQLite 持久化实现；接入方提供凭据解析，可复用 SQLite 或自行实现存储。集群接入时应使用共享数据库和跨实例 session 租约。至少持久化：

- BotBinding 及配置版本；
- ConversationSession、Runtime/ACP 标识和可空的 Provider 话题引用；
- InboundEvent 的唯一 `(BindingID, EventID)` 或消息 ID、处理状态与错误；
- 三种模式共用的 `im_deliveries` 投递状态；飞书 CardKit 细节存于带版本的 `provider_state`，防止重复建卡、重复回复或遗留未关闭卡片。

入站流程为：验真/解密 → 去重并可靠入队 → 快速 ACK → 按 session 串行领取 → Agent prompt → 按 ReplyMode 更新流式卡片或发送最终回复 → 记录结果。公共 `channel.Ingress` 实现唯一的 `EventSink.Accept`，只负责调用宿主的持久 `InboxStore.Insert`，不在飞书长连接/回调处理函数内运行 Agent。飞书 `Service.LoadTenant` 同步该 Tenant 的多个机器人：同一 revision 且健康的 Channel 保持运行，不因重复同步而重连；停用先于其他机器人的激活执行，某机器人激活失败不阻止其余机器人同步，激活期间若持久 Binding 再次换版，不得装上过期接收器；`Activate` 的凭据解析、配置校验或 Provider 打开失败也必须停用旧接收器，且不得误停并发装上的新版本。`Service.Run` 启动有界数量的工作者，同一个 Service 不得重复启动工作循环，外部 `Stop` 会取消它们；回调路由和长连接事件入站都在写入前核对当前持久 Binding 的启用状态与 revision。`BindingGuardedInbox.InsertForBinding` 把该核对与幂等入队放在同一个存储事务中，旧 revision 即使重投已存在的 EventID 也不能获得成功 ACK；当前 revision 可幂等确认跨轮换重投，但保留事件原始 `BindingRevision`，worker 不得用新 Agent 目标执行旧事件。凭据轮换或停用后不得继续由旧 Channel 接受新事件；回调请求读取不持有 Service 注册表锁，慢请求不得阻塞停用，停用后才读完的旧请求不得成功 ACK；成功的 `Channel.Stop` 等待已进入的入站处理完成，超时则明确返回未排空错误；Service 保留不可路由的待清理条目，后续 `Deactivate`、`Stop` 或换版可重试确认排空，不能因首次超时丢失清理状态。对同一机器人、同一群，尚在 `claimed` 入场判定阶段的早先消息会暂时挡住后续消息领取，避免后续未再次 `@` 的话题消息抢先被误忽略；首条消息进入 `submitting` 后，各话题仍可独立处理。事件尚未提交到 Agent 前可按状态重试；领取租约在提交屏障前过期时，必须撤销临时会话 `running` 状态，再按领取次数上限重排或标记失败，不得标成 Agent 结果未知；`WorkQueue.BeginSubmission` 必须在调用 Agent 前落库，之后若连接中断且执行结果未知，标为 `unknown` 并核查状态，**不自动重放 prompt**。耗时 Agent 与外部投递期间定期续约会话租约；续约失败则取消当前操作并隔离为结果未知，已经发出的外部请求仍需核查，不能假定取消成功。飞书发送结果未知时也不盲目再发。队列容量、超时和重试次数均需有界，并暴露 Binding 连接、积压、失败和结果未知状态。

对于已领取但尚未提交 Agent 的事件，`BeginSubmission` 还须在同一存储事务内复核当前 Binding revision 与启用状态；如果此时换版，撤销临时的会话 `running` 状态、忽略旧事件，不调用 Agent。若提交屏障已成功落库，此后发生换版则属于已开始的旧回合，仍按目标快照和结果未知规则处理。平台已明确返回成功、但请求上下文在本地确认写入前取消时，可用短时独立上下文重试**仅本地状态确认**，不得再次调用飞书发送或 CardKit 更新 API；若本地确认仍不可判定，保持投递结果待核查。

路由得到规范化 `SessionKey` 后，`WorkQueue.PrepareRoute` 将其哈希持久绑定到 Inbox 事件，并检查同会话中更早的未完成事件；后续事件必须等待前序回合，不能越过暂时延后的消息。当会话租约被另一 worker 占用，或更早的同会话事件尚未完成时，`WorkQueue.DeferClaim` 将当前事件短暂延后并退还这次领取尝试，不把正常等待计入失败重试上限；SQLite 使用 `im_inbox.session_hash` 和 `available_at` 保持同会话入队顺序。前序结果未知时后续消息继续等待；若其投递已确认且经 `ReconcileConfirmedDelivery` 完成，后续消息才能 Attach 原会话继续处理。真正的预提交处理错误仍走有界 `ReleaseClaim`。同群不同话题的 session key 不同，首条消息进入 `submitting` 后仍可并行推进。

独立 module 现在提供 `im/sqlite` 作为单宿主持久化基础，最终仅有 `im_bindings`、`im_inbox`、`im_conversations`、`im_deliveries` 四张表：Tenant 作用域内多 BotBinding 的版本化存储、Inbox 事件去重/领取/提交边界、会话及其 Agent 目标快照/Runtime 标识、跨 worker 的会话租约、可空话题引用和通用投递状态 CAS。闲置租约过期可以接管；`running` 或 `unknown` 回合不会自动接管。公共 `channel.Processor` 已通过假 Agent/Channel 测试验证私聊隔离、群话题续聊、ACP 增量驱动卡片、未知 prompt 不重放，以及 Binding 换版后已启动的会话继续 Attach 原 Runtime/AgentTarget、不偷换 Profile 修订；飞书新话题入场通过 bot/v3/info 查询当前机器人 Open ID 后匹配结构化 @。飞书 Service 的本地假 API 整链测试覆盖签名回调、持久入队、Run worker、两个私聊用户隔离与同用户 Attach 续聊，也覆盖群顶层 `@` 入场、另一成员同 thread 续聊、新话题隔离和各轮 `reply_in_thread` 回复；回调验收矩阵现已连接完整的回调入队 → Processor → duneagent → 受信任 host → Gateway → fabricd → 本地假 ACP 子进程 → 假飞书 API，覆盖 3 种回复模式 × 私聊/群话题，核实 assistant 正文与思考过滤、每轮投递持久完成、流式卡片按原话题创建/更新/关闭且仅发一条消息。这些测试仍不能替代飞书平台验收。`im/duneagent` 已接到受信任 host 入口并通过模拟 ACP 协议事件测试覆盖 Attach 与失效 Runtime 恢复；host 入口另以本地假 ACP 子进程验证 Gateway 上的 new/prompt/load。另以实际 ACP 子进程验证两个私聊会话的独立 Runtime、Attach 续聊、Stop 退出确认及新进程通过 `session/load` 保持原会话历史。已实现“投递持久完成而收尾未知”这一确定结果的核查恢复；其他无法证明结果的回合继续隔离而不自动重放。真实 Agent 行为和真实飞书平台仍未验收。后续接入方可复用 SQLite 或提供满足相同接口的共享存储。

## 交付顺序与验收

同 Tenant 双机器人回调整链测试使用不同 App ID、App Secret、Encrypt Key、Verification Token、Agent Profile 和最终回复模式；即使两条消息来自同一用户并共享 EventID，也分别形成独立会话、投递记录和正确的文本/卡片 API 调用。该测试只覆盖本地假飞书 API，不代替真实多应用授权验收。

本地 WebSocket 假网关测试会经过底层 SDK 的 bootstrap、连接、事件帧分发和响应帧：持久入队成功返回 200、队列满返回 500、同一事件重投仍可幂等 ACK、同 ID 不同载荷的冲突事件返回 500；另有 WebSocket 群顶层 `@` 消息 → Inbox → 话题会话 → 假 Agent → 假飞书 API 的整链测试，分别覆盖三种回复模式，确认回复仍设置 `reply_in_thread=true`、流式卡片原生创建/更新/关闭且投递持久完成。同 Tenant 混合 WebSocket 与回调机器人的测试验证两种入口可同时激活，并在停用、重新激活和凭据轮换长连接机器人时保持回调机器人可用；轮换后旧 Channel 已停止，新 Channel 使用新凭据。停用长连接时先让 SDK 正常关闭，再取消其上下文，避免把正常停用误报为 `context canceled`；整体 Service.Stop 也覆盖这一路径。另有 SQLite 子进程退出后由新进程打开同一数据库的测试，验证进入 `submitting` 的回合不会重放；已确认投递而 Inbox 仍为 `submitting` 的收尾恢复由重开数据库的测试覆盖。CardKit 未知更新也通过 SQLite 关闭重开测试确认保持隔离，不自动新建卡片或重发消息。这些测试只证明本地持久化和协议边界，不替代飞书平台上的长连接、重连及话题验证。

2026-09-17 本次交付核查：

| 检查 | 结果与证据范围 |
| --- | --- |
| `make test-im-race check-im` | 通过；IM 全量竞态回归和 vet，含完整 Gateway/ACP 回调矩阵、生命周期、WebSocket 假网关、SQLite 跨进程恢复 |
| `make test` 的主 module 检查：`go test ./... -count=1 -timeout=180s` | 通过；主仓库全量 Go 回归，外部环境用例按配置显式跳过 |
| `make check-go` | 通过；主 module 与 IM module 的 vet |
| `make web-check web-build` | 通过；构建提示现有主包体积超建议阈值 |
| 独立临时 module 编译 IM README 的装配代码 | 通过；消费者显式替换主/子两个 module，无需导入 Dune internal 包；临时目录已清理 |
| `go test ./tests -run '^TestRealAgentACP$' -v -count=1 -timeout=30s` | 明确跳过：未配置 `DUNE_REAL_AGENT=1`；该测试即使启用也只证明 ACP 初始化握手 |
| 真实飞书 WebSocket/公网回调/CardKit | 未执行：无已配置的飞书平台验收凭据与目标群聊 |

常规验证入口已覆盖独立子模块：默认 `make test`、`make test-race` 会继续执行 IM 的对应检查，`make check-go` 检查两个 module；指定主 module 的 `TEST_PKGS` 时保留定向范围，IM 定向验证使用 `make test-im IM_TEST_PKGS=./feishu`。这些通过项不构成真实模型调用、真实飞书平台或已发布 module 的完成证据。

1. 定义独立 Go module、Provider/Store/AgentBackend 接口和标准事件；用假 Provider/Backend 验证 session key、跨 Binding 隔离及会话串行。
2. 增加受信任后台 Agent 执行入口；用 mock ACP 验证每个 conversation 独立 Runtime、Attach、`session/load` 支持判断、目标变更与未知结果处理。
3. 基于底层飞书 SDK 实现 WebSocket、三种回复模式和群 `reply_in_thread`；验证两人私聊互不串上下文、同群不同话题不被按 chat 合并、同话题多人续聊、首轮回复后不跳 session，以及 ACP 流式卡片只更新原 CardKit 实体、关闭流式模式、不重复发送最终答案。真实飞书环境必须验证 CardKit 卡片实体在 `reply_in_thread` 下的发送和更新。
4. 实现公网回调的校验/解密、可靠 ACK 和与 WebSocket 共用的处理链；验证重复事件、回调重投、重启和凭据轮换。

SandDance 的 Tenant/BotBinding 管理、secret/store、HTTP 路由和运营状态装配均不在本次范围内，也不是 Dune IM module 的完成条件。本次仍须验证飞书 Provider 的 WebSocket 和公网回调行为；有真实飞书环境时再做平台端到端验收。

三种回复模式都要分别覆盖私聊和群话题；`streaming_card` 配 PTY 必须在启动前报错。不可把 mock ACP 或本地假 ACP 测试称为真实 Agent 验收；跨进程重启、飞书回调重试、真实话题和真实卡片更新必须单独验证。本文定义的 Dune 模块、通用接口、飞书双入口、Tenant 多机器人、会话隔离、ACP 生命周期、三种回复模式及持久化可靠性已实现，并完成当前环境的自动化和本地端到端验收。真实 Agent 与飞书平台验收、正式版本发布保留为明确的外部交付条件；不得把本地假平台结果表述为这些条件已满足。
