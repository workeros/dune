# Agent MCP 运行凭据

`internal/metadata/agent_credentials.go` 为可信宿主提供签发、读取和撤销。`dune_agent_credentials` 直接保存凭据哈希、Tenant / Owner、调用方 namespace / ID / subject / kind、完整 Runner binding、Runtime ID / incarnation / generation / adapter、有效期。它不依赖会话恢复表，不保存命令、环境、消息或输出。

只在确认 Runtime 后签发。签发验证现有 Runner 归属、binding 和启用状态；同一 Owner / target 原子轮换 token。数据库只存哈希，提交结果未知不返回秘密值。有效期最长 30 天，签发时清理过期记录。

每个 MCP HTTP 请求先查凭据、命名空间和账号状态，再经 SDK / Gateway 核验调用方 Runtime 仍存在且运行、Runner binding 未变化且访问有效。数据库读取成功本身不证明进程存活。撤销、过期、禁用账号或 Runtime 退出均使调用被拒绝；重新启用本地账号不会复活旧 token。原生会话切换复用同一进程的凭据。

凭据跨宿主 Pod 共用数据库，宿主重启不影响存活 Runtime 的凭据。新建 Runtime 使用独立凭据；没有进程恢复、自动重签或自动重启。企业身份不复制为本地用户，仍由现有 AccessChecker 判断 Tenant 访问。

SQLite / PostgreSQL 回归覆盖哈希、跨连接读取、并发轮换、过期、撤销、账号状态、损坏或不匹配目标、跨 Tenant 隔离和数据库重开。HTTP / Gateway 验证另覆盖每次请求的实例与权限校验。
