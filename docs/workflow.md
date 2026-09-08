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

运行前从私有配置设置 `DUNE_TEST_POSTGRES` 为测试连接 URL。测试创建随机 `dune_test_` 或 `dune_workbench_` schema，结束后删除；密码轮换测试还创建并清理随机测试角色，因此测试账号需要创建 schema 和角色的权限，服务器须实际使用密码认证。不要使用业务数据库。SQLite 的私有目录、独占锁、结构校验、并发与事务失败测试始终在临时目录运行。`TestSchemaInitializationIsAtomic` 验证建库失败完整回滚及不匹配结构不被改写；`TestPostgresConcurrentSchemaInitialization` 验证多个连接池同时创建当前结构。PostgreSQL 工作台测试与 SQLite 复用同一注册、CLI enrollment、fabricd 接入、真实 PTY 和退出撤销流程。

`TestPostgresBackupRestore` 还需要与服务器版本匹配的 `pg_dump`、`pg_restore`（从 PATH 查找，或通过 `DUNE_TEST_PG_BIN` 指定目录）。该测试只备份、删除并恢复自己新建的随机 schema，再逐字段核对记录和原会话/机器身份。工具缺失时明确跳过，不能计为备份验收。同结构备份恢复的操作顺序见[元数据存储与恢复](metadata-operations.md)。

`TestDurableAccessCredentials` 检查短期凭据的 SQLite 重开、PostgreSQL 跨池单次消费、期限、会话/身份源隔离及固定绑定。`TestPostgresAccessIssuerAndGateway` 用两个独立宿主和真实 fabricd/PTY，验证 A 签发后由 B 消费，并从 A 撤销 B 的空闲用户访问；它显式选择目标 Gateway，不证明 S3 的目录或自动跨节点转发。

协议 peer 回归使用 `go test -race ./pkg/gateway -run Peer -count=1 -timeout=60s`，覆盖双端请求授权、独立角色和启动身份、错误归属、禁止第三跳、上下文界限及断线不重拨。用户上下文另跑 `go test -race ./internal/metadata ./internal/authorization -run Peer -count=1 -timeout=90s`，验证 SQLite 重开、PostgreSQL 跨池单次消费、身份/请求/绑定隔离、有界期限与容量、提交回执丢失。PostgreSQL 子测试仍需专用配置，不把跳过计作通过。

启用专用 PostgreSQL 后运行 `go test -race ./pkg/fabricd -run TestPostgresOwnedReverseConnections -count=1 -timeout=90s`：三个独立协议 Gateway 使用各自的 peer HTTPS 监听器，经双向 TLS、WebSocket 和 Yamux 连接；客户端固定入口，fabricd 在两个 owner 间顺序迁移。验证目录续租、竞争拒绝、旧 peer 失效、显式重接后真实 PTY 保留及恢复代次隔离。用户上下文使用正式 SQL 签发与消费路径，验证入口允许不能覆盖目标拒绝；故意让入口的有效性回答保持过期状态后，另一 SQL 池退出用户，目标的独立检查仍关闭空闲终端。测试 CA 在内存中临时生成；SDK 与反向连接仍是本地协议夹具，不证明完整 host/CLI 部署或三节点进程/网络故障矩阵。

peer 传输本身运行 `go test -race ./pkg/transport/peer -count=1 -timeout=60s`，覆盖错误 CA/主机、独立 Handler 信任检查、明文与转发头、重复身份头、原 owner 协商、证书到期、取消、代理/重定向拒绝及真实 HTTPS CA 轮换。接入配置和独立监听器的职责见 [peer 传输接入](peer-transport.md)。

外部身份回归运行 `go test ./pkg/identity/... ./internal/metadata ./tests -run 'OIDC|ExternalIdentity|ExternalBrowserLogin' -count=1 -timeout=180s`，PostgreSQL 子测试沿用上述配置。签名协议测试使用本地 TLS issuer 和轮换密钥；宿主测试使用可信测试适配器，验证 SQLite 重开、PostgreSQL 跨实例回调及 Cookie 绑定。这些测试不替代真实身份源验收。

真实浏览器验收可使用独立 [Dex 2.45.1](https://github.com/dexidp/dex/releases/tag/v2.45.1) 和专用账号，按 [Dex 配置](https://dexidp.io/docs/configuration/) 注册精确 Dune 回调地址，将 client secret 保存在 0600 配置文件中。先验证官方 `web --identity-config` 的登录、退出和部署前缀，再用两个共享 PostgreSQL 的独立 `host.App` 分别接收 start/callback，确认跨实例完成；停用 Dune 用户后验证已有 Cookie 和再次上游登录均被拒绝，启用后仅新登录成功。使用 loopback HTTP 的演练不代表 HTTPS 代理、真实企业 IdP 或 S3 执行路由验收；完成后停止专用身份服务、工作台并清理测试 schema。

人类 CLI 回归使用 `go test ./tests -run TestHumanCLILoginAndExecution -count=1 -timeout=180s`，覆盖正式二进制的浏览器确认、私有文件、实际执行和浏览器退出后 API/现有连接撤销；启用 PostgreSQL 时还验证独立签发与消费宿主。`internal/metadata` 的 `TestCLILoginTransactions` 检查单次消费、确认身份不可替换、父会话与版本、并发和持久化；`pkg/login` 检查 HTTP 模糊失败及重定向不重放。真实 Dex 演练另运行 `dune login --site ... --no-browser`，完成浏览器核对与确认、临时 fabricd 上的实际命令，再退出浏览器验证 CLI 失效。手动启动临时 fabricd 时指定构建产物 `DUNE_TMUX=/absolute/repo/bin/tmux`，结束后清理测试进程和凭据。

`TestRunnerBindingSnapshot` 在 SQLite/PostgreSQL 验证逻辑 Runner 与机器 ID 分离、owner 隔离、绑定修订变化后旧访问失效及重开恢复；替换环境在测试中直接构造数据库事实，不代表 Managed 生命周期已经实现。`TestHumanCLILoginAndExecution` 另通过正式 CLI 的 `runners`、`--runner` 和 `DialRunner` 执行真实命令。`TestRunnerDoesNotFollowReplacement` 验证过期快照不会触发 SDK 自动重新解析或重放。

执行流授权运行 `go test -race ./pkg/access -count=1 -timeout=60s`。它使用独立 Gateway 和真实 fabricd/PTY，覆盖文件与上传提交、Git 暂存、端口输入、只读订阅、短期决定复用、空闲撤销和最终决定观测；不调用真实 Agent。宿主观测出口另运行 `go test -race ./pkg/host -run 'Observation|HostEmitsStructuredAccess' -count=1 -timeout=60s`，检查慢 sink 的有界队列、取消和丢弃计数，以及 API 检查事件的字段裁剪。完整语义见[执行流访问检查](access-checks.md)。

以下操作会使用已配置的 Agent 账号或指定远端，仅在任务包含对应验收且已有授权时运行。沿用明确指定的账号、模型和机器；历史文档里的地址与登录状态不是当前授权或可用性证据。环境缺失时报告具体缺口，继续本地检查。

```sh
# 本机真实 PTY 编码任务；默认使用已配置的 codex
DUNE_REAL_AGENT=1 go test ./tests -run '^TestRealAgentPTY$' -v -count=1 -timeout=180s

# 已配置的 Gemini ACP，仅验证 initialize
DUNE_REAL_AGENT=1 go test ./tests -run '^TestRealAgentACP$' -v -count=1 -timeout=180s

# 指定已有客户端配置；该测试会在远端 /tmp 创建并清理测试资源
DUNE_REMOTE_CONFIG=/absolute/client.yaml \
  go test -race ./tests -run '^TestDirectRemote$' -v -count=1 -timeout=180s
```

PTY 测试支持 `DUNE_PTY_AGENT=claude`，Dune 不代办登录或修改模型配置。上述测试超时还受测试内部 context 限制；增加 Go 的 timeout 不会延长内部期限。

`TestRealAgentPTY` 经 Dune 创建代码并独立运行检查；`TestRealAgentACP` 只测试握手。完整 Web 编码验收还需要从页面提交任务、看到实际结果并独立验证产物。mock ACP 可验证协议与权限状态机，不能替代真实模型任务。

发布构建不自动部署。Linux 运行验收应从当前源码重新构建对应发布包，在远端独立 `/tmp` 目录和端口启动，且让产品请求直接访问远端地址；不要把 SSH 转发或旧发布包当作目标平台证据。若验证 `service install`，使用唯一的临时 unit 名，检查重启前后的 Runtime ID、incarnation 和 generation，并在结束时显式 `runtime stop`、停用及删除 unit 和测试目录。对指定环境的操作应保留已有会话；连接服务重启只替换 fabricd，终止 tmux 会话需显式执行 `runtime stop`。

## 完成与续接

审阅本次差异并报告结果、实际验证和剩余限制。跨阶段任务可在现有进展文档中记录完成项、关键决定和下一步；小改动无需新建计划或验收档案。只有影响实现语义或验收状态时更新对应文档，避免多份清单重复维护。

当前目录若没有 Git 元数据，用修改清单和文件快照审阅；无需为执行开发流程创建 Git 仓库。将来接入 CI 时复用 Makefile 入口，外部账号/远端验收保持显式启用。

`TestEnterpriseSharedExecutionAndRevocation` 验证所选企业检查器允许跨 owner 的真实 Web 终端及 CLI 执行，同时拒绝文件写入、关闭策略撤销后的空闲流，保留父会话和身份停用撤销。PostgreSQL 子例使用两个独立宿主签发/消费凭据，并不代表 S3 自动路由。`TestAuthorizedDiscoveryAndWrites` 验证 128 候选扫描上限、分页游标的用户/身份源/用途隔离、SQLite 重开和跨 PostgreSQL 池恢复、绑定及会话写入竞争。浏览器分页改动还需在实际工作台验证空页继续、前后翻页、过期恢复及策略故障反馈。

`TestBrowserRunnerBindingIsFixed` 逐一检查浏览器 Runner 的 call、sessions、events 和解绑入口，覆盖绑定缺失/歧义、错误 Runner/Fabric/修订、会话缺失及修订变化后不拨号替代目标。`TestPrefixedWorkbenchEnrollmentAndTerminal/runner-entry` 和企业共享回归通过同一 Runner 快照路径使用真实 fabricd/PTY；浏览器需额外检查环境选择、绑定变化提示、明确重新进入后保留原会话及历史，不自动重连或重复创建。直接修改测试数据库的绑定修订只模拟已提交事实，不代表 Managed 生命周期验收。

`TestRuntimeSelection` 检查事件订阅的完整执行身份及重复、缺失、溢出参数；前缀工作台与企业共享进程回归在实际 PTY 上拒绝错误 incarnation/generation，并验证随后合法订阅可用。浏览器验收还需确认刷新及自动重连保留选定 Runtime 身份，同 ID 身份变化后必须重新点选。仅在专用测试进程中改变持久 Runtime 身份的夹具用于模拟执行身份变化，不代表生产生命周期支持直接修改该字段。

`TestAuthenticatedSubjectIsFixed` 检查同一 principal、同一 namespace 关联两个 subject 时的权限隔离，以及 CLI、连接凭据和安装材料在 SQLite 重开/PostgreSQL 跨池后保留原身份。`TestExternalBrowserLoginCallbacks` 经真实宿主登录回调将已验证 subject 交给企业检查器；`pkg/access` 的真实执行流回归还验证 Bind 后不能替换 subject。这些使用可信测试身份适配器，不替代某个企业 IdP 或权限 SDK 的部署验收。

`make test TEST_PKGS=./internal/lifecycle` 验证个人默认续期决定，包括首次连接宽限期、曾可用后离线、引导失败、未知资源/调用、销毁限制、明确过期事实及配置边界。该包只验证纯策略计算；持久巡检另见下方回归。两者都不证明资源已实际续期，也不替代 Managed 的创建、反向连接和清理验收。

生命周期 SQL 协调使用 `make test-race TEST_PKGS=./internal/metadata TEST_FLAGS='-run Operation -count=1 -timeout=180s'`，启用上述 PostgreSQL 配置后验证跨连接池领取、重开恢复、业务互斥、数据库租约到期、暂停后的条件写入、旧 worker 拒绝和等待连接时的维护串行化。`TestCommitAcknowledgementLossIsNotReplayed` 另覆盖意图、领取及完成的回执丢失；原生备份测试保存实际 unknown 操作行。协调夹具使用已有 Runner 元数据，没有模拟或调用 Managed 提供方，不能用这些结果替代外部副作用与完整阶段恢复验收。

Managed 模板与创建入口使用 `go test -race ./pkg/fabric ./internal/managed ./internal/metadata -run 'CreateRequest|Template|ManagedCreation|ManagedAndAttached' -count=1 -timeout=120s`，启用专用 PostgreSQL。覆盖不可变模板目录、精确类型/范围/大小、非法 Unicode、逐模板访问过滤、策略故障关闭、创建双重检查、事务内原会话复核、同一请求键并发去重、完整回滚、Attached/Managed 共享数量上限，以及回执丢失后按原键核对。原生 PostgreSQL 备份测试另包含创建快照。这里验证公开模板契约及内部创建服务；尚未装配宿主/Web、提供方或 worker，不能据此宣称 Managed 环境已经创建。

提供方动作记录使用 `go test -race ./internal/metadata -run 'ProviderAction|OperationWriteRechecks|BackupRestore' -count=1 -timeout=120s`，启用专用 PostgreSQL 和原生备份工具。验证首次派发预约、未知提交不授予派发、接管者只核对、旧 worker/绑定拒绝、动作与资源原子提交、同一 Fabric 内资源唯一性、续期绝对期限，以及引导结束后的互斥释放。原生恢复保留未决动作键、已知资源和首次确认时间。动作结果使用可信适配器事实夹具；它不证明真实提供方的去重、旧修订拒绝、引导或销毁已运行。

Managed 创建执行器使用 `go test -race ./internal/managed -run Executor -count=1 -timeout=90s`。它通过公开 `fabric.CreateProvider` 夹具验证预约明确提交后只调用一次 Create，调用携带原动作键和执行修订；unknown、timeout 和再次执行只走 Reconcile，终态不再调用适配器，确认事实与 Operation 同事务保存。夹具可证明调用编排和不重放边界，不能替代真实提供方的关联查询、去重、外部 fencing 或反向连接验收。

Managed 创建恢复循环使用 `go test -race ./internal/managed ./internal/metadata -run 'Worker|RecoverableManagedCreate' -count=1 -timeout=120s`，PostgreSQL 子用例需要专用测试库。检查数据库时钟筛选、锁内阶段复核、租约竞争、长调用续约、provider deadline 后仍能落库、unknown 到期前不核对、到期后只 Reconcile、终态交还执行租约，以及不领取未配置 Fabric。这里的 provider 仍是可信夹具，worker 尚未由宿主启动。

宿主排空中的 Managed worker 边界使用 `go test -race ./internal/managed ./pkg/host -run 'Worker(Drain|ClosedDrain)|ManagedWorkerParticipatesInHostShutdown' -count=1 -timeout=120s`。回归检查已关闭 drain 不领取初始操作、已领取提供方调用在预算内完成后不领取下一操作，以及 deadline 取消当前调用并等待 worker 退出。测试使用可取消的可信适配器，不证明外部 SDK 一定遵守 context；实际适配器必须自行验证取消和幂等查询语义。

结构化观测运行 `go test -race ./pkg/gateway ./pkg/host -run 'GatewayEmits|Observation|HostEmitsStructuredAccess|ManagedProviderCalls' -count=1 -timeout=120s`。覆盖本地与 peer 流、连接/路由生命周期、peer owner/epoch、容量拒绝、Managed Create 调用身份与保守错误归一化、回调 panic 隔离，以及宿主慢 sink 的有界队列和丢弃计数。Managed 用例还检查 provider 错误正文和未校验资源引用不会进入事件。事件不包含业务载荷；测试采集器也不是可靠审计存储。

Managed 资源绑定 enrollment 使用 `go test -race ./internal/metadata -run 'ManagedBootstrap|Enrollment' -count=1 -timeout=120s`，并为 PostgreSQL 配置专用测试库。检查 Bootstrap 动作与令牌哈希原子提交、明文不落库、提交回执丢失不重发、资源/Fabric/修订与数据库时钟有效期复核、访问检查区分 Managed、并发单次消费及绑定已有 Runner。原生 PostgreSQL 备份恢复还保留未消费的绑定 grant。

Managed Bootstrap 执行器使用 `go test -race ./internal/managed ./internal/metadata -run 'BootstrapExecutor|ManagedBootstrap' -count=1 -timeout=120s`。检查公开地址和安装版本摘要、首次调用携带资源绑定 grant、重复及接管只核对原动作、配置变化不改写旧动作、已消费 grant 通过数据库绑定收敛、provider deadline 后仍写入 unknown/timed_out，以及缺失适配器不会预约动作。这里仍使用可信夹具，不代表任何真实提供方或反向 WS 已验收。

Managed 阶段 worker 使用 `go test -race ./internal/managed ./internal/metadata -run 'Worker|RecoverableManaged|ScheduledManagedRenewal' -count=1 -timeout=180s`。检查巡检 → 已有 Renew → 冻结 Renew → Bootstrap → create 的优先级、数据库时钟下的资源与动作复核、终态交还租约、timeout 后仅核对、按能力配置的 Fabric 过滤以及锁外快照失效后拒绝领取。SQLite 与 PostgreSQL 都应执行；测试适配器不证明安装脚本、真实资源或首次反向连接可用。

Managed 首次连接确认使用 `go test -race ./pkg/gateway ./internal/metadata -run 'DaemonOnlineCallback|ManagedCreateFinishes|AttachedMachineOnline' -count=1 -timeout=120s`。Gateway 回归检查回调位于输入确认和集群 Publish 之后、本机路由可见之前，失败或超时不暴露路由，并隔离回调对绑定快照的修改。metadata 在 SQLite/PostgreSQL 检查 Bootstrap 前拒绝、资源和绑定复核、精确集群 route、提交回执丢失及重连幂等；PostgreSQL 子例需要专用测试库。这里确认的是可信 Gateway 观察与持久创建状态的衔接，尚不证明某个真实提供方已成功安装并回连。

Managed 巡检和续期执行使用 `go test -race ./internal/lifecycle ./pkg/fabric ./internal/managed ./internal/metadata -run 'Renewal|Inspection|ConfirmedGone|FirstOnline' -count=1 -timeout=240s`。SQLite/PostgreSQL 元数据回归检查数据库时钟候选、跨池单次领取、租约续约与旧修订拒绝、策略版本重算、unknown 不覆盖旧到期事实、首次在线重新激活、确认 Gone 原子关闭访问、冻结计划单次消费、互斥操作阻塞、过期目标、提交回执丢失及原生备份恢复。worker 回归检查只读巡检、首次 Renew 与 Reconcile 的权限分离、稳定动作身份、确认期限、错误字段丢弃、deadline 独立落库、长调用续租、能力过滤和优先级。适配器仍为可信夹具；这些结果证明 Dune 的调用编排和落库边界，不证明真实提供方已执行续期、具备动作去重或旧执行者 fencing。

Managed 销毁接受阶段使用 `go test -race ./internal/authorization ./internal/managed ./internal/metadata -run 'ManagedDestroy|DestroyChecks|DestroyRejects|Discovery' -count=1 -timeout=180s`。SQLite/PostgreSQL 回归覆盖 `runner.destroy/managed` 的固定身份、Fabric 和 binding scope，事务内浏览器复核，并发幂等、冲突生命周期互斥、提交回执丢失、访问/续期/enrollment/机器/票据的原子失效、无机器时直接确认关闭、集群实例关闭待办快照，以及提供方已确认 Gone 时收敛完成。

Managed 销毁执行使用 `go test -race ./pkg/gateway ./internal/managed ./internal/metadata -run 'Disconnect|ManagedDestroy|GatewayClosure|DestroyExecutor|WorkerAdvancesDestroy' -count=1 -timeout=180s`。回归检查 Gateway 的目标级不可逆关闭、连接/流/清理回调完成边界、按 live 实例持久扇出、单个确认不能越过集群等待、精确幂等确认、数据库 deadline 到期记录 timed_out、Fabric 过滤和锁内复核，以及首次 Destroy、接管后只 Reconcile、错误字段丢弃、Gone 终态和 worker 优先级。deadline 超时允许资源清理继续，但不等于未确认实例已经关闭；适配器仍为可信夹具，真实平台的动作去重、旧执行者 fencing 和 SDK 超时行为仍须真实验收。

Managed 人工核对使用 `go test -race ./internal/managed ./internal/metadata ./pkg/host -run 'ManagedReview|Manual|ManagedHost' -count=1 -timeout=180s`。SQLite/PostgreSQL 元数据回归检查当前浏览器与绑定复核、幂等请求、同一 action 单个待办、跨池单次领取、Operation 与核对租约同步、普通恢复先完成时的审计收敛和原生备份恢复；执行器检查只读 Reconcile、候选引用适配器核验、原动作身份、引用冲突和错误保守性。宿主回归检查前缀 API、跨用户隐藏及响应字段边界。这里验证的是 Dune 的受控核对路径；真实平台仍须证明候选关联查询和动作历史保留语义。

Managed 宿主与工作台装配使用 `go test -race ./internal/managed ./internal/webapp ./pkg/host -run 'Managed|Status' -count=1 -timeout=180s`，再运行 `make web-check web-build`。回归覆盖完整提供方集合、启动失败释放 SQLite、后台 worker 实际领取、启动能力、模板/创建/Operation/Runner 状态 API、跨主体隐藏和内部动作键不出现在响应；前端类型与生产构建检查动态模板表单、精确 `int64` JSON、持久状态和 Managed/Attached 分流。提供方仍是可信夹具，且当前没有浏览器自动化，因此这些检查不证明真实云资源闭环或最终视觉交互。

Managed 运维快照使用 `go test -race ./internal/metadata ./pkg/host -run 'ManagedStatusSnapshot|ManagedProviderCalls' -count=1 -timeout=120s`，并为 PostgreSQL 配置专用测试库。回归建立正常绑定、部分 unknown 资源、timed-out Operation 和访问已关闭资源，检查数据库时钟、策略版本漂移、续期积压、到期提前量、残留去重及无 Managed 配置的零状态；宿主用例确认 provider 事件之后只能从已持久事实观察 unknown。快照不执行 provider 探测，不能作为真实平台可用性测试。

Managed 多实例执行栅栏使用专用 PostgreSQL 运行 `go test -race ./internal/managed -run '^TestPostgresWorkerTakeoverRejectsLateCreateResult$' -count=1 -timeout=60s`。测试让第一个连接池派发 Create 后阻塞到数据库租约过期，第二个连接池接管同一 Operation 并且只用原 action ID、issuer 和 execution revision 查询事实；旧调用随后返回的成功结果必须得到 lease lost，不能覆盖接管者确认的资源。它验证 Dune 的提交栅栏和接管路径；外部平台仍须用自身动作键或修订规则处理已经到达平台的迟到调用。

Managed 历史保留使用 `go test -race ./internal/metadata ./internal/managed ./pkg/host -run 'ManagedHistory|OperationClaimsAndBusinessMutex|ManagedCreateFinishes|ManagedDestroy|Worker|ManagedHost' -count=1 -timeout=240s`，并为 PostgreSQL 配置专用测试库。回归检查数据库完成时间、24 小时至 366 天配置、32 条删除上限、续期历史清理、完整终态 Runner 归档、跨池 `SKIP LOCKED` 竞争，以及近期请求、unknown/timed_out、残留资源、未确认关闭、机器和 enrollment 的保留。自动清理只在生命周期工作为空时运行；测试直接调整夹具的历史时间，不代表修改生产数据库或系统时钟。

连接目录使用真实 PostgreSQL 的 `TestPostgresConnectionDirectory`、`TestPostgresDirectoryRechecksExpiredLeaseAfterLock`、`TestPostgresDirectoryCommitLoss` 和 `TestPostgresDirectoryDatabaseClockDisturbance`，验证跨池竞争、未确认发布隔离、旧 owner/绑定拒绝、恢复代次、锁等待后的过期检查、回执丢失不重放，以及数据库时间前跳/回拨后的期限与 epoch 栅栏。时钟用例只在测试 schema 中用同名函数替换目录 SQL 读取的 `clock_timestamp()`：回拨保留数据库原期限但本地下发仍最多十五秒，前跳使旧续约失败并由新 epoch 接管，恢复正常后未来期限继续阻止第三个 owner。`TestSQLiteRejectsSharedClusterServices` 检查 SQLite 拒绝集群目录和共享准入；原生备份测试恢复实际 route 后旋转代次，确认历史归属不可用。`tests/TestPostgresClusterRecoveryCLI` 执行正式离线读取/旋转命令。这些检查不修改操作系统或 PostgreSQL 进程时钟，也不替代 Gateway 三进程、跨主机 NTP 或数据库 HA 演练。

`pkg/fabricd/TestPostgresOwnedReverseConnections` 将两个独立 SQL 池与两个真实协议 Gateway、fabricd 引擎相连，检查 live owner 竞争拒绝、确认后发布、持续数据库续约、原请求 epoch、owner 释放后接管、旧清理拒绝和原 PTY 重新输入；测试中主动旋转恢复代次作为故障注入，验证已有旧连接关闭，不代表生产恢复可以跳过停站。`pkg/gateway/TestOwnership*` 和 `TestOwnedHandshakeRequiresEpochConfirmation` 检查迟到目录响应、保守本地期限、过期不复活及错误确认不发布；`TestInputGrantCannotCrossOwnershipTerm` 单独验证有效输入 grant 不能跨归属。此范围不涵盖 HTTP peer、负载均衡或三节点网络分区。

`pkg/gateway/TestOwnerExpiryClosesLocallyHandledStream` 检查没有 fabric relay 的本地处理流也随执行归属关闭；当前 route 的检查同时位于新请求及后续输入进入应用处理器之前，不能仅依赖关闭计时器及时获得调度。

宿主集群装配使用 `go test -race ./tests -run 'TestPostgresClusterWorkbench|TestPostgresHostClusterConfiguration' -count=1 -timeout=180s`，启用专用 PostgreSQL。两个正式 `host.App` 的 peer 经各自独立 mTLS 监听器连接，真实 fabricd 进程固定在 B，Web 与人类 CLI 固定在 A，验证远端在线事实、Runner/Runtime、PTY 和授权撤销。`internal/metadata/TestPostgresOnlineConnections` 检查未发布/过期/旧代次与有界 ID 查询，`pkg/host` 检查非法配置和监听器所有权。此范围不替代多进程集群故障、配置一致性租约或 Linux 部署验收。

官方集群 CLI 使用 `go test -race ./tests -run TestPostgresClusterCLIProcesses -count=1 -timeout=180s`。它启动三个正式服务进程及独立 fabricd，经私有配置加载测试证书，在共享 PostgreSQL 上验证跨进程安装、在线发现、Web/人类 CLI 定向执行和退出撤销。此正常运行检查不替代三节点故障矩阵；配置读取的私有权限、相对路径和证书错误另由 `internal/config/TestPrivateClusterConfiguration` 覆盖。

多用户集群能力使用 `go test -race ./tests -run TestPostgresClusterTwoUserIsolationAndCapabilities -count=1 -timeout=120s`。三个正式 Web 节点共用 PostgreSQL，两名本地身份用户各自拥有一条连接到不同 owner 的真实 fabricd 隧道；统一入口只发现本人的机器，不能为另一台机器签发访问，绑定正确机器的一次性票据也不能改投另一目标且错误握手后不可重放。两个用户分别经 mTLS peer 完成 File 读写、Git 状态、PTY 输入和原始 ACP 消息。这是产品目标与能力路由隔离；两个 fabricd 仍运行于同一测试 OS 用户，不把它表述为 Shell 内部的主机文件隔离。

配置准入使用 `go test -race ./pkg/gateway ./internal/metadata ./tests -run 'Admission|InstanceAdmission' -count=1 -timeout=180s`，启用专用 PostgreSQL。覆盖并发不兼容配置、跨池续约、锁等待、未知提交与原生恢复；宿主回归观察实际续约、Close 后保留原期限及到期后变更。`TestPostgresAdmissionSurvivesProcessPause` 暂停正式 CLI 宿主超过十五秒，确认恢复后旧进程退出、旧终端输入未执行、替换配置后原 PTY 可继续使用。它证明同机 PostgreSQL 准入与进程暂停边界，不证明数据库时钟跳变、三节点分区或 Linux 部署。

就绪与排空运行 `go test -race ./pkg/gateway ./pkg/host ./pkg/transport/peer ./internal/webapp -count=1 -timeout=90s`，覆盖入口/owner 排空期间原流双向收发、新流拒绝、清理回调等待，HTTP 完整响应/部分请求超时、挂载与自有监听器、并发 Close/Shutdown 及前缀探针。`go test -race ./tests -run TestWebCLIShutdownPreservesPTY -count=1 -timeout=90s` 启动正式 SQLite CLI 和真实 fabricd/PTY，验证 SIGTERM、限时退出、旧连接新请求拒绝、原终端重接与空闲隧道不延迟退出。该测试没有执行 Agent，也不替代 PostgreSQL 三节点分区或 Managed worker 排空验收。

`TestPostgresClusterNetworkPartitions` 需要专用、可由本机回环地址访问的 PostgreSQL 测试服务器，和正常三进程回归一样使用正式 CLI、测试 CA、真实 fabricd/PTY。测试只在各节点的 PostgreSQL/peer 字节链路上暂存双向流量，不改 SQL 或协议、不关闭 TLS 校验；恢复后释放滞留字节。它验证已受理 Exec 的响应丢失返回 unknown 且不重放、入口 SQL 失联后退出及另一入口使用原 owner、owner SQL 失联后退出及 fabricd 重连到第三节点时 epoch/连接 generation 推进。恢复后的原 PTY 和一次副作用分别独立核对。组合运行 `go test -race ./tests -run 'TestPostgresClusterCLIProcesses|TestPostgresClusterTwoUserIsolationAndCapabilities|TestPostgresClusterNetworkPartitions|TestPostgresAdmissionSurvivesProcessPause' -count=1 -timeout=300s`；本机代理停滞、单机进程暂停、schema 内数据库时间偏移和正常链路分别给出独立证据，不等同于跨主机内核丢包、操作系统时钟修改、Linux 或 Managed worker 的完整故障组合。

当前 Linux 集群发布验收从目标提交重新交叉构建静态 amd64 二进制，并与固定 bundled tmux 部署到专用远端目录。三个正式 Web/peer 进程应使用独立端口、同一恢复代次与独立 mTLS 证书，两份正式 fabricd 分别连接不同 owner；另一主机上的客户端从第三个入口验证双用户发现隔离及 Exec、File、Git、PTY、原始 ACP。重启一个 owner 时记录原 fabricd PID 与 Runtime 身份，等待 readiness 与目录恢复后重新附加原 PTY，同时检查另一 owner 路径。验收材料必须记录目标提交、内核/架构、数据库位置和清理结果；同机三实例及跨主机 PostgreSQL 不能表述为三台 Linux 主机、Linux 数据库 HA、真实 Agent 或 Managed 提供方验收。

PostgreSQL 容量基线使用 `DUNE_PERFORMANCE_BASELINE=1 go test ./internal/metadata -run '^TestPostgresEnterprisePerformanceBaseline$' -count=3 -timeout=300s -v`，并通过 `DUNE_TEST_POSTGRES` 指向专用数据库。固定数据集为 128 台已在线 Managed machine 与 128 条到期续期计划，并发数为 16；输出目录 Resolve/续租及续期扫描/领取的吞吐和 p50/p95/p99。用 `-race -count=1` 另验并发正确性，性能数字只取无 race 运行。记录 CPU、内存、OS、Go、PostgreSQL 版本及运行次数；没有业务目标时只报告观测值和波动，不设置门槛，也不把本机回环结果外推到跨主机数据库或真实 Provider。
