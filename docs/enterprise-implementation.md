# 企业扩展方案实施记录

实施依据：[企业扩展方案](enterprise-extensibility-plan.md)。按独立功能提交，阶段只有取得对应证据后才标记完成。S0a、S0b、S0c 已通过本地验收；S1–S3 尚未交付。

## S0a：连接与传输

### 已完成：默认拨号与协议分离

- `pkg/transport/ws` 提供默认 WS 拨号和 `net.Conn` 适配；`internal/wire` 只保留 Yamux 配置、消息编解码和握手，不再导入 HTTP、TLS 或 WebSocket 实现。
- `sdk.Connect(ctx, conn, target)` 接收已建立的连接；原 `sdk.Dial` 仍装配默认 WS 拨号。两者取得连接所有权，握手失败或取消会关闭连接；连接建立后由 `Client.Close` 管理生命周期。
- fabricd 的 `ServeConn` 承担一次反向连接的协议处理，拨号与退避仍由启动入口装配。连接取消不会销毁 tmux 会话，也不重放业务请求。
- 验证：`DUNE_REAL_AGENT= DUNE_REMOTE_CONFIG= make test` 全部本地回归通过，包括默认 WS 分片、PTY、ACP mock、文件、Git、并发和重连的进程测试；`make check-go` 通过；Gateway、daemon、SDK 的 race 检查通过。SDK 额外覆盖等待握手响应时取消及建立连接后的 context 所有权。

### 已完成：Gateway 接入与请求处理链分离

- `pkg/gateway` 仅依赖字节连接、协议与执行类型。`ServeConn` 必须显式提供可信协议绑定及连接处理器；`pkg/access` 保存每连接的授权关联，`pkg/transport/tunnel` 完成认证与 WS Upgrade。`internal/gateway` 仅保留官方独立入口的配置和监听装配。
- 处理器通过 `Open` 处理首条请求，通过受控 `Stream.Forward` 选择转发，或通过 `Send` 在模块中处理。后续消息按方向有序交给 `Message` 检查；模块不能读取裸流。关闭通知恰好一次，写入有界且串行，钩子期限为 1 秒；受信任的宿主回调必须响应 context 取消。
- 现有 Web owner/会话检查通过独立连接处理器保留，空闲连接也每秒检查本地授权有效性。这只是现有个人访问检查的迁移，还不是 S1 的企业操作级 AccessChecker。
- `wire.Stream.Close` 明确取消双向 I/O；WS 适配在关闭时解除 fasthttp 默认 hijack 所有权造成的退出等待。修复由空闲流取消测试和 Gateway 进程退出回归发现。
- `samples/gateway` 仅通过公开包嵌入标准 HTTP 服务，无需产品数据库。标准 HTTP 与 fasthttp 默认入口都接入同一 core。
- 验证：全量本地 `make test`、相关 race 和 `make check-go` 通过；补充覆盖缺失处理器、首请求拒绝、后续输入拒绝、空闲撤销、钩子超时、模块内处理、关闭通知和 fasthttp 退出。`TestTmuxSurvivesFabricdAndGateway` 验证 Gateway 重启及 fabricd 强制/正常退出保留 PTY。检查 `pkg/gateway`、`internal/wire` 导入，不含 HTTP/WS、SQL、用户或 Runner 模块。

### 已完成：公开 fabricd 执行引擎

- 执行实现迁至 `pkg/fabricd`，公开 `Open`、`Engine.ServeConn`、`Engine.Close`。`internal/daemon` 只装配官方配置、WS 拨号与重连，不再包含执行逻辑。执行包不导入应用配置或 HTTP/WS 实现。
- 状态目录在引擎存活期间独占；关闭先禁止新请求、取消连接并等待已受理处理退出，再清理 ACP/临时上传并释放锁，保留 tmux 内容。失败的重复打开不能释放原引擎的锁。
- `RunHelper` 明确暴露既有进程守护和临时上传清理子命令；官方 CLI 和 `samples/fabricd` 均使用它。宿主须在解析自己的命令行之前分派该入口，部署时提供 tmux。
- `TestExternalFabricdHost` 将示例复制到独立 module 构建，以默认 WS 接入 Gateway 并验证真实命令执行及上传提交，不依赖用户库或产品数据库。配套目录互斥、关闭中断握手及关闭后拒绝连接测试；本地全量、fabricd race、PTY 进程保留回归及 vet 通过。

### 已完成：客户端包依赖边界

- `pkg/client` 提供纯连接协议客户端，`pkg/sdk` 保留默认 WS 拨号和现有类型/API，通过类型别名复用同一实现。
- 检查 `pkg/client`、`pkg/gateway`、`pkg/fabricd` 和 `internal/wire` 的导入，均不依赖产品身份、应用配置、SQL 或 HTTP/WS 实现；默认 SDK 的既有调用方不需要修改。
- 验证：全量本地 Go 回归、客户端/Gateway/接入 race 检查及 vet 通过，包含默认 WS、独立宿主、执行与持久 PTY 的进程回归。

下一检查点为 S0b：可复用 Web/API 与宿主组装入口、有限启动能力和部署前缀。宿主 SDK、真实身份源、Managed 提供方及集群验收仍待实施和实际环境验证。

## S0b：模块与应用装配

### 已完成：部署地址与前缀

- `pkg/deployment` 统一规范化浏览器地址、Origin、Cookie Path 和完整机器 WS(S) 地址；拒绝用户信息、查询/fragment、非法端口、通配地址及有歧义的路径。默认机器地址从 `public_url` 派生；独立入口通过 `gateway_url` 显式覆盖。
- 官方 Web 入口通过 `--url` 支持前缀，代理保留前缀，服务器只映射一次。静态资源、HTTP API、浏览器 WS、安装和注册使用同一地址契约；`--gateway-url` 不改变浏览器入口或内部 SDK 连接路径。fabricd 配置接受完整连接路径，不再强制根 `/tunnel`。
- 前端构建使用相对资源地址，API 和终端/ACP 订阅保留部署目录。Cookie 使用部署路径，Origin 只比较协议、主机和端口。浏览器在 `/tools/dune/` 下完成登录、在线机器展示、真实终端输入/输出、刷新保留登录及退出；页面实际显示 `DUNE_PREFIX_UI_OK`。
- 回归覆盖根路径与前缀下的资源、Cookie、安装地址和覆盖隔离；`TestPrefixedWorkbenchEnrollmentAndTerminal` 经过真实注册、CLI 绑定、fabricd 反向连接、Web API、终端 WS 和退出撤销，分别验证默认入口及另一个监听地址/路径的机器入口。创建后的输入订阅释放是异步的，测试与浏览器一样仅在明确 INPUT_OWNED 时重连既有会话，不重放创建或输入。
- 验证入口：全量本地 Go 回归、Web/接入/地址模块 race、vet、`make web-check web-build`，以及实际浏览器交互。构建仍报告现有主 bundle 体积提示。

### 已完成：有限启动信息与注册入口

- 匿名 `GET api/bootstrap` 只返回已装配登录方式及公开入口、本地注册开关、Attached/Managed 和公开地址，禁止缓存。当前本地密码与 Attached 可用，Managed 为 false；不提供能将未实现提供方标成可用的配置。
- 工作台先读取启动信息再显示入口，失败提供明确错误和重试。`--disable-registration` 同时关闭页面注册入口与服务端注册操作，保留已有账号登录。
- 验证：Web 后端回归覆盖公开字段白名单、可信地址、关闭注册不创建账号、已有账号登录；`make check-go`、`make web-check web-build` 通过。在独立 Chrome 会话中通过真实登录、退出及前缀页面验证，并注入一次 bootstrap 网络失败，确认错误页重试后恢复登录表单。构建仍报告现有 bundle 体积提示。

### 已完成：基础宿主装配与独立工作台

- `pkg/host.Open` 显式装配现有本地账号、owner 检查、Attached、Gateway 和工作台。公开参数不暴露应用配置、内部 Store 或企业依赖；S0b 的过渡存储已在 S0c 替换为 SQL。
- `App` 可挂载到宿主已有 HTTP 服务，或通过 `Serve` 接管传入 listener。外部 context 取消和幂等 `Close` 停止准入、取消请求及连接，关闭自有 listener，等待已接收处理结束后释放元数据锁；`Done` 可等待实际完成。HTTP 中间件须保留升级与 ResponseController 能力，以解除未发完请求体等 I/O 等待。
- Web/API 处理不再读取 CLI 配置、推断监听地址或管理 HTTP 服务。`cmd/dune/web.go` 负责官方配置与 TLS/loopback 地址，并通过同一宿主入口启动。`DialGateway` 只建立认证字节连接，Web 仍通过真实 Gateway 网络链路执行协议握手与请求，不绕过处理器。
- `samples/workbench` 在宿主自有 `/health` 路由旁挂载前缀工作台。进程回归将源码复制到独立 Go module，以 race 构建并完成静态挂载、注册、CLI 绑定、fabricd、PTY 输入输出和退出撤销；浏览器在该示例中登录并运行真实终端，显示 `DUNE_HOST_UI_OK`。
- 验证：全量本地 `make test`、host/Web race、vet 通过；新增父 context 取消、多 listener 关闭、存储独占及重开、关闭后拒绝请求、外部 HTTP 未完成请求取消与宿主路由保留测试。

### 已完成：本地身份与连接访问模块

- `internal/identity` 承担账号规范化、密码验证、会话期限和数量策略。Web 负责 Cookie 与 HTTP，存储按内部领域契约原子提交账号/首个会话及后续会话；注册开关由身份模块执行，启动信息读取同一配置。存储故障与无效登录分开返回。
- `internal/authorization` 创建固定用户/目标的连接授权，管理一次性临时 Gateway 凭据及其释放。连接建立后继续检查原会话与归属；释放临时凭据不撤销已建立连接，退出登录或解绑则使对应授权失效。机器凭据只能取得 daemon 角色，浏览器 Cookie 不能作为隧道凭据。
- `pkg/host` 从同一内部后端显式构造身份和访问模块，再交给 Web/API；handler 不再保存临时 SDK ticket 或决定其有效性。协议 core 不新增产品依赖。新内部领域接口不构成宿主可独立替换各个 Store 的承诺。
- 全量本地 `make test`、身份/访问/Web/host 相关 race 和 vet 通过。定向回归覆盖账号/会话写入失败整体回滚、取消不写入、会话上限、凭据一次消费、凭据类型隔离、两个用户连接及撤销隔离。此处仍是默认本地身份与 owner 策略；企业 IdentityProvider/AccessChecker 和所有子操作映射属于 S1。

下一检查点为 S0c：SQLite/PostgreSQL、统一事务后端、连接鉴权注入和 JSON 导入。企业身份与登录回调随 S1 验收。

## S0c：SQL 存储与迁移

### 已完成：统一 SQL 后端基础

- `internal/metadata` 实现同一组身份、会话、Attached Runner/机器及 enrollment 事务，使用一个 SQL 连接池和内部事务入口；不开放可分别替换的领域 Store。账号与首个会话、消费 enrollment 与 Runner/机器身份均一起提交。提交确认丢失返回明确的结果未知错误，不自动重放。
- SQLite 使用私有文件、WAL、外键和完整同步，沿用原 `accounts.lock` 排斥同目录的旧进程及第二实例；拒绝符号/硬链接和未经导入的旧 JSON。PostgreSQL 用事务内 schema 锁协调初始化，用 principal 行锁保证跨连接池的注册和会话数量限制。初始 Attached Runner ID 沿用机器 ID，绑定修订为 1。
- `pkg/storage.Config` 只选择支持的后端与连接配置。PostgreSQL 的 `BeforeConnect` 对每条新物理连接取得独立配置副本，可通过私有 SDK 更新鉴权；连接仍须属于同一数据库命名空间。采用固定版本的 [modernc SQLite](https://pkg.go.dev/modernc.org/sqlite) 与 [pgx stdlib](https://pkg.go.dev/github.com/jackc/pgx/v5/stdlib)，SQL 驱动未进入协议核心。
- 本机独立 PostgreSQL 17 实例使用真实 SCRAM 密码认证。共同契约已验证跨对象失败回滚、并发单次消费、跨池会话上限、重启恢复和撤销；另外验证并发 schema 初始化、实际密码轮换及池恢复。提交丢失测试在真实 SQLite 提交之后注入回执错误，确认只提交一次并保留已落库状态。
- 验证：启用该独立 PostgreSQL 的全量 `make test`、SQL `make test-race` 和 `make check-go` 通过；新增存储包以 `CGO_ENABLED=0` 交叉编译通过 Linux amd64/arm64、macOS amd64，本机 macOS arm64 完成运行测试。交叉编译不代替目标平台运行验收。

### 已完成：离线 JSON 导入与 SQL 工作台

- `pkg/migrate.JSON` 和 `dune metadata import-json` 将旧目录导入空 SQLite/PostgreSQL 目标。离线命令不要求 Gateway 配置；源目录持锁至结束，严格校验重复键（含大小写字段别名）、版本、凭据唯一性及关联，失败不改源文件。所有业务记录同事务导入，保留账号、密码/凭据哈希、原机器 ID 和会话/enrollment 到期时间。
- 官方 CLI 与 `pkg/host` 默认使用 SQLite，另可通过私有 `--database-config` 文件或 Go `Options.Database` 选择 PostgreSQL。旧 JSON 运行时和相关内存保存代码已删除；有旧数据但未导入的目录明确拒绝启动。身份策略测试迁至同一 SQL 契约，覆盖错误密码、取消、过期、会话上限与凭据哈希。
- SQL 故障不会伪装成账号错误或机器不存在；提交确认丢失返回 `RESULT_UNKNOWN`，普通后端故障返回 503，禁止自动重放。连接期间的持久访问查询有界，注销后空闲终端仍会被撤销。
- 两种后端均通过注册、CLI enrollment、真实 fabricd 接入、PTY 输入输出和退出撤销；外部 Go module 宿主继续使用同一 SQL 应用。真实 PostgreSQL 启用下的全量 `make test`、metadata/Web/host `make test-race` 和 `make check-go` 通过；最后新增的 JSON 字段别名回归也通过。
- 真实升级演练使用 `8aba205` 的旧 JSON 工作台建立账号、机器和 PTY，停站备份并导入 SQLite 后在原地址启动新版本。原 Cookie/密码、机器配置、Runtime ID、incarnation/generation 及终端历史保持不变，fabricd 无需重装或重新签发凭据；浏览器登录并连接该原会话，看到 `BEFORE_SQL_IMPORT_OK`、`AFTER_SQL_IMPORT_OK`，输入后显示 `SQL_MIGRATION_UI_OK`。完成后已停止测试 Runtime、fabricd 和站点。
- 安装配置、导入顺序和回退边界见[元数据迁移](metadata-migration.md)。SQL 转库与备份恢复验收见下节，S1–S3 仍未交付。

### 已完成：SQLite 转库与 SQL 备份恢复

- `pkg/migrate.SQLite` / `dune metadata copy-sqlite` 将停站 SQLite 的全部当前业务字段复制到空 SQLite/PostgreSQL 目标。源库独占且不会被初始化或升级，复制使用单一源快照和目标事务，按外键顺序逐行传输，不引入双写或外部副作用。固定字段清单有 schema 覆盖回归，避免后续新增持久字段时漏迁移。
- 两种目标均逐字段验证源与目标一致，包括 JSON 中没有的 Fabric ID 和 binding revision；注入最后一张表的插入失败，确认之前的所有记录完整回滚。目标重启后数据保持一致，重复复制拒绝非空库。不存在/未知源库、源仍运行、源目标相同均明确失败。
- SQLite 完成离线备份、再复制恢复和原密码/会话验证。真实 PostgreSQL 17 使用 `pg_dump` 归档测试 schema，删除该测试 schema，再以 `pg_restore --single-transaction --exit-on-error --no-owner --no-privileges` 恢复，逐字段验证并确认原 Cookie 和机器凭据有效。未引入自制 PostgreSQL 备份格式。
- 真实 CLI 演练从旧 JSON 经 SQLite 再切换 PostgreSQL，整个过程保持同一 fabricd、机器配置及部署地址。切换后原会话/密码、机器绑定、Runtime ID、incarnation/generation 和 PTY 历史保留，原终端执行并显示 `AFTER_POSTGRES_TRANSFER_OK`；测试 Runtime、连接进程、站点及专用测试 schema 已清理。
- 验证：启用真实 PostgreSQL 及其备份工具的全量 `make test` 通过；SQL/迁移 `make test-race` 和 `make check-go` 通过。材料明确限定当前 schema 1、SQLite 与 PostgreSQL 17 的已验收组合，不承诺任意未来版本混用或降级。

S0c 本地验收完成。下一检查点为 S1：企业身份适配、浏览器及人类 CLI 登录、短期访问凭据、稳定 Runner/绑定和全部操作的访问检查。真实身份源及后续 Managed/集群仍需对应实现和实际环境证据。

## S1：身份与 Attached（实施中）

### 已完成：Dune 用户停用与会话授权版本

- schema 2 持久化 principal 启用状态和单调授权版本，会话记录签发时版本。停用原子更新 principal、删除全部会话与待消费 enrollment；与并发登录/绑定共用 principal 行锁。重新启用只允许新登录，旧 Cookie、残留旧版本会话和安装命令都不能恢复。现有 MachineIdentity 与任务不随用户停用销毁。
- 公开宿主 `App.SetPrincipalEnabled` 提供可信管理入口，调用方须完成管理员授权；没有新增普通用户 HTTP 管理入口。操作响应 context，应用关闭会取消并等待已受理管理工作。上游身份停用同步、企业 IdentityProvider/AccessChecker 和人类 CLI 登录尚未实现，不将此能力视为完整企业身份验收。
- schema 1 → 2 通过事务和迁移锁升级，已有账号/会话默认保持有效。SQLite/PostgreSQL 均验证中途 ALTER 失败完整回滚、并发登录不能越过停用、其他用户不受影响。转库清单同步覆盖新增字段，按 SQLite 声明的 BOOLEAN 类型转换 PostgreSQL 参数，保留停用状态及版本。
- 真实 PTY 测试中，SQLite 和 PostgreSQL 停用后分别约 0.98 秒关闭空闲用户终端；重新启用后的旧 Cookie 仍拒绝，新登录可连接原 Runtime 并读取原历史。测量来自本机单机器测试，不作为任意部署规模下的延迟承诺。旧 schema 1 CLI 工作台到新二进制的升级演练也保留原 Cookie/密码、机器配置、Runtime ID、incarnation/generation 和 PTY 历史。
- 验证：启用真实 PostgreSQL 17 和备份工具的全量 `make test`、metadata/host/Web/migrate `make test-race`、`make check-go` 通过。升级演练的测试 Runtime、fabricd 和工作台已清理；schema 2 的 JSON 导入、SQL 转库与原生备份恢复随回归重验通过。
