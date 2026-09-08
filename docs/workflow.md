# Dune 开发与验证流程

本文件维护可执行的开发入口和验证选择。日常协作约定见 [AGENTS.md](../AGENTS.md)，专门的验证任务可调用 [dune-verify](../.agents/skills/dune-verify/SKILL.md)。下列检查按改动选择，不是每次任务都要执行的清单。

## 环境与启动

从项目根目录执行。Go 版本见 `go.mod`；前端使用 Node.js/npm 与 `web/package-lock.json`。本地进程测试面向 macOS/Linux，需要 Git、Python 3、POSIX shell；`tests/TestMain` 会以 `-race` 编译服务，还需可用的 C 编译工具链。具体外部命令以所选测试为准。

```sh
make build             # bin/dune 与校验过的 bin/tmux
make web-deps          # 初次准备或 package.json / lockfile 变化时安装
make tools             # 仅需 protobuf 工具时，安装固定版本到 .tools/
```

CLI 启动、Web 后端配置与部署参数见 [README](../README.md)。前端开发命令为 `npm --prefix web run dev`，监听 `127.0.0.1:5173` 并代理 `/api` 到 `127.0.0.1:7443`；完整 Web 交互需要 `dune web` 后端。

## 按改动选择检查

| 改动 | 检查与完成证据 |
| --- | --- |
| 文档或指令 | 校验引用路径、命令与实际实现，审阅差异；无需仅为文字改动运行 Go/Web 全量测试 |
| 单个 Go 模块 | `gofmt` 修改文件；`make test TEST_PKGS=./internal/wire`（替换为受影响包）；`make check-go` |
| 跨包 Go 行为 | `make test`；`make check-go`，结合相关 e2e 验证网络或进程行为 |
| 并发、订阅或状态生命周期 | `make test-race TEST_PKGS='./pkg/fabricd ./pkg/host ./internal/webapp ./pkg/gateway ./pkg/transport/tunnel ./internal/tmux'`，缩小或调整为实际涉及包；涉及进程边界再选 `tests/` 回归 |
| protobuf/schema | 工具缺失时 `make tools`；`make proto`，审阅生成差异，再执行 wire 和受影响调用方测试 |
| 前端代码或样式 | 依赖已安装时 `make web-check web-build`；交互或视觉变化在浏览器验证相关路径，视觉要求见 [DESIGN.md](../DESIGN.md) |
| 前端依赖 | `make web`，执行锁文件安装、类型检查和生产构建 |
| 发布产物 | `make release` 构建 Web 与 Linux/macOS × amd64/arm64 二进制；目标平台的安装和运行另行验证 |

`make check` 汇总 Go vet 与 buf lint，单独入口为 `make check-go`、`make check-proto`。`make web` 保留完整干净安装流程；`web-check`、`web-build` 不重新安装依赖。

`make test` / `make test-race` 默认包范围 `./...`、参数 `-count=1 -timeout=180s`。`TEST_PKGS` 与 `TEST_FLAGS` 可覆盖；覆盖 flags 时写出所需的完整参数。例如，仅跑与 ACP 离线授权相关的进程测试：

```sh
make test TEST_PKGS=./tests \
  TEST_FLAGS='-run TestManagedACPOfflinePermissions -count=1 -timeout=180s'
```

PTY 重连/服务重启选 `TestTmuxSurvivesFabricdAndGateway`；原生画面、历史和环境隔离选 `./internal/tmux`。先看测试内容是否匹配待验证的行为；不以测试名代替覆盖分析。

`pkg/fabricd/TestReplacementConnectionRejectsOldStreamInput` 建立两条实际反向协议连接，验证新 generation 生效后，旧 PTY、原始 ACP 和端口输入在执行端被拒绝，Runtime 保留且新连接可执行。`TestCancelledConnectionRejectsBufferedMessage` 单独覆盖取消后仍在 Yamux 缓冲中的消息。它们验证当前连接关联，不证明目录 epoch、输入租约或三节点集群已完成。

输入租约回归覆盖 `internal/wire/TestInputLease*` 的延迟确认、原 grant 期限、禁止复活和有界历史，Gateway 的确认后发布及客户端标识覆盖，以及 fabricd 的实际缓冲消息过期和连续续租。`tests/TestInputLeaseSurvivesProcessPause` 分别用 SIGSTOP 暂停独立 fabricd / Gateway 超过十五秒，向旧终端写入后恢复，确认文件未创建、原 PTY 保留、新连接正常输入。该测试约需四十秒，使用本地正式二进制与真实 tmux，不是三节点、数据库 owner 租约、主机时钟修改或真实 Agent 验收。协议版本变化后另检查全部调用方与旧 hello 拒绝；发布时协调全部协议端升级。

在 Make 变量中使用正则结尾 `$` 时写成 `$$`，避免被 Make 当作变量展开；直接运行 `go test` 时不需要这一层转义。确认选中的测试实际执行，`[no tests to run]` 不算行为验证通过。

`tests/` 即使按 `-run` 过滤也会执行 TestMain 的 race 构建；包内 Go 测试的 race 检查仍需 `test-race`。需要明确排除已继承的外部测试开关时：

```sh
DUNE_REAL_AGENT= DUNE_REMOTE_CONFIG= DUNE_TEST_POSTGRES= make test
```

## 外部验收

SQL 后端使用 `internal/metadata` 的同一组业务契约测试 SQLite 和 PostgreSQL。未提供 `DUNE_TEST_POSTGRES` 时只跳过 PostgreSQL 子测试；不能据此声明两种后端均已验收。指定一个专用 PostgreSQL 测试数据库后，可执行：

```sh
go test ./internal/metadata -v -count=1 -timeout=90s
go test -race ./internal/metadata -count=1 -timeout=180s
go test ./tests -run TestPostgresWorkbenchEnrollmentAndTerminal -count=1 -timeout=180s
```

运行前从私有配置设置 `DUNE_TEST_POSTGRES` 为测试连接 URL。测试创建随机 `dune_test_` 或 `dune_workbench_` schema，结束后删除；密码轮换测试还创建并清理随机测试角色，因此测试账号需要创建 schema 和角色的权限，服务器须实际使用密码认证。不要使用业务数据库。SQLite 的私有目录、独占锁、schema 校验、并发与事务失败测试始终在临时目录运行。PostgreSQL 工作台测试与 SQLite 复用同一注册、CLI enrollment、fabricd 接入、真实 PTY 和退出撤销流程。

`TestPostgresBackupRestore` 还需要与服务器版本匹配的 `pg_dump`、`pg_restore`（从 PATH 查找，或通过 `DUNE_TEST_PG_BIN` 指定目录）。该测试只备份、删除并恢复自己新建的随机 schema，再逐字段核对记录和原会话/机器身份。工具缺失时明确跳过，不能计为备份验收。`pkg/migrate` 的 SQLite 备份恢复始终在临时目录运行；操作顺序见[元数据迁移](metadata-migration.md)。

`TestDurableAccessCredentials` 检查短期凭据的 SQLite 重开、PostgreSQL 跨池单次消费、期限、会话/身份源隔离及固定绑定。`TestPostgresAccessIssuerAndGateway` 用两个独立宿主和真实 fabricd/PTY，验证 A 签发后由 B 消费，并从 A 撤销 B 的空闲用户访问；它显式选择目标 Gateway，不证明 S3 的目录或自动跨节点转发。

协议 peer 回归使用 `go test -race ./pkg/gateway -run Peer -count=1 -timeout=60s`，覆盖双端请求授权、独立角色和启动身份、错误归属、禁止第三跳、上下文界限及断线不重拨。用户上下文另跑 `go test -race ./internal/metadata ./internal/authorization -run Peer -count=1 -timeout=90s`，验证 SQLite 重开、PostgreSQL 跨池单次消费、身份/请求/绑定隔离、有界期限与容量、提交回执丢失和迁移回滚。PostgreSQL 子测试仍需专用配置，不把跳过计作通过。

启用专用 PostgreSQL 后运行 `go test -race ./pkg/fabricd -run TestPostgresOwnedReverseConnections -count=1 -timeout=90s`：三个独立协议 Gateway 使用各自的 peer HTTPS 监听器，经双向 TLS、WebSocket 和 Yamux 连接；客户端固定入口，fabricd 在两个 owner 间顺序迁移。验证目录续租、竞争拒绝、旧 peer 失效、显式重接后真实 PTY 保留及恢复代次隔离。用户上下文使用正式 SQL 签发与消费路径，验证入口允许不能覆盖目标拒绝；故意让入口的有效性回答保持过期状态后，另一 SQL 池退出用户，目标的独立检查仍关闭空闲终端。测试 CA 在内存中临时生成；SDK 与反向连接仍是本地协议夹具，不证明完整 host/CLI 部署或三节点进程/网络故障矩阵。

peer 传输本身运行 `go test -race ./pkg/transport/peer -count=1 -timeout=60s`，覆盖错误 CA/主机、独立 Handler 信任检查、明文与转发头、重复身份头、原 owner 协商、证书到期、取消、代理/重定向拒绝及真实 HTTPS CA 轮换。接入配置和独立监听器的职责见 [peer 传输接入](peer-transport.md)。

外部身份回归运行 `go test ./pkg/identity/... ./internal/metadata ./tests -run 'OIDC|ExternalIdentity|ExternalBrowserLogin' -count=1 -timeout=180s`，PostgreSQL 子测试沿用上述配置。签名协议测试使用本地 TLS issuer 和轮换密钥；宿主测试使用可信测试适配器，验证 SQLite 重开、PostgreSQL 跨实例回调及 Cookie 绑定。这些测试不替代真实身份源验收。

真实浏览器验收可使用独立 [Dex 2.45.1](https://github.com/dexidp/dex/releases/tag/v2.45.1) 和专用账号，按 [Dex 配置](https://dexidp.io/docs/configuration/) 注册精确 Dune 回调地址，将 client secret 保存在 0600 配置文件中。先验证官方 `web --identity-config` 的登录、退出和部署前缀，再用两个共享 PostgreSQL 的独立 `host.App` 分别接收 start/callback，确认跨实例完成；停用 Dune 用户后验证已有 Cookie 和再次上游登录均被拒绝，启用后仅新登录成功。使用 loopback HTTP 的演练不代表 HTTPS 代理、真实企业 IdP 或 S3 执行路由验收；完成后停止专用身份服务、工作台并清理测试 schema。

人类 CLI 回归使用 `go test ./tests -run TestHumanCLILoginAndExecution -count=1 -timeout=180s`，覆盖正式二进制的浏览器确认、私有文件、实际执行和浏览器退出后 API/现有连接撤销；启用 PostgreSQL 时还验证独立签发与消费宿主。`internal/metadata` 的 `TestCLILoginTransactions` 检查单次消费、确认身份不可替换、父会话与版本、并发和持久化；`pkg/login` 检查 HTTP 模糊失败及重定向不重放。真实 Dex 演练另运行 `dune login --site ... --no-browser`，完成浏览器核对与确认、临时 fabricd 上的实际命令，再退出浏览器验证 CLI 失效。手动启动临时 fabricd 时指定构建产物 `DUNE_TMUX=/absolute/repo/bin/tmux`，结束后清理测试进程和凭据。

`TestRunnerBindingSnapshot` 在 SQLite/PostgreSQL 验证逻辑 Runner 与机器 ID 分离、owner 隔离、绑定修订变化后旧访问失效及重开恢复；替换环境在测试中直接构造数据库事实，不代表 Managed 生命周期已经实现。`TestHumanCLILoginAndExecution` 另通过正式 CLI 的 `runners`、`--runner` 和 `DialRunner` 执行真实命令。`TestRunnerDoesNotFollowReplacement` 验证过期快照不会触发 SDK 自动重新解析或重放。

执行流授权运行 `go test -race ./pkg/access -count=1 -timeout=60s`。它使用独立 Gateway 和真实 fabricd/PTY，覆盖文件与上传提交、Git 暂存、端口输入、只读订阅、短期决定复用和空闲撤销；不调用真实 Agent。完整的语义映射与尚未装配的企业入口见[执行流访问检查](access-checks.md)。

以下操作会使用已配置的 Agent 账号或指定远端，仅在任务包含对应验收且已有授权时运行。沿用明确指定的账号、模型和机器；历史文档里的地址与登录状态不是当前授权或可用性证据。环境缺失时报告具体缺口，继续本地检查。

```sh
# 本机真实 PTY 编码任务；默认使用已配置的 codex
DUNE_REAL_AGENT=1 go test ./tests -run '^TestRealAgentPTY$' -v -count=1 -timeout=180s

# 已配置的 Gemini ACP，仅验证 initialize
DUNE_REAL_AGENT=1 go test ./tests -run '^TestRealAgentACP$' -v -count=1 -timeout=180s

# 指定已有客户端配置；该测试会在远端 /tmp 创建并清理测试资源
DUNE_REMOTE_CONFIG=/absolute/client.yaml \
  go test ./tests -run '^TestDirectRemote$' -v -count=1 -timeout=180s
```

PTY 测试支持 `DUNE_PTY_AGENT=claude`，Dune 不代办登录或修改模型配置。上述测试超时还受测试内部 context 限制；增加 Go 的 timeout 不会延长内部期限。

`TestRealAgentPTY` 经 Dune 创建代码并独立运行检查；`TestRealAgentACP` 只测试握手。完整 Web 编码验收还需要从页面提交任务、看到实际结果并独立验证产物。mock ACP 可验证协议与权限状态机，不能替代真实模型任务。

发布构建不自动部署。对指定环境的操作应使用独立配置和测试目录，保留已有会话；连接服务重启只替换 fabricd，终止 tmux 会话需显式执行 `runtime stop`。

## 完成与续接

审阅本次差异并报告结果、实际验证和剩余限制。跨阶段任务可在现有进展文档中记录完成项、关键决定和下一步；小改动无需新建计划或验收档案。只有影响实现语义或验收状态时更新对应文档，避免多份清单重复维护。

当前目录若没有 Git 元数据，用修改清单和文件快照审阅；无需为执行开发流程创建 Git 仓库。将来接入 CI 时复用 Makefile 入口，外部账号/远端验收保持显式启用。

`TestEnterpriseSharedExecutionAndRevocation` 验证所选企业检查器允许跨 owner 的真实 Web 终端及 CLI 执行，同时拒绝文件写入、关闭策略撤销后的空闲流，保留父会话和身份停用撤销。PostgreSQL 子例使用两个独立宿主签发/消费凭据，并不代表 S3 自动路由。`TestAuthorizedDiscoveryAndWrites` 验证 128 候选扫描上限、分页游标的用户/身份源/用途隔离、SQLite 重开和跨 PostgreSQL 池恢复、绑定及会话写入竞争。浏览器分页改动还需在实际工作台验证空页继续、前后翻页、过期恢复及策略故障反馈。

`TestBrowserRunnerBindingIsFixed` 逐一检查浏览器 Runner 的 call、sessions、events 和解绑入口，覆盖绑定缺失/歧义、错误 Runner/Fabric/修订、会话缺失及修订变化后不拨号替代目标。`TestPrefixedWorkbenchEnrollmentAndTerminal/runner-entry` 和企业共享回归通过同一 Runner 快照路径使用真实 fabricd/PTY；浏览器需额外检查环境选择、绑定变化提示、明确重新进入后保留原会话及历史，不自动重连或重复创建。直接修改测试数据库的绑定修订只模拟已提交事实，不代表 Managed 生命周期验收。

`TestRuntimeSelection` 检查事件订阅的完整执行身份及重复、缺失、溢出参数；前缀工作台与企业共享进程回归在实际 PTY 上拒绝错误 incarnation/generation，并验证随后合法订阅可用。浏览器验收还需确认刷新及自动重连保留选定 Runtime 身份，同 ID 身份变化后必须重新点选。仅在专用测试进程中改变持久 Runtime 身份的夹具用于模拟执行身份变化，不代表生产生命周期支持直接修改该字段。

`TestAuthenticatedSubjectIsFixed` 检查同一 principal、同一 namespace 关联两个 subject 时的权限隔离，以及 CLI、连接凭据和安装材料在 SQLite 重开/PostgreSQL 跨池后保留原身份；`TestSchemaSevenRequiresKnownLoginSubject` 检查旧企业会话及安装材料撤销、本地访问和机器身份保留、迁移失败回滚。`TestExternalBrowserLoginCallbacks` 经真实宿主登录回调将已验证 subject 交给企业检查器；`pkg/access` 的真实执行流回归还验证 Bind 后不能替换 subject。这些使用可信测试身份适配器，不替代某个企业 IdP 或权限 SDK 的部署验收。

`make test TEST_PKGS=./internal/lifecycle` 验证个人默认续期决定，包括首次连接宽限期、曾可用后离线、引导失败、未知资源/调用、销毁限制、明确过期事实及配置边界。当前只有纯策略计算，没有提供方调用或持久调度；这些测试不证明资源已实际续期，也不替代 Managed 的创建、反向连接和清理验收。

生命周期 SQL 协调使用 `make test-race TEST_PKGS=./internal/metadata TEST_FLAGS='-run Operation -count=1 -timeout=180s'`，启用上述 PostgreSQL 配置后验证跨连接池领取、重开恢复、业务互斥、数据库租约到期、暂停后的条件写入、旧 worker 拒绝和等待连接时的维护串行化。`TestCommitAcknowledgementLossIsNotReplayed` 另覆盖意图、领取及完成的回执丢失；转库和原生备份测试保存实际 unknown 操作行。协调夹具使用已有 Runner 元数据，没有模拟或调用 Managed 提供方，不能用这些结果替代外部副作用与完整阶段恢复验收。

连接目录使用真实 PostgreSQL 的 `TestPostgresConnectionDirectory`、`TestPostgresDirectoryRechecksExpiredLeaseAfterLock` 和 `TestPostgresDirectoryCommitLoss`，验证跨池竞争、未确认发布隔离、旧 owner/绑定拒绝、恢复代次、锁等待后的过期检查及回执丢失不重放。`TestDirectoryBackendAndSchemaUpgrade` 检查 SQLite 拒绝集群、schema 9 升级与失败回滚；原生备份测试恢复实际 route 后旋转代次，确认历史归属不可用。`tests/TestPostgresClusterRecoveryCLI` 执行正式离线读取/旋转命令。它们仅证明目录事务与恢复工具，不代表 Gateway 已使用目录或三节点转发已经通过。

`pkg/fabricd/TestPostgresOwnedReverseConnections` 将两个独立 SQL 池与两个真实协议 Gateway、fabricd 引擎相连，检查 live owner 竞争拒绝、确认后发布、持续数据库续约、原请求 epoch、owner 释放后接管、旧清理拒绝和原 PTY 重新输入；测试中主动旋转恢复代次作为故障注入，验证已有旧连接关闭，不代表生产恢复可以跳过停站。`pkg/gateway/TestOwnership*` 和 `TestOwnedHandshakeRequiresEpochConfirmation` 检查迟到目录响应、保守本地期限、过期不复活及错误确认不发布；`TestInputGrantCannotCrossOwnershipTerm` 单独验证有效输入 grant 不能跨归属。此范围不涵盖 HTTP peer、负载均衡或三节点网络分区。

`pkg/gateway/TestOwnerExpiryClosesLocallyHandledStream` 检查没有 fabric relay 的本地处理流也随执行归属关闭；当前 route 的检查同时位于新请求及后续输入进入应用处理器之前，不能仅依赖关闭计时器及时获得调度。

宿主集群装配使用 `go test -race ./tests -run 'TestPostgresClusterWorkbench|TestPostgresHostClusterConfiguration' -count=1 -timeout=180s`，启用专用 PostgreSQL。两个正式 `host.App` 的 peer 经各自独立 mTLS 监听器连接，真实 fabricd 进程固定在 B，Web 与人类 CLI 固定在 A，验证远端在线事实、Runner/Runtime、PTY 和授权撤销。`internal/metadata/TestPostgresOnlineConnections` 检查未发布/过期/旧代次与有界 ID 查询，`pkg/host` 检查非法配置和监听器所有权。此范围不替代多进程集群故障、配置一致性租约或 Linux 部署验收。

官方集群 CLI 使用 `go test -race ./tests -run TestPostgresClusterCLIProcesses -count=1 -timeout=180s`。它启动三个正式服务进程及独立 fabricd，经私有配置加载测试证书，在共享 PostgreSQL 上验证跨进程安装、在线发现、Web/人类 CLI 定向执行和退出撤销。此正常运行检查不替代三节点故障矩阵；配置读取的私有权限、相对路径和证书错误另由 `internal/config/TestPrivateClusterConfiguration` 覆盖。

配置准入使用 `go test -race ./pkg/gateway ./internal/metadata ./tests -run 'Admission|InstanceAdmission' -count=1 -timeout=180s`，启用专用 PostgreSQL。覆盖并发不兼容配置、跨池续约、锁等待、未知提交、升级与原生恢复；宿主回归观察实际续约、Close 后保留原期限及到期后变更。`TestPostgresAdmissionSurvivesProcessPause` 暂停正式 CLI 宿主超过十五秒，确认恢复后旧进程退出、旧终端输入未执行、替换配置后原 PTY 可继续使用。它证明同机 PostgreSQL 准入与进程暂停边界，不证明数据库时钟跳变、三节点分区或 Linux 部署。

就绪与排空运行 `go test -race ./pkg/gateway ./pkg/host ./pkg/transport/peer ./internal/webapp -count=1 -timeout=90s`，覆盖入口/owner 排空期间原流双向收发、新流拒绝、清理回调等待，HTTP 完整响应/部分请求超时、挂载与自有监听器、并发 Close/Shutdown 及前缀探针。`go test -race ./tests -run TestWebCLIShutdownPreservesPTY -count=1 -timeout=90s` 启动正式 SQLite CLI 和真实 fabricd/PTY，验证 SIGTERM、限时退出、旧连接新请求拒绝、原终端重接与空闲隧道不延迟退出。该测试没有执行 Agent，也不替代 PostgreSQL 三节点分区或 Managed worker 排空验收。
