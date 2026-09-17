# Agent MCP 会话凭据

`internal/metadata/agent_credentials.go` 提供宿主内部的签发、校验与撤销。凭据绑定发起启动的身份命名空间、主体、Tenant / Owner、恢复记录和启动 attempt；目标 Runtime 从该 attempt 的已确认回执取得，不接受模型提供的 Tenant 或调用方身份。

签发可以发生在进程启动前，便于 PTY 注入；在 Runtime 回执保存前凭据不能使用。同一记录重新签发会原子轮换旧凭据，保存的只有哈希；数据库提交结果未知时不会返回秘密值。每个恢复记录最多保留一个当前凭据，过期记录在签发时清理。默认最长有效期为 30 天，Runtime 退出、原绑定失效或显式撤销会提前终止使用；到期后需要重新签发并注入，不静默延长期限。

校验每次读取数据库，不缓存宿主 Pod 归属。恢复产生新 attempt 后，旧凭据立即失效；宿主 Pod 重启不使活跃凭据丢失。本地账号禁用会撤销其全部 Agent 凭据，重新启用也不恢复旧 token。企业身份原样保存 namespace / kind / subject，并由宿主现有 AccessChecker 继续验证 Tenant 访问；此存储层不新增企业用户表，也不假定能观察上游退出登录。

存储校验本身不证明 Runtime 存活或 Runner 可达。MCP HTTP 接入必须在每次调用时检查身份命名空间，通过现有授权及 SDK / Gateway 核验调用 Agent 的完整 Runtime 身份和运行状态；目标 Agent 的读写另走同一 Tenant 作用域的共享服务。原生会话切换仍属于同一个 Runtime，凭据绑定进程启动 attempt；显式恢复使用新凭据。

凭据只用于本次运行配置，不能保存回 Profile、实际启动快照、Agent 摘要或日志。签发和轮换仅向可信宿主编排代码开放，不提供模型自助扩大作用域的工具。

SQLite / PostgreSQL 验证覆盖启动前拒绝、哈希存储、跨连接轮换与撤销、身份命名空间、数据库重开、过期、账号禁用后重启用、恢复 attempt 变化及跨 Tenant 拒绝。HTTP 鉴权、Runtime 核验和实际注入由后续接入实现验证。
