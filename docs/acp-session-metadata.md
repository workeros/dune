# ACP 自动标题与目录订阅

实现依据：SandDance `docs/dune-acp-title-requirements.md`（2026-09-23 复审稿）。
本文件记录 Tracer Bullet 切片及最终公开合同。未勾选项尚未交付。

## 实现切片

- [x] ACP 常驻宿主唯一归并点、统一元数据、发现和会话读取；实际子进程贯通验证。
- [ ] Runner 范围完整快照通知、合并与有界背压；保留宿主跨 connector 重连。
- [ ] Tenant / Runner 目录订阅与 SDK：就绪、发现衔接、成员资格、撤权、重同步。
- [ ] Dune Web 消费标准元数据和目录订阅。
- [ ] 屏障、乱序、生命周期、权限与整链验收；文档、构建产物与交付。

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

## 验证记录

首个切片：

- `go test ./pkg/fabricd ./pkg/api ./tests -run '^(TestSessionMetadata|TestConversationNotification)' -count=1 -timeout=120s`：通过。
- `go test -race ./pkg/fabricd ./pkg/host -run '^(TestSessionMetadata|TestConversation|TestAgentDirectory)' -count=1 -timeout=180s`：通过。

覆盖 v1/v2 归并、缺失/null/非法/超限/重复、跨代次与大修订、正文淘汰、并发原子快照，
以及 SDK/Gateway/独立 ACP 宿主和 AgentDirectory 的实际子进程发现。RPC 日志验证
读取与浏览器重连没有调用原生控制方法。目录范围通知及 connector 重启验收尚未完成。
mock 进程不计为真实厂商 Agent 验收；本次环境未启用 `DUNE_REAL_AGENT`。
