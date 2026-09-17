# Dune IM

可选 Go module `github.com/aiomni/dune/im`，供宿主接入 IM 机器人。
Dune 主 module 不依赖本 module；不开启 IM 时，不会创建飞书连接或回调路由。
本 module 已实现飞书 WebSocket、公网回调、Tenant 多机器人、独立 ACP 会话及三种回复模式。
真实飞书平台行为仍需使用接入方的应用和群聊完成验收，见下文。

| 包 | 职责 |
| --- | --- |
| `channel` | Provider/Channel、规范化消息、会话路由、AgentBackend、持久队列、租约和投递接口 |
| `feishu` | 飞书验真与解密、WebSocket、回调 handler、话题定位、普通回复、原生 CardKit 和多 Binding Service |
| `duneagent` | 经受信任 `host.AgentExecutor` 和 Gateway 操作托管 ACP Runtime |
| `sqlite` | 单宿主持久化：Binding、Inbox、Conversation、Delivery 四张表 |

## 引入与装配

当前是同仓库开发版本，主 module 依赖仍使用 `v0.0.0` 和本地 `replace`。
外部项目本地开发时，必须同时替换两个 module；依赖中的 `replace` 不会传递：

```sh
go mod edit -require=github.com/aiomni/dune@v0.0.0
go mod edit -require=github.com/aiomni/dune/im@v0.0.0
go mod edit -replace=github.com/aiomni/dune=/absolute/path/to/dune
go mod edit -replace=github.com/aiomni/dune/im=/absolute/path/to/dune/im
go mod tidy
```

正式发布需先发布含 `host.AgentExecutor` 的 Dune 主 module，再更新 IM 对它的实际版本依赖，
最后发布 `im/` 子模块标签。当前没有对已发布版本或外部 `go get` 可用性的承诺。

宿主提供已有的 `*host.App`、`feishu.CredentialResolver` 和 `duneagent.ScopeResolver`。
装配顺序如下；错误均由宿主处理，`Run` 是阻塞工作循环：

```go
store, err := sqlite.Open(ctx, "/absolute/private-directory/im.sqlite")
if err != nil {
    return err
}
defer store.Close()

agents := duneagent.Backend{
    Executor: app.AgentExecutor(),
    Scopes:   scopes,
}
service, err := feishu.NewService(store, credentials, agents)
if err != nil {
    return err
}
defer func() {
    stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if err := service.Stop(stopCtx); err != nil {
        onError(err)
    }
}()

// BotBinding 已由宿主通过 store.Put 持久化；同一 Service 装载所有 Tenant。
for _, tenantID := range tenantIDs {
    if err := service.LoadTenant(ctx, tenantID); err != nil {
        return err
    }
}

// 每个 callback Binding 挂独立路径；WebSocket Binding 不需要 HTTP 路由。
handler, err := service.CallbackHandler(callbackTenantID, callbackBindingID)
if err != nil {
    return err
}
mux.Handle("/im/feishu/bot-a", handler)

return service.Run(ctx, 4, onError)
```

示例中的包分别为 `context`、`time`、`github.com/aiomni/dune/im/{sqlite,duneagent,feishu}`；
HTTP 服务、Tenant 发现、路由挂载和错误记录由宿主拥有。一个 Service 对共享 Inbox 运行一组
worker；不要为同一队列建立只认识部分 Binding 的多个 Service。集群宿主应实现相同原子语义的共享存储。

`CredentialResolver.ResolveCredentials` 根据持久 Binding 的 `CredentialRef` 返回飞书凭据 JSON。
`ScopeResolver.ResolveAgentScope` 根据会话的 Tenant 和目标快照核验 Runner 授权，返回 Dune Owner、
同一 Runner 和受信任 actor；IM Tenant 与 Dune Owner 可以使用不同 ID 空间。
消息发送者不能决定 actor、Runner 或工作目录。Dune host 还会校验实际 Runner 归属和操作权限。

## 机器人配置

`store.Put` 使用 `BotBinding.Revision` 做 CAS：创建为 0，更新时传最近读到的 revision，
使用返回值继续操作。Binding ID 全局唯一，不能转移到另一个 Tenant。

```json
{
  "app_id": "cli_example",
  "site": "feishu",
  "receive_mode": "callback",
  "reply_mode": "streaming_card"
}
```

上述 JSON 放入 `BotBinding.Config`，`ConfigVersion=1`；`Provider="feishu"`。
另设 `TenantID`、`CredentialRef`、`Enabled` 和 `Target`：Runner ID、该 Runner 上保存的 ACP
AgentConfig ID、绝对工作目录。凭据 JSON 包含 `app_secret`、`encrypt_key`、`verification_token`，
通过 resolver 提供，不存入展示用 Config。

- `site` 为 `feishu`（默认）或 `lark`。
- `receive_mode` 为 `websocket` 或 `callback`；同一 Binding 只能选一种。
- `reply_mode` 为 `final_text`（默认）、`final_card` 或 `streaming_card`。
- 回调必须配置 Encrypt Key 和 Verification Token；两种入口都必须配置 App Secret。
- 首版三种模式均使用 ACP，流式模式另外要求有序 assistant 正文增量。PTY 在激活前被拒绝。
- 飞书应用须订阅 `im.message.receive_v1`，按使用的消息、话题查询和 CardKit API 配齐权限。

更新或轮换凭据后，持久化新 Binding revision 并调用 `LoadTenant`；停用也先持久化 `Enabled=false`。
健康且版本未变的连接会保留。更新 Agent 目标只影响尚未启动 Runtime 的会话；已有会话继续使用目标快照。

私聊按 Tenant、Binding、Chat、发送者隔离；群聊按 Tenant、Binding、Chat、规范化根消息隔离。
新群话题需结构化 `@` 当前机器人；已参与的话题内其他成员可以直接续聊。
群回复始终使用 `reply_in_thread=true`，失败时不会改发群顶层。

## 停用与结果核查

`Service.Stop` 停止入站和 worker，不销毁各会话的 Agent Runtime。重新建立 Service 后可 Attach
仍存活的会话。显式 `duneagent.Backend.Stop` 发出一次停止请求并等待 Runtime 退出确认；
查询失败或超时返回结果未知，不自动重放停止请求。旧 Runtime 明确失效后，Attach 才会新启进程，
按 ACP 的 `loadSession` 能力恢复；不支持加载时新建会话并在回复中提示上下文未恢复。

`Status` 查看连接状态和队列计数，`Issues` 按 Tenant/Binding 返回有界、脱敏的失败和未知结果。
回调 `callback_ready` 只证明本地 handler 可用。

事件先持久入队再 ACK，Agent 提交和外部投递前均有持久屏障。提交后断线不自动重放 prompt，
发送或 CardKit 更新未知也不自动重发。`ReconcileConfirmedDelivery` 仅恢复已持久确认 Agent
完成且完整送达的回合；`ReconcileRejectedDelivery` 仅恢复明确未调用平台 API 的本地拒绝。
其他未知结果继续隔离，需宿主核查，不能把重启当作重试。

最终文本/卡片保持单条发送，超出平台请求体限制时明确失败；流式长答案按 UTF-8 和序列化请求预算
拆为同一目标中的续卡。每卡关闭流式模式，完成时不会另发重复答案。

## 验证

在仓库根目录执行：

```sh
make test-im-race
make check-im
make test            # 默认同时包含主 module 和 IM module
make check-go        # 同时检查两个 module
```

本地自动化覆盖双入口持久 ACK/重投、同 Tenant 多机器人、会话隔离与串行、换版、队列和租约，
以及已提交回合的跨进程恢复边界。回调的三种模式 × 私聊/群话题矩阵穿过真实 Dune host、Gateway、
fabricd 和本地假 ACP 子进程，再调用假飞书 API；另测 Attach、Stop 和 `session/load`。
WebSocket 测试使用飞书 SDK 连接本地假网关，覆盖三种模式的群话题整链。
这些测试没有模型调用，也不能证明飞书平台已经验收。

真实平台验收应记录应用、入口、模式、EventID/MessageID、Runtime/ACP session 和 Delivery 状态，
不记录凭据；至少覆盖：

1. 同 Tenant 两个应用分别通过 WebSocket 和 HTTPS 回调接收；轮换/停用一个不影响另一个。
2. 三种回复模式分别验证两个私聊用户、同群两个话题、同话题多人续聊及回调重投。
3. 群话题 CardKit 原生建卡、引用实体回复、同卡累计更新、最终关闭及长答案续卡。
4. 回合中断/宿主重启后核查 Inbox、会话和 Delivery；确认没有重复 prompt、建卡或发送。
5. 配置真实 ACP Agent，验证实际 prompt、增量、最终结果及支持时的 `session/load`；
   主仓库 `TestRealAgentACP` 只验证初始化握手，不替代这项验收。

详细状态契约与设计依据见[集成方案](../docs/im-integration-plan.md)。
