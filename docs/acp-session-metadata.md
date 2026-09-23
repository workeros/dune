# ACP 自动标题与目录订阅

实现依据：SandDance `docs/dune-acp-title-requirements.md`（2026-09-23 复审稿）。
本文件记录 Tracer Bullet 切片及最终公开合同。未勾选项尚未交付。

原型约束：直接更新公开类型、协议与消费者，不提供历史数据迁移、旧字段别名或
兼容层。设计以可读性、职责清晰、简单实现和当前 Go/浏览器 API 为先。

## 实现切片

- [x] ACP 常驻宿主唯一归并点、统一元数据、发现和会话读取；实际子进程贯通验证。
- [x] Runner 范围完整快照通知、合并与有界背压；保留宿主跨 connector 重连。
- [x] Tenant / Runner 目录订阅与 SDK：就绪、发现衔接、成员资格、撤权、重同步。
- [x] Dune Web 消费标准元数据和目录订阅。
- [x] 屏障、乱序、生命周期、权限与整链验收；文档与构建流程。

## 元数据合同

`Runtime.session_metadata` 是 managed ACP 的完整快照；PTY/raw ACP 不提供此对象。
`acp.state.session_metadata`、conversation 描述和通知复用 `api.SessionMetadata`。
`revision` 是十进制 JSON 字符串，按整数比较，在同一准确 Runtime 内跨 conversation
递增；`conversation_id` 和 `title` 为字符串或显式 null。无模型时两者均为 null。
启动默认名称仍是 `Runtime.title`。

ACP v1/v2 的 `session_info_update` 由宿主在接收时处理。缺失 title 保留旧值，null
和 trim 后为空的字符串清空。输入按解码后的 UTF-8 字节计数，trim 前最多 1024
bytes；非法类型、无效 UTF-8、超限、trim 后仍含 Unicode 控制字符或 U+2028/U+2029
的标题被忽略，并计入 `invalid_session_title` 诊断。空白裁剪使用 Unicode 空白规则。
拒绝先于清空判断，因此超限的空白串不会清空旧标题。

协议字段核对 [ACP b9d6aca 的 v1 schema](https://github.com/agentclientprotocol/agent-client-protocol/blob/b9d6aca6757d0f5b6e435cad54f9f04657aa9802/schema/v1/schema.json)
与同提交的 `schema/v2/schema.json`；两者的 SessionInfoUpdate 都允许 title 为 null。

标题状态独立于正文、原始事件、Controller/Conversation 修订；只在归一化标题改变
或实际建立新 conversation 时推进修订。new/load/resume 都清空旧代次标题。当前
代次已经收到的标题在打开失败/未知后仍作为观察保留；标题不证明打开成功。

元数据不落盘，生命周期跟随常驻 ACP 宿主。Connector 重启不得初始化 Agent 或
重放请求；宿主死亡不承诺恢复。目录成员资格与订阅代次在标题修订比较之前校验。

## Runner 范围通知

`client.SubscribeRuntimes(ctx)`（`runtime.watch`）在确认订阅后返回，`Next` 读取
`api.RuntimeChange`。更新携带完整 Runtime；`removed:true` 携带准确身份，表示成员移除。
首次连接与每次宿主重连主动提供当前观察，随后标题更新通过宿主 IPC 推送，不查询
`acp.state`、`runtime.get` 或原生历史。独立宿主的来源连接由 connector 共享。

同一 Runtime 的待发值可以合并，移除优先于该成员迟到的发布。每条范围流最多缓存
512 个 Runtime；超限以 `RESYNC_REQUIRED` 结束并丢弃缓冲值。网络中断同样使该
订阅失效，调用方先建立新订阅再批量发现。订阅使用独立 `watch` 容量，单连接/Engine
最多 16 条，不占用普通执行、状态读取或控制保留槽。

Connector 内每秒扫描注册表以发现异步注册的宿主，仅用于成员变化；标题不通过
轮询获取。来源暂时不可用时发布保留观察和 `availability:unavailable`，不会伪造
更高修订的空标题；恢复连接后主动推送当前宿主快照。

## 验证记录

首个切片：

- `go test ./pkg/fabricd ./pkg/api ./tests -run '^(TestSessionMetadata|TestConversationNotification)' -count=1 -timeout=120s`：通过。
- `go test -race ./pkg/fabricd ./pkg/host -run '^(TestSessionMetadata|TestConversation|TestAgentDirectory)' -count=1 -timeout=180s`：通过。

覆盖 v1/v2 归并、缺失/null/非法/超限/重复、跨代次与大修订、正文淘汰、并发原子快照，
以及 SDK/Gateway/独立 ACP 宿主和 AgentDirectory 的实际子进程发现。RPC 日志验证
读取与浏览器重连没有调用原生控制方法。目录范围通知及 connector 重启验收见第二个切片。
第二个切片：

- `go test -race ./internal/latest ./internal/wire ./pkg/fabricd ./tests -run '^(TestMailbox|TestRuntimeWatch|TestProtectedStreamsAfterConnectorCrashWithOrdinarySubscriptionsFull|TestRequestClass)' -count=1 -timeout=240s`：通过。

覆盖有界合并、缓冲失效、移除后的迟到发布，以及 SDK → Gateway → connector →
独立宿主通知；订阅后启动的新 Runtime 无需打开面板即可收到标题。强杀并重启
connector 后重订阅恢复最后标题，RPC 日志不增加；普通流占满时控制和恢复仍可用。

mock 进程不计为真实厂商 Agent 验收；本次环境未启用 `DUNE_REAL_AGENT`。

## Tenant 目录与公开 SDK

`app.AgentDirectoryObserver().Subscribe(ctx, scope, agents.DirectoryWatch{RunnerIDs: ids})`
按 Tenant 授权范围建立逻辑订阅；不传 IDs 覆盖当前 Tenant 中获 runtime.list 授权的
Runner，拒绝的 Runner 不占用来源连接。显式选择未获授权的 Runner 则拒绝整个请求。最多 128 个 Runner，
每个订阅最多积压 4096 个成员。每个 `member` 事件确认当前成员资格，并带完整
Agent（Runner binding、Runtime 身份与标准元数据）。标题事件不需要任何补查。

Subscribe 返回前，已有可连接 Runner 的范围流已注册。初始不可连接的 Runner
恢复、新 Runner 加入、Runner 移除/换绑/撤权、Runtime 移除或流中断，均终止整个
目录订阅并报告 `RESYNC_REQUIRED`；服务丢弃全部缓冲值。每秒启动一次 Runner
成员与授权核对；每次发送前再次校验 Runner 授权。已有 TCP 在途字节不能撤回，SDK
收到失效/连接错误后立即作废整批数据。初始离线来源恢复时必须重新订阅，避免把
连接前产生的分页误当成连续观察。标题修订不能延长目录成员资格。

推荐使用拥有发现与失效屏障的高层 SDK：

```go
monitor, err := agents.ObserveDirectory(ctx, app.AgentDirectory(),
    app.AgentDirectoryObserver(), scope, agents.DirectoryWatch{})
if err != nil { return err }
defer monitor.Close()
for {
    page, err := monitor.Next(ctx)
    if err != nil { return err } // 丢弃本轮目录，再订阅并批量发现
    render(page.Items)
}
```

Monitor 在订阅就绪后批量分页发现。事件和分页先校验订阅/发现代次，然后比较
同一准确身份的元数据修订；中间状态可合并。错误在返回前清空内部目录，迟到的
分页即使忽略网络取消也不能恢复条目。调用方收到错误也应清空已渲染的目录。
`DirectoryCache` 提供相同的底层合并能力；`Begin` 返回不可重标记的发现批次，
`Invalidate` 作废批次及旧事件，新订阅必须使用新的服务端订阅 ID。

HTTP `GET /api/v1/agents/events`（Tenant 路由相同后缀）使用 SSE；可重复传入
`runner_id`。首事件是 `ready`，包含 `subscription_id`，之后批量读取 `/agents`。
正常事件为 `member`，心跳为 `heartbeat`，失效为 `invalidated`。任何传输错误
也表示必须清空当前目录并重新订阅发现。浏览器身份每次写入前校验，心跳间隔
5 秒，写入期限 5 秒，慢消费者不会无限占用写入队列。

第三个切片的 race 测试覆盖 SDK 发现窗口、迟到分页屏障、大修订、慢消费者最终值，
以及公开宿主订阅新增 Runtime 和撤权、HTTP Tenant 边界与发送前凭据撤销。


## Dune Web 消费

工作台以一个目录 SSE 覆盖获授权的 Tenant 范围，先收 `ready` 再分页发现。
自动标题用于侧栏、pane 名称、操作的可访问名称和确认对话框；清空后回退启动名称。
正常标题变化不发起列表、状态或模型补查，也不连接会话面板。失效时清空目录及
在途分页，500ms 起按上限 10 秒退避重订阅。范围整体失效会使已有 pane 重新进行
只读连接；正常标题变更不重连 pane。手动刷新仍可在当前有效订阅内批量重新发现。

前端状态测试覆盖十进制大修订、显式清空、旧分页/旧通知屏障；Chromium 覆盖未打开
会话的标题通知与断线后的最终快照、分页与健康 pane 保留、移除后的范围重同步。
Web 生产构建通过，保留现有 bundle 大小警告。

## 验收场景对应证据

以下为当前实现的本地证据；真实厂商 Agent、真实 PostgreSQL 和跨主机部署另行验收。

| 需求场景 | 测试与证据 |
| --- | --- |
| 首次发现前已发标题、标题后发无 title 的更新 | `TestSessionMetadataDiscoveredWithoutAttachment`、`TestAgentDirectoryDiscoversSessionMetadataWithoutACPReads`；原生 RPC 日志不增加 |
| A → B → 清空逐步屏障 | `TestRuntimeWatchIncludesUnopenedAndNewRuntimes`：独立更新通道驱动，通知确认后读取批量 List，完整快照相同 |
| 慢消费者且后续无更新 | `TestMailboxCoalescesAndFailsBeforeBufferedValues`、`TestDirectoryMonitorSlowConsumerGetsFinalSnapshot`；有界缓存保留最终值 |
| null、非法类型、超长、重复更新 | `TestSessionMetadataMergesBothProtocols`；v1/v2 规范化和诊断 |
| new/load/resume，同原生 ID 的新代次 | `TestSessionMetadataOutlivesContentAndConversationRevisions` 与现有 ACP 队列/原生会话测试 |
| resume 回放 × 成功/失败/未知 × 有无标题；建立前拒绝 | `TestACPV2ResumeTitleOutcomes`；实际 controller 队列、原生结果回调及模型元数据 |
| 切换后旧连接和在途标题回调 | `TestACPRejectsCallbacksFromOldConnectionWithSameNativeID`、`TestACPInFlightCallbacksCannotCrossLoadBoundary` |
| 新通知先到、旧分页后到、修订 > 2^53 | Go/浏览器 `DirectoryCache` 测试；同时保留与新标题对应的原生操作引用 |
| 并发原子快照 | `TestSessionMetadataAtomicConcurrentReads` 及相关 race 测试 |
| 正文/原始对象淘汰不丢标题 | `TestSessionMetadataOutlivesContentAndConversationRevisions`、宿主发现测试 |
| 未开面板实时更新，新增 Runtime | `TestRuntimeWatchIncludesUnopenedAndNewRuntimes`、公开 `AgentDirectorySubscription` 测试及 Chromium 标题场景 |
| 首次发现窗口与最终事件 | `TestDirectoryMonitorDiscoveryWindowAndInvalidationBarrier`；先流后发现，延迟页不覆盖最终通知 |
| 断线/溢出后的最终状态 | mailbox 溢出错误、目录 Monitor 失效屏障、Runtime 重订阅和 Chromium 断线批量发现 |
| 浏览器重连、connector 强杀重启 | Runtime watch 进程测试和 Chromium；标题/修订保持，原生 RPC 日志不增加 |
| 退出保留、暂不可用、替换、forget | 标题保留测试、现有 `TestHostLossUsesIndependentEvidenceAndDoesNotReplay`、移除屏障、生命周期浏览器测试 |
| v1/v2、无标题、PTY/raw | 归并矩阵、初始空模型快照，现有 raw ACP/PTY 回归及工作台回退名称 |
| Tenant、Runner 授权、绑定、撤权 | HTTP personal/Tenant 订阅、发送前注销、宿主订阅撤权与现有 `pkg/access` 回归 |
| 先失效再释放旧通知/分页，含重新订阅 | Go/浏览器缓存的移除/替换/撤权屏障；Monitor 的无视取消迟到分页不能恢复条目 |
| 一页多个 ACP Runtime 无逐会话补查 | `TestAgentDirectoryPreservesPartialRuntimeDiscoveryFromOneRunner`：多个完整标题、一个连接、一次 runtime.list，保留独立发现错误 |

验收中还修复两条生命周期边界：关闭 ACP 宿主时不持有成员锁等待会产生最终通知的
进程退出；完成 forget 时同步清除该准确 Runtime 的旧注册诊断，避免已退休身份被
解释为仍可恢复的临时注册失败。这两条均有原有进程/授权回归和专项 race 复验。

## SandDance 后续接入

升级 Dune 依赖和整套 Runner 包后，使用 `AgentDirectory` 与
`AgentDirectoryObserver`/`ObserveDirectory` 获取标题。删除逐会话 `acp.state`
补查、原始 ACP title 解析、v1/v2 标题分支和 Controller/Conversation 双修订归并。
手动标题和离线索引仍由 SandDance 管理，显示顺序为手动标题 → 自动标题 → 启动名称。
旧订阅失效后清空受影响的在线目录；离线保存的观察不能恢复在线成员资格。

本次使用临时 modfile 将 SandDance 的 Dune/IM 依赖指向本地工作树；其
`go test ./cmd/... ./internal/... ./provider/...` 通过。完整 `go test ./...` 受其
`artifacts/acp-host-startup-3f7ac74/release-harness-e2e_test.go` 跨 module 导入
Dune internal 包阻断。没有修改 SandDance 当前代码、go.mod 或 go.sum。

## 交付验证与限制

- 前端：`npm --prefix web test` 18 项通过；`npm --prefix web run e2e` Chromium 30 项通过；`make web-check web-build` 通过。
- 全量 Go：修复启动关闭锁等待、退休身份残留诊断后，`make test` 完整通过，包含主 module 与独立 IM module。
- 静态检查：`make check-go` 覆盖主 module 与 IM module，通过。
- 专项 race：`pkg/agents`、`pkg/host` 的目录测试，`pkg/fabricd` 的归并/代次/旧连接/启动失败测试，以及 `tests/TestRuntimeWatchIncludesUnopenedAndNewRuntimes` 通过。
- 移除边界：`go test -race ./pkg/access ./pkg/fabricd -run '^Test(ConversationAuthorizationIdentityAndRetainedExit|RuntimeWatchRemovalFencesLatePublication)' -count=3 -timeout=90s` 通过；`go test -race ./pkg/access -count=1 -timeout=120s` 通过。
- IM：`make test-im` 覆盖 channel、duneagent、feishu、sqlite，通过。
- 最终 Tenant 授权范围调整：公开宿主订阅 race 测试与目录 Chromium 用例复验通过；无读取权限的 Runner 在拨号前排除。
- 环境未配置 `DUNE_REAL_AGENT` 或 `DUNE_TEST_POSTGRES`，对应外部验收不计为通过。本次没有部署到远端，也没有推送 Git 分支。


## Runner 交付包

在干净提交上运行 `make release`，构建以下完整 Runner 包及相邻 `.sha256` 文件：

- [Linux amd64](../bin/dune-linux-amd64.tar.gz)
- [Linux arm64](../bin/dune-linux-arm64.tar.gz)
- [macOS amd64](../bin/dune-darwin-amd64.tar.gz)
- [macOS arm64](../bin/dune-darwin-arm64.tar.gz)

每包包含匹配提交的 dune、固定版本的 tmux/rg 及许可证；安装时整体替换程序包。
产物不纳入源码提交。实际校验记录保存在本地
`.local/acceptance/acp-title-20260923/manifest.json`，记录源码提交、包 SHA-256、
可执行权限与归档成员检查。跨平台编译和包校验不等于各目标平台的运行/部署验收。
