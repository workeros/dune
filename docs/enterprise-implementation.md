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

### 已完成：外部身份与 OIDC 浏览器登录

- 公开 `pkg/identity.Provider` 只验证上游身份，宿主通过 `Options.Identity` 显式装配。内置 OIDC 适配器使用固定版本 go-oidc 和 oauth2，执行 Authorization Code + PKCE、签名、issuer/audience、nonce、有效期、azp 和可选 at_hash 校验，支持 JWKS 密钥轮换。令牌交换选择确定的客户端认证方式并禁止 HTTP 重定向，不因模糊失败自动重放授权码。
- schema 3 持久化 namespace + subject 到 Dune principal 的关联、会话身份源及十分钟登录事务。state 和浏览器 proof 只保存哈希，nonce/verifier 用于一次交换；回调绑定身份源、浏览器 proof 和精确部署地址，原子消费后才调用外部服务。并发首次登录只创建一个身份，账号与会话同事务提交；同邮箱不合并，邮箱变化不改变稳定用户 ID。
- 官方 `web --identity-config` 从私有文件构造同一适配器。企业模式关闭本地密码入口并拒绝本地/其他身份源 Cookie；工作台显示有限的企业登录按钮。Dune Cookie 为 HttpOnly、SameSite Strict 和部署路径，跨站回调 proof 为 SameSite Lax 和回调路径，HTTPS 下均为 Secure。退出仅撤销 Dune 会话；上游令牌不持久化。
- SQLite 验证关闭并重开应用后完成待处理回调；真实 PostgreSQL 验证独立连接池之间单次消费和并发身份注册。实际 Dex 2.45.1 浏览器演练将 start 固定交给宿主 A、callback 固定交给独立宿主 B，在 `/tools/dune/` 完成认证和回调；停用后旧 Cookie 返回 401，Dex 再次认证也无法越过 Dune 停用，重新启用后必须新登录。另通过正式 CLI、SQLite、私有配置在 `/enterprise/` 完成登录和退出。
- 验证：真实 PostgreSQL 17 和原生备份工具启用的全量 `make test`、metadata/Web/host/identity/migrate race、`make check-go`、`make web-check web-build` 通过。签名协议测试覆盖拒绝非法 token、轮换密钥和模糊交换不重放；转库清单与备份恢复覆盖 schema 3 新字段。Dex 是本机独立真实身份服务，此证据不代表某个企业 IdP 的部署已验收，也不代表跨节点执行路由已经实现。
- 上游停用尚无自动通知/轮询同步，默认八小时 Dune 会话（可设一分钟至二十四小时）提供到期边界；可信宿主可更早调用用户停用入口。明确审计的身份关联和短期跨实例连接凭据见后续小节；人类 CLI 登录、完整操作级 AccessChecker、Runner/绑定公开边界仍待实现，S1 尚未整体完成。

### 已完成：显式身份关联与持久决定记录

- 可信宿主 `App.LinkIdentity` 接收明确管理员、请求 ID、既有 principal、稳定 namespace/subject 和核验依据。宿主负责认证管理员、授权并核验两个身份，不提供普通用户可直接调用的管理 HTTP 入口。已有其他归属拒绝关联，不合并账号或按邮箱认领；已有正确归属可提交新的明确决定。
- schema 4 将关联和成功决定记录、授权版本递增、会话/enrollment 撤销原子提交，审计写入失败时整体回滚。相同请求及字段只执行一次，冲突请求不改变数据；`App.IdentityLink` 支持提交回执丢失后的核对。该记录只覆盖成功关联，宿主另行审计拒绝和授权过程。
- 首次外部登录与管理员关联使用同一身份锁，SQLite/PostgreSQL 并发回归验证不会串入其他账号，关闭重开后决定保留。SQL 转库字段清单覆盖审计记录，备份恢复包含相同数据。公开宿主回归从本地注册、显式关联、关闭重开到外部浏览器回调，确认稳定 principal ID、Cookie 撤销和新会话重试不受影响。
- 真实 SQLite/PostgreSQL 工作台 PTY 回归中，关联撤销空闲用户连接，重新登录可读取原 Runtime ID、incarnation/generation 和终端历史，现有 fabricd 和机器配置保持有效。这是可信测试身份适配器与真实执行进程的组合验证，不声称新增了某企业 IdP 的部署验收。
- 验证：启用真实 PostgreSQL 17 和备份工具的全量 `make test`、metadata/host race 和 `make check-go`；身份关联及其持久化/撤销/并发/回执丢失回归通过。迁移步骤与审计边界见[元数据迁移](metadata-migration.md#schema-4显式关联既有账号)。下一检查点继续人类 CLI 登录和访问授权，S1 未整体完成。

### 已完成：跨实例短期连接凭据

- 访问模块不再保存进程内 ticket/闭包映射。schema 5 保存单次凭据哈希与原会话引用，捕获用户授权版本、身份源、机器、Runner、Fabric 和绑定修订；原用户凭据不会被复制到新实例。凭据有明确类型前缀，消费失败不回退到机器认证，同一身份配置下才允许消费。
- 原子消费支持 SQLite 重开和 PostgreSQL 独立连接池，并发竞争只有一个成功；30 秒后不能建立新连接，同一会话最多 64 个待消费凭据。连接建立后按固定引用继续复核有效会话与当前绑定，释放 ticket 不撤销已建立访问，退出/停用/关联/解绑会撤销，绑定变化不自动重定向。仍采用默认 owner 策略，完整企业操作级检查尚未完成。
- 两个独立 `host.App` 共享真实 PostgreSQL，Web 在 A 签发凭据并经网络拨号到 B；fabricd 仅连接 B。真实 PTY 创建、输入输出、停用和身份关联后的空闲撤销、重新登录连接原 Runtime 及历史均通过。测试明确向实际 owner 查询就绪状态，不将本地连接目录当作跨实例目录。
- 验证：启用 PostgreSQL 17 原生备份工具的全量 `make test`、metadata/Web/host race 和 `make check-go`；签发/消费回执丢失不重放、凭据类型与身份源隔离、会话/机器级联清理、绑定变更拒绝及 SQL 转库/备份恢复回归通过。此能力尚无公共人类 CLI 登录入口，也不构成 S3 自动路由、peer 或接管；下一步补 CLI 浏览器确认及用户会话交换。

### 已完成：人类 CLI 浏览器确认与执行

- `dune login --site` 使用站点现有本地账号或企业 SSO，浏览器明确展示账号、站点和终端核对码，由用户确认或拒绝。客户端 proof 不进入浏览器 URL；跨 OIDC 回调保留待确认请求。schema 6 持久化十分钟请求，确认身份不可替换，原子单次消费与 CLI 子会话创建支持 SQLite 重开及 PostgreSQL 独立连接池。
- CLI 凭据独立于机器配置，以 0600 文件保存在私有目录；同文件登录/退出持锁且不覆盖已有凭据。会话最长八小时并受父浏览器会话期限约束，退出浏览器、停用或身份关联会撤销相关 CLI 访问。CLI 自身退出只撤销该子会话，已被浏览器撤销时仍可清理本地文件。
- 公开 `pkg/login` 完成登录请求、等待确认、机器查询、短期目标凭据交换及默认 SDK 拨号；`--login FILE --target ID` 沿用原执行命令。身份逻辑位于执行协议之外，用户令牌不交给 fabricd。HTTPS 校验及精确站点/目标绑定必需，只有数字 loopback 允许明文；HTTP 模糊失败和重定向不自动重放。
- SQLite 和真实 PostgreSQL 回归均用正式 CLI 二进制、fabricd 和执行链路完成登录与命令，浏览器退出后 API 和已建立 SDK 连接撤销；PostgreSQL 同时验证独立宿主签发与消费。实际 Dex 2.45.1 经宿主 A start、B callback 返回前缀确认页，终端核对成功后执行并显示 `CLI_DEX_EXEC_OK`，浏览器退出后 CLI 被拒绝，CLI 退出清理私有文件。专用进程和测试 schema 随后清理。
- 验证：启用真实 PostgreSQL 17 和原生备份工具的全量 `make test`、metadata/Web/host/login/config race、`make check-go`、`make web-check web-build` 通过。覆盖消费回执丢失、并发单次消费、事务回滚、凭据类型隔离、父会话与授权版本、私有文件保护及 schema 6 转库/备份恢复；前端仍有既有主 bundle 体积提示。最后的确认版本检查和 CLI 参数校验另经定向回归重验。

下一检查点为完整操作级 AccessChecker、发现与稳定 Runner/绑定边界。当前仍是默认 owner 访问，S1 尚未整体完成；Managed 和 S3 集群继续按方案实施，Dex 本地演练不代表企业 IdP 或集群部署验收。

### 已完成：公开 Runner 查询与固定绑定访问

- `pkg/runner` 提供逻辑 Runner 与绑定快照，包含 Runner、Fabric、机器和单调修订；不进入协议 core。浏览器及人类 CLI 可查询自己的 Runner，`pkg/login` 和正式 CLI `runners` / `--runner` 使用同一模型。新 Attached 注册在原事务内分别生成 Runner ID 与机器 ID，既有 SQL 和 JSON 导入的原 ID 保持不变，机器配置与 Runtime 无需重建。
- `DialRunner` 接收调用方选定的完整快照，服务端在访问凭据签发事务内复核归属及绑定，PostgreSQL 行锁覆盖修订。快照不匹配返回拒绝，SDK 不重新解析替代环境或自动重放；已建立访问持续检查原绑定。原 `--target` 机器入口保持可用，与 `--runner` 互斥。
- SQLite/PostgreSQL 验证发现和查询的 owner 隔离、Runner 与机器 ID 分离、错误 Fabric/修订拒绝、同一 Runner 更换机器后旧授权失效、关闭重开及显式选择新快照。替换环境由测试直接构造数据库事实，没有新增可绕过生命周期的绑定编辑入口，也不代表 Managed 已实现。旧 JSON 的数据形状独立固定，新增 Runner 字段不会放宽导入校验。
- 验证：启用真实 PostgreSQL 17 和原生备份工具的全量 `make test`、metadata/Web/host/login race 和 `make check-go` 通过；正式 CLI 与独立 Gateway 经真实 fabricd 执行并返回 `CLI_RUNNER_EXEC_OK`，父浏览器退出撤销相应 SDK 连接。最后的旧 JSON 字段拒绝及保留原 Runner/机器身份另经定向迁移回归验证。

下一步将企业 AccessChecker 接入完整操作映射及有界发现，并把工作台产品入口统一到 Runner。当前公开 Runner 发现仍采用默认 owner 策略；Managed 绑定变更、生命周期及 S3 集群仍未交付，S1 未整体完成。

### 已完成：可复用的执行流 AccessChecker

- `pkg/access.Grant.Policy` 在既有 core 钩子上装配一个检查器，复制可信用户、owner 与 Runner 绑定；请求体及后续消息不能替换关联。检查器接收实际 operation、子操作、Runtime 身份和有界选择属性，不取得命令、终端内容、文件/Git 正文、环境变量、prompt 或凭据。未知操作拒绝，企业检查没有 owner 回退。
- 首条请求先检查再转发；持续 input/resize/signal、端口 data/eof 分别取得该流的短期决定。只读订阅独立禁止所有输入类型，原始 ACP 输入整体映射为 `acp.raw/exchange`，托管 ACP 动作按实际 action 区分。上传提交单独检查，非 create 请求中伪填的路径不会被当作有效资源属性。详细契约与映射见[执行流访问检查](access-checks.md)。
- 检查最多一秒，决定最多三十秒，外部到期时间转换为本机单调期限。后台提前复核并独立执行截止时间，空闲流及阻塞的检查器不能延长已过期授权，迟到 allow 不恢复旧流。会话/机器/绑定有效性仍由独立 Grant.Valid 约束；访问撤销不回滚已受理操作或停止远端任务。
- 独立 Gateway、真实临时 fabricd/PTY 验证拒绝写入、上传提交和 Git 暂存不产生副作用，拒绝端口输入时字节未到目标服务，只读 attach 不能输入、resize 或 signal，重复终端输入复用决定，空闲复核失败及迟到检查器关闭流。全量本地 `make test`、access/Gateway/host/Web race 和 `make check-go` 通过；最后补充的 Git/端口及资源属性语义另经 access race 重验。本次不涉及 SQL 变更，未重新启用外部 PostgreSQL 或真实 Agent 验收。

本检查点交付可独立装配的执行流处理器，尚未向 `host.Options` 暴露覆盖不完整的企业开关。下一步将同一检查器接入 Web/CLI 发现和管理入口、权威目标解析及默认宿主装配，再完成 S1 的端到端企业授权验收。工作台 Runner 入口、Managed 与集群继续推进。

### 已完成：宿主统一访问检查与有界发现

- `host.Options.AccessChecker` 在默认工作台选择唯一检查器，覆盖 Web/CLI 的 Runner/机器发现、Attached 注册材料签发与消费、解绑、连接交换和全部执行流。留空使用默认 owner 策略，企业实现可明确允许跨 owner 访问；拒绝、故障或超时不回退。身份与固定绑定仍独立约束执行，机器连接不取得人类权限，core 无新增产品依赖。
- 元数据负责提供权威 owner/Runner/Fabric/机器/修订，检查通过后的写入仍在事务内复核原会话和绑定。schema 7 将签发时 owner 保存到一次性凭据，旧访问不能跟随归属变化；停用后再启用不恢复旧请求的写入资格。Attached 解绑只撤销绑定和机器身份，不删除远端主机或任务。
- 列表改为 `items` 与可选 `next_cursor`，默认 32、最多 100 项，单页最多扫描 128 个候选且总期限五秒。个人模式先按 owner 查询，复合索引支持稳定位置分页；企业模式逐候选检查，不暴露拒绝项 ID 或全局总数。SQL 游标绑定用户、身份源和用途，十分钟过期、每用户最多 64 个位置，刷新复用已有位置。公开 Go 客户端和 CLI 同步分页，不自动遍历全库。
- 工作台支持前后翻页、空页继续、过期后回到第一页和策略故障重试。侧栏列表单独滚动，翻页及接入按钮保持可见；真实浏览器用 130 条通过正式 enrollment 建立的离线验收记录检查上述状态，在 1440、820 和 390 像素宽度及 200% 文本缩放下验证布局。列表测试记录不代表真实开发机在线或 Managed 提供方就绪。
- SQLite 和两个共享 PostgreSQL 17 的独立宿主通过真实 Web PTY 与正式 CLI 执行另一个账号的机器；测试检查器拒绝 Web Agent 配置保存和 CLI 文件写入且无副作用，策略撤销关闭空闲终端，用户停用、身份关联和父会话退出仍撤销连接。单元回归覆盖候选扫描上限、游标跨池/重开/隔离/配额、明确拒绝与故障无 owner 回退、固定归属及绑定和会话的写入竞争。转库、原生 PostgreSQL 备份恢复包含真实游标行与 owner 字段。
- 验证：启用 PostgreSQL 17 和原生备份工具的全量 `make test`、metadata/Web/host/access/login race、`make check-go`、`make web-check web-build` 及实际浏览器交互通过。最后的分页索引、配额和 Web 写入拒绝另经对应 SQL race 与企业进程回归验证；前端保留既有主 bundle 体积提示。升级须同步部署分页客户端和后端，按 schema 7 停站备份，不支持混合旧版本运行。

下一检查点将工作台的产品入口统一到逻辑 Runner 和固定绑定，再核对 S1 验收矩阵。企业检查器在此使用明确允许/拒绝的测试适配器，不声称验证某个企业私有权限 SDK；Managed、续期及 S3 自动路由/接管尚未交付，完整目标继续推进。

### 已完成：工作台 Runner 入口与明确绑定选择

- 工作台按逻辑 Runner 发现和选择环境，浏览器列表/详情展示当前绑定的在线、系统和架构事实。点击环境保存完整绑定，call、创建会话、事件订阅及 Attached 解绑统一使用 Runner 路径和 `machine_id/fabric_id/revision` 选择约束；前端不再通过机器路径执行。原具体机器 API 保留，公开 CLI/SDK 绑定契约不变，无需新增 schema 或重装既有 fabricd。
- 后端把选择值与权威元数据、当前用户和企业决定核对，再按固定快照签发访问或提交解绑；缺失/歧义参数、错误 Runner/Fabric/修订和已变化绑定均不会拨号新目标。解绑在事务内再次比较同一绑定，其他执行与持续流继续通过已有 Gateway 检查链，协议 core 未新增产品依赖。
- 列表刷新只更新发现事实，不覆盖选中的绑定；变化时卸载旧工作区与订阅，明确进入当前环境后才使用新快照。环境不可访问或未绑定时不提供执行工作区，进入环境仍需点选已有 Runtime。后台刷新保持变化提示及操作按钮稳定，不用加载态反复替换用户正操作的提示。
- 真实前缀浏览器创建终端并显示 `RUNNER_BROWSER_OK`，模拟 SQL 已提交绑定修订后观察到旧工作区关闭和明确重新进入提示；选择当前环境及原会话后保留原历史并显示 `RUNNER_REENTER_OK`。浏览器实际解绑后单独检查原 tmux pane 仍存活，输出 `RUNNER_UNBIND_PRESERVES_PTY_OK`，随后停止专用进程和清理测试会话。修订变更只模拟生命周期模块提交事实，不代表 Managed 创建或重绑能力已实现。
- 验证：启用 PostgreSQL 17 与原生备份工具的全量 `make test`、Web/host/access race、`make build`、`make check-go`、`make web-check web-build` 通过。`TestBrowserRunnerBindingIsFixed` 覆盖四个入口的固定绑定及无拨号拒绝；真实 PTY 前缀回归新增 Runner 入口，SQLite/PostgreSQL 企业共享回归均改由同一 Runner 路径执行并验证写入拒绝与撤销。前端仍有既有主 bundle 体积提示。

下一检查点逐项核对 S1 的身份、访问和执行身份契约，再继续 S2 Managed、持久操作及续期。S1 尚未据此标记整体完成；S2/S3 和真实企业集成的部署证据仍须按原方案取得。

### 已完成：浏览器订阅固定 Runtime 执行身份

- Runner 及原机器事件入口均要求完整 Runtime ID、incarnation、generation，以该身份执行有界 `runtime.get` 后再 attach；不再按 ID 查询并填充最新身份。缺失、重复或非法选择值在拨号前拒绝，实际执行引擎拒绝旧 incarnation/generation。协议核心及 SQL schema 无变更。
- 工作台保存选中的 Runtime 快照，PTY 和 ACP 订阅及重连均携带原身份；列表刷新可以更新状态，不能替换所选执行身份。身份变化或不可用时卸载旧视图，用户明确重新点选后才连接，不重放输入或创建请求。旧事件客户端须与静态资源、后端同步升级。
- 真实浏览器创建 PTY 并输出 `RUNTIME_FIXED_OK`；在专用 fabricd 停止期间用测试夹具改变原 Runtime 的持久 incarnation，再启动同一 fabricd，浏览器自动重连仍使用旧身份并关闭旧视图。记录的 WebSocket URL 确认重新点选前未使用新身份；点选后原历史保留并输出 `RUNTIME_RESELECT_OK`。夹具模拟同 ID 的执行身份变更，不是生产重建入口或 Managed 验收。
- 验证：定向 Web/前缀进程回归、Web/host/access race、Go 静态检查、Go/Web 构建通过。启用真实 PostgreSQL 17 和备份工具的全量测试中，tmux 环境隔离和 access 执行测试各出现一次 tmux 操作确认超时，其余包（含完整跨进程测试）通过；两处失败包随后以 `-p=1 -count=1` 串行复验通过。前端仍有既有 bundle 体积提示。

S1 审查还发现企业检查器当前只能取得 Dune principal 与 namespace，无法取得这次登录已验证的外部 subject；下一检查点补齐稳定身份归属，确保关联多个外部身份时不猜测身份、不传递上游令牌。S2/S3 和真实企业集成证据继续按原方案推进。

### 已完成：企业访问检查保留本次验证的 subject

- `access.Scope` 新增本次登录验证的 `Subject`，与 `Namespace` 共同标识外部身份；Dune principal 仍为稳定产品用户，本地登录的两个外部字段为空。身份引用不含原始声明、邮箱或上游令牌，不暴露到浏览器用户 JSON。企业适配器可以同时实现身份验证和权限检查，不需要反查或猜测同一 principal 的多个关联身份。
- schema 8 随浏览器会话保存 subject，CLI 复制确认的父会话引用，短期连接凭据在事务中捕获同一引用；父子会话、原访问凭据及持续流不能更换 subject。发现、管理和流检查均取得这一身份。Attached 材料保存实际签发者的 namespace/subject，消费时使用原引用并拒绝跨身份源；仍保持独立十分钟单次能力的原有退出边界。
- 旧企业会话和旧安装材料缺少可靠的签发身份，升级分别要求重新登录和重新签发，不按映射猜测。原本地访问、外部关联、机器凭据、Runner 绑定及远端 Runtime 保留。迁移、转库、备份与回退边界见[元数据迁移](metadata-migration.md#schema-8本次登录的外部主体)。
- SQLite 重开和真实 PostgreSQL 跨池回归验证同一 principal/namespace 关联两个 subject 时的明确允许/拒绝隔离，CLI/凭据/安装材料持久身份、错误父子引用及写入身份拒绝。真实宿主浏览器回调将已验证 subject 交给同一提供方实现的 Checker；执行流回归验证 Bind 后修改原对象不能替换身份。schema 7 升级及失败回滚、SQLite 转库和 PostgreSQL 17 原生备份恢复均覆盖新增字段。
- 验证：启用 PostgreSQL 17 与备份工具的全量 `make test TEST_FLAGS='-p=1 -count=1 -timeout=300s'`、metadata/Web/host/access race、`make build check-go` 通过。最后补充的写入身份隔离及宿主回调定向回归通过。本次没有前端改动，身份适配器为可信测试实现，没有重复声称企业 IdP 或私有权限 SDK 已取得新的部署证据。

下一检查点继续核对 S1 验收矩阵，并推进真实提供方驱动的 S2 Managed 闭环。完整目标中的持久生命周期、续期及 S3 集群仍未交付。

## S2：Managed 生命周期（实施中）

### 已完成组件：个人默认续期决定

- `internal/lifecycle` 提供无 I/O 的默认续期计算，默认延长一小时、提前十分钟、首次连接宽限五分钟；显式零宽限保留，非法时长或无法形成有效提前量的配置拒绝。该内部组件尚未形成提供方公共接口，官方应用仍未启用 Managed。
- 决定使用首次确认资源的原始时间及曾经可用事实，查询、浏览器关闭和 worker 重启不重置宽限期；明确引导失败、销毁、提供方确认已消失或策略关闭时停止维护。未知目标不猜测，未完成的外部变更不能因 worker 租约到期而让续期进入。已可用后离线只要求重新核对事实，不重新套用首次连接宽限。
- 返回的是绝对续期目标或下一检查时间，不表示调用成功。已知未到续期时间的资源直接安排到阈值或宽限期末；未知事实及未完成调用安排有界复查。过去的到期时间本身不能证明已删除或授权盲目续期。后续执行器须持久化原目标与动作关联，处理提供方限制、领取、抖动、退避和实际结果，不能重新计算目标后重放原动作。
- 定向生命周期测试及 `make check-go` 通过，覆盖宽限期与到期边界、初次/再次连接区别、未知事实、业务互斥、配置校验和调度时间。这些仅证明策略决定，不证明真实提供方、持久调度或多实例领取已实现。

本机配置、环境变量和已安装能力中尚未发现可用 Managed 提供方，已向用户请求首个实际平台、Go SDK 和测试配置路径。等待具体集成信息期间仍可推进确定的内部机制；不得以 mock 或本机进程模拟替代方案要求的真实提供方闭环。S2/S3 保持未交付。

### 已完成组件：持久操作意图、业务互斥与执行租约

- schema 9 在同一事务内保存现有 Runner 的受控操作意图与业务互斥，请求键绑定原发起者、身份源/subject、摘要及资源选择。相同键返回原 Operation，字段不同拒绝；提交回执丢失返回结果未知，不自动重放。内部存储不承担用户授权，也不接受提供方响应正文、工作内容或凭据。
- Operation 完成状态、业务互斥和 worker 租约分别保存。未完成的创建工作可以在外部变更确认结束后释放互斥，观察首次连接，同时让续期串行进入；另一 Operation 的完成不会释放未决续期的互斥。unknown、timed_out 保留互斥，当前接口不提供无条件解除未知调用的入口。
- worker 用进程 incarnation 和单调执行修订领取，只获得跟进原操作的权利。领取、刷新及结果条件写入使用数据库时间；更新复核原意图、Runner/Fabric/绑定修订、worker 和租约，暂停后过期的写入与旧 worker 均拒绝。数据库机制不隔离迟到的外部调用，接管者仍须先查询事实，并依赖提供方真实去重或隔离能力才能继续变更。
- SQLite 与真实 PostgreSQL 验证并发请求去重、单次领取、跨池/重开接管、租约到期不释放业务互斥、等待连接时的维护、旧绑定及修订拒绝、暂停写入、字段界限、修订溢出和事务失败回滚。SQL 转库及 PostgreSQL 17 原生备份恢复包含实际 unknown 操作和原租约，意图/领取/完成的回执丢失均可按原 ID 核对。
- 这是内部协调组件；新 Runner 创建与权限复核、销毁访问限制、阶段结果与资源引用仍须由后续生命周期专用事务一起提交。尚无公共 Operation API、提供方动作键、恢复 worker 或真实 Managed 调用，不将该组件视为 S2 闭环或 S3 执行路由完成。
- 验证：启用 PostgreSQL 17 的 metadata 全包回归、操作/回执丢失/转库/原生恢复定向 race、host/Web/migrate 回归、前缀工作台及跨宿主真实 PTY 定向回归、`make build` 和 `make check-go` 通过。最后拆分业务互斥与完成状态后重验了相关 SQL/race；本次未运行不相关的完整 Agent 任务或宣称真实提供方已验收。

## S3：集群连接归属（实施中）

### 已完成：执行端逐消息校验原连接

- 修复 fabricd 原先只在首请求检查 connection generation 的缺口。所有业务处理器使用同一个连接关联读取入口，在 protobuf 解码后检查反向连接 context、引擎状态和原 generation，随后才允许首请求或后续 PTY、原始 ACP、端口消息进入操作处理。后续消息仍隐式属于原 stream，不新增用户或产品身份字段，也不修改线协议。
- 第二条真实反向连接推进同一引擎的 generation，保持旧 Gateway 及其流存活的回归中，旧终端未创建测试文件，原始 ACP 输入拒绝，端口字节未到目标；现有 PTY/ACP Runtime 保持运行，新连接执行并返回 `CURRENT_CONNECTION_OK`。另一回归先将输入写入 Yamux 缓冲，再取消连接 context，确认消息不被交给处理器。
- 此处拒绝的是失效连接上的新消息准入，不回滚已受理操作。目录 epoch、恢复代次、接收方有界输入租约、owner 发布与 peer 转发尚未实现，不将 connection generation 等同于集群 epoch。下一步仍须实现这些约束并在三节点、进程暂停、缓冲输入和分区下验证。
- 验证：全量本地 `make test TEST_FLAGS='-p=1 -count=1 -timeout=300s'`、fabricd/access/gateway/wire 竞态回归、`make build check-go` 通过。新增回归使用两个真实协议 Gateway、PTY、原始 ACP 和 TCP 连接；本次未启用 PostgreSQL、真实 Agent 或远端验收。

### 已完成：有界输入租约与确认后发布路由

- 协议升级至 `dune-mvp/2`。fabricd 发起随机 challenge，双方保存本地单调起始时间，最多十五秒输入期限、五秒续租；初始确认后 Gateway 才发布路由，错误或缺失确认不能替换已有连接。旧版本明确拒绝，不隐式允许无租约执行。升级、回退及 PTY/ACP 生命周期边界见[协议说明](implementation.md#网络和协议)。
- Gateway 为每条转发消息设置当前已确认 grant，覆盖客户端输入的协议字段；fabricd 在解码后检查消息原 grant，传输延迟、阻塞发送或下一次续租均不能延长旧消息期限。允许有限的未过期 grant 重叠，已过期的连接不能靠迟到控制消息复活；空闲隧道也受期限约束。续租不改变 Runtime 身份或业务去重，原始 ACP、PTY 和端口统一经过同一执行读取入口。
- 真实独立 Gateway/fabricd 进程的 SIGSTOP 回归分别暂停两端超过十五秒，确认恢复后旧终端命令未创建文件，连接 generation 前进、原 PTY 保留且重新订阅可输入。协议回归检查连续续租超过首租约、续租后的原请求去重、缓冲消息过期、未确认路由保留、伪造标识覆盖及控制状态容量；时间边界另由可控本地时间测试。
- 这是反向连接的输入期限，不是 PostgreSQL owner 租约。目录 Acquire/Publish、恢复代次、epoch 与 peer 转发仍未实现；后续集群发放的输入期限还必须受 owner 保守期限限制。已受理操作不回滚，也不将进程暂停演练称为三节点分区或主机时钟修改验收。S3 保持实施中。
- 验证：全量本地 `make test TEST_FLAGS='-p=1 -count=1 -timeout=300s'`、wire/fabricd/gateway/access/tunnel 竞态检查、`make build check` 和 `make proto` 通过。最后补充的旧协议拒绝、续租去重和取消缓冲消息回归已通过相关 race。未启用 PostgreSQL、真实 Agent 或远端测试；本功能未改动数据库与前端。

### 已完成组件：PostgreSQL 连接目录与离线恢复代次

- `gateway.Directory` 定义 Acquire、确认后 Publish、Resolve、Renew、Release 的协议层契约，独立于账户与 SQL。第二层使用当前元数据后端实现，拒绝 SQLite；schema 10 保存机器归属、完整执行绑定、owner 启动身份、定向地址、单调 epoch 与发布状态，普通单机迁移不启用集群。
- 同一机器的首次插入和后续变更串行化；有效 owner 不能被另一个连接或同一启动身份抢占。释放保留 epoch，更新比较恢复代次、owner、epoch、地址及绑定。期限以数据库时间为准，续约/发布在实际 UPDATE 复核未过期；返回剩余时长供核心按调用起点折算保守本地期限，不能按响应收到时间增加执行权。
- 所有目录事务核对恢复代次；初次选择可创建指定代次，已有代次不在启动时替换。离线 `metadata cluster-recovery` 支持读取及按原代次条件旋转，生成新随机身份；结果未知会输出候选值供只读核对，不自动再旋转。旧目录对象在旋转后失效，恢复后的历史 route 不可发布或续约。管理员仍须先停止旧实例，再配置新代次并重启。
- 真实 PostgreSQL 跨池回归覆盖并发唯一 owner、两阶段发布、完整绑定、释放后的 epoch、旧进程迟到清理与续约、代次恢复及溢出；锁等待跨越期限不能复活租约。提交回执丢失覆盖 Acquire/Publish/Renew/Release/恢复旋转，均只提交一次并可独立读取核对；原生备份包含实际归属，恢复后旋转隔离历史路由。正式 CLI 验证读取、旋转及旧条件重试拒绝。
- 此检查点尚未把目录装配到 Gateway/host：下一步须在握手时领取 owner，确认 epoch 后发布，将现有输入期限限制在 owner 期限内，并补齐 peer、恢复身份接入与三节点验证。目录接口或 SQL 测试本身不证明集群执行已经交付。
- 验证：启用 PostgreSQL 17 的 metadata 全包及 migrate/host/Web 回归、目录/恢复/转库/原生备份定向 race、正式恢复 CLI 与 PostgreSQL 工作台及跨宿主 PTY 回归、`make build check-go` 通过。最后的现有 schema 只读打开与类型拆分已复验；未执行完整真实 Agent 任务、三节点故障注入或对现有部署进行恢复操作。

### 已完成：Gateway 归属领取、epoch 确认与输入期限衔接

- 协议 Gateway 新增显式目录构造器，每实例使用新启动身份和定向地址；校验 hello 后按目录 epoch 领取归属，有效 owner 不能抢占。fabricd 确认恢复代次、epoch 及原输入 challenge 后，Gateway 才发布路由。协议层不增加 SQL、用户或 Runner 依赖。
- 目录调用限时一秒，owner 的本地有效期从调用起点扣除已耗时间；每次五秒控制更新先刷新原归属，再把输入 grant 限制在 owner 的保守期限内。暂停后的迟到响应、过期或结果未知不复活本地权限。空闲连接、新 stream 和转发均受期限约束，本地处理流同样绑定原连接，并在处理新消息前复核归属；结束时先关隧道再按原归属清理；不盲目重试目录变更。
- v2 协议新增可选的 `route_recovery` / `route_epoch`，目录模式要求完整确认与消息关联。SDK 固定首请求归属，Gateway 后续转发覆盖客户端提供的归属字段，fabricd 对有效输入 grant 也执行原归属校验。单机模式仍为空/零；旧 v2 端不能静默降级接入目录模式。SQL 续约保留原有的更晚期限，已有 grant 的承诺不会因一次更新缩短。
- 真实 PostgreSQL 与两个协议 Gateway 的回归中，竞争引擎无法打断有效 owner；超过首个十五秒后原连接仍正常执行。释放后原引擎连接另一个 Gateway，epoch 和连接 generation 分别前进，旧 owner 清理被拒绝，原 PTY 重接输出 `OWNER_TRANSFER_OK`。测试旋转代次后，运行中的旧连接在下一次目录复核关闭；时间边界与错误 epoch 确认另有定向回归。
- 当前新增的是协议核心的目录装配。官方 host/CLI 集群配置、peer 用户访问上下文、跨实例转发、排空及三节点故障验收仍未交付；不将这次真实 SQL/协议集成等同于完整 S3 或 S1/S2 集群闭环。
- 验证：启用 PostgreSQL 17 和备份工具的全量 `make test TEST_FLAGS='-p=1 -count=1 -timeout=300s'`、相关 gateway/fabricd/access/wire race、`make proto` 与 `make build check` 通过。最后补齐本地处理流的生命周期后，重新执行完整跨进程测试和受影响包 race，均通过。测试在专用 SQL schema 和本地真实 PTY 上进行，未修改既有部署或启用真实 Agent 账号。

### 已完成组件：协议 peer 单跳转发

- `gateway.NewWithPeers` 在原目录与输入租约上增加固定 owner 拨号。SDK 连接只解析并建立一次 peer，后续请求使用原绑定；失效、网络中断及结果未知均不重新选路或重放。拨号由应用提供已认证、保密的 `net.Conn`，核心继续不依赖 HTTP、SQL 或产品用户。
- 独立 `peer` 角色核对可信入口启动身份、目标启动身份、原执行绑定、恢复代次及 epoch。共享、SDK 和机器角色不能充当 peer；目标实例只允许访问自己当前持有的本地连接，缺失时返回 `ROUTE_STALE`，不转发第三跳。原 owner 关闭会终止空闲 peer 和上游 SDK 连接。
- 远端流要求访问模块显式调用 `ForwardPeer`，普通转发不隐式授予权限。每个首请求携带最多 16 KiB 的应用上下文，目标实例的处理器仍独立验证；SDK 注入、上下文缺失、越界和后续替换在准入前拒绝。快照及上下文缓冲复制，应用不能更改核心原路由；上下文在 owner 截止，由 owner 为 fabricd 设置自己的执行输入 grant。
- 协议回归覆盖入口和目标分别拒绝、错误身份/归属、禁止第三跳、固定快照、上下文界限、迟于握手的本地归属变化和断线不重拨。真实 PostgreSQL 回归使用三个协议 Gateway、两个 SQL 池和真实 fabricd/tmux：客户端固定入口，经 owner A 续租超过首个期限；fabricd 顺序连接 owner B 后，旧 peer 失效，显式新连接取得新 epoch 并恢复原 PTY 输出，随后恢复代次变更使旧访问关闭。
- 验证：完整本地 `make test TEST_FLAGS='-p=1 -count=1 -timeout=300s'` 通过（跨进程包 174.854 秒）；gateway/fabricd/access/wire/tunnel race、最终 Gateway 全包 race、启用 PostgreSQL 17 的归属/peer/PTY 定向 race（23.71 秒）、`make proto`、`make build check` 与差异检查通过。专用 PostgreSQL 已停止并清理，未调用真实 Agent 或远端资源。
- 此处的真实协议测试使用可信 peer 身份和用户上下文夹具。生产 peer 认证、用户上下文签发验证与跨实例撤销、默认 host/CLI 集群配置、配置指纹、排空及三节点故障矩阵仍待接入；普通 HTTP tunnel 保持拒绝 peer。S3 及完整 S1/S2 集群闭环继续实施，不能将此组件表述为已经交付生产 HA。

### 已完成组件：逐请求 peer 用户上下文与目标独立撤销

- 默认授权服务可按 Gateway 启动身份装配 peer 用户访问。入口复核原会话及操作权限后，在同一 Dune SQL 后端签发一次性随机能力；传输中只携带随机值，数据库保存其哈希、原会话/subject/授权版本、双方启动身份、Runner/Fabric/绑定修订、有界操作属性和入口决定 ID。完整请求摘要绑定业务字节、请求 ID、Runtime 与执行身份、恢复代次和 epoch，不存储业务正文、原始 bearer 或上游令牌。
- schema 11 保存最多三十秒、每会话最多六十四个待消费项，期限使用数据库时间并扣除签发等待。签发复核当前会话与绑定，先锁定会话、Runner 和机器再清理/插入，遵守退出和机器撤销的外键锁顺序。消费原子匹配身份源、双方启动身份、目标与请求摘要；错配不消耗其他请求的能力，提交回执丢失不给予执行权也不自动重试。迁移失败回滚，原登录与环境不变，转库及备份覆盖新增记录。
- 目标侧单次消费后，重新从实际请求生成操作描述并执行自己的 AccessChecker；入口决定不能替代目标决定。每个流重建原用户授权范围，持续检查本次会话、CLI 父会话、授权版本和产品绑定，独立撤销检查关闭空闲流；原授权期限、只读、Runtime 及后续输入检查继续生效。能力到期只限制新的首请求，已建立流不借此延长用户会话。
- SQLite 重开与 PostgreSQL 跨池验证并发单次消费、请求/源/目标/身份源错配、subject/owner/绑定/授权版本拒绝、到期与容量、退出级联删除、回执丢失和 schema 10 升级失败回滚。真实 PostgreSQL 与三个协议 Gateway 的 PTY 回归已使用正式用户签发/消费路径：入口允许不能覆盖目标拒绝；测试故意保留入口的旧有效性回答后，从另一 SQL 池退出，目标仍独立关闭空闲终端。原 owner 迁移与 PTY 保留回归继续通过。
- 验证：启用 PostgreSQL 17 与备份工具的全量 `make test TEST_FLAGS='-p=1 -count=1 -timeout=300s'` 通过，跨进程包 220.265 秒；metadata/access/authorization race、PostgreSQL 用户 peer/PTY race、schema/凭据/转库/原生备份定向 race、构建与静态检查通过。最终外键锁顺序修改后复验相关 PostgreSQL/SQLite race 与静态检查。未调用真实 Agent、私有权限 SDK 或远端部署。
- 用户访问上下文采用现有 SQL 一次性能力模式，没有新增一套签名密钥；它仍要求独立的 peer 传输认证与保密。当前测试中的传输身份由可信 `net.Pipe` 夹具提供，生产 peer 接入、host/CLI 集群配置、配置一致性、排空和故障矩阵继续推进，默认 HTTP tunnel 尚未开放 peer。

### 已完成组件：peer 双向 TLS / WebSocket 传输

- `pkg/transport/peer` 使用标准 TLS 1.3 与独立集群 CA 验证成员身份，复用原 WebSocket、Yamux 和 protobuf。每实例的叶证书须匹配本机广告地址，同时支持 serverAuth/clientAuth；启动验证链、期限、用途和私钥匹配，复制证书与信任集合。核心只新增只读广告地址查询，仍不解析 TLS、HTTP、账户或 SQL。
- 独立 Handler 要求实际 TLS 客户端证书，并按自己的原 CA 集合复核；不接受转发头代替 TLS、用户/机器 bearer、Cookie 或 Origin。HTTP 协商严格核对版本、完整路径、Host、唯一身份头与本实例 boot；回调只能返回匹配的 peer 绑定。受认证成员声明入口启动身份，目标 core 再验证 peer hello 和原归属，用户仍通过上一检查点的独立 SQL 上下文授权。
- 拨号只访问原 owner 的 HTTPS 地址，不使用环境代理、跟随重定向或重放。错误主机/端口、通配地址、路径歧义和目录广告与 Handler 地址不一致在配置或准入时拒绝。证书到期关闭空闲连接；应用负责独立 TLS 监听器和生命周期，普通 tunnel 不开放 peer。接口与协调 CA 轮换见 [peer 传输接入](peer-transport.md)。
- 真实 HTTPS 测试覆盖未知 CA、错误主机、独立 Handler 信任、伪造 TLS 转发头、重复身份、原 owner、取消、证书到期和新旧 CA 交叠/移除。PostgreSQL / PTY 回归已经把 peer `net.Pipe` 替换为三个独立双向 TLS / WebSocket 监听器，继续验证正式用户上下文、跨 owner 显式重接、原终端保留、目标独立拒绝和空闲撤销；测试 CA 只存在于内存，不修改生产信任。
- 验证：最终固定源码启用 PostgreSQL 17 与备份工具的全量 `make test TEST_FLAGS='-p=1 -count=1 -timeout=300s'` 通过，跨进程包 219.527 秒；真实 PostgreSQL / HTTPS peer / PTY 定向 race、最终 peer/Gateway race、`make build check` 与差异检查通过。首次全量期间补充广告地址校验造成了新 peer 源码与已编译 Gateway 的快照不一致；该次结束后冻结源码重跑全量，最终通过，没有用定向结果替代失败的全量检查。
- 本检查点交付独立传输组件，尚未增加官方 host/CLI 集群配置、配置指纹、readiness/排空或三节点进程故障矩阵；测试中的 SDK 和反向连接仍使用本地协议夹具，也没有执行真实 Agent 或 Linux 多节点部署。完整 S3 及 S1/S2 集群闭环继续推进。

### 已完成功能：工作台宿主装配共享目录与 peer

- `host.Options.Cluster` 将 PostgreSQL 目录、同一次启动的 Gateway、用户访问上下文和 mTLS 传输装配在一起；Web 模块接收宿主创建的 Gateway。目录与身份、会话、授权、Runner 记录始终使用所选的同一数据库池，默认 SQLite/单机行为保留。非法后端、恢复代次与证书配置在打开存储前拒绝，已有集群目录不能由缺失或错误代次配置的宿主启动。
- `App.ServePeer` 接管独立普通 TCP listener 并使用内部 mTLS 配置，不在公开 HTTP 路由暴露 peer。它复用 App 的请求计数、关闭准入、取消与监听器所有权规则，父 context 或 Close 一并释放连接和存储。跨实例用户请求走原 owner 的一次 peer 连接，不改变 Runner/Runtime 选择或重放行为。
- 工作台机器、Runner 列表/详情及 CLI 机器列表在授权后批量读取共享在线事实，一页最多一百个已准入 ID、数据库查询最多一秒。未确认和过期路由不展示为在线，恢复代次不符或数据库故障返回服务错误；查询不枚举其他机器，也不授予执行权。
- 新宿主回归使用两个正式 `host.App`、独立 mTLS peer 端口和真实 fabricd 进程。机器固定连接 B，Web 与人类 CLI 都固定进入 A，验证远端在线、固定 Runner/Runtime、真实 PTY、企业共享权限与拒绝、策略/用户撤销和 peer listener 释放。存储回归另覆盖发布前后、过期、旧恢复代次及查询 ID 界限；宿主配置回归覆盖 SQLite、非法参数、已初始化数据库及公开路径隔离。
- 这是 Go 宿主的实际接入功能；官方 CLI 集群配置、持续配置一致性租约、readiness/排空及三进程故障矩阵仍待实现。现有启动检查不能阻止并发混合模式，部署模式切换须先停站；文档明确要求实例配置一致，没有把这项要求描述为已实现的运行期隔离。此次没有运行真实 Agent、私有权限 SDK 或 Linux 集群部署。
- 验证：启用 PostgreSQL 17 和备份工具的全量 `make test TEST_FLAGS='-p=1 -count=1 -timeout=300s'` 通过，跨进程包 281.872 秒；PostgreSQL 宿主集群/在线事实定向 race、host/Web 全包 race、`make build check-go` 及差异检查通过。先前试编译发现测试误用不存在的配置保存函数，已改为正式 `config.Create` 写入独立测试配置；此后冻结源码，定向和全量均通过。

### 已完成功能：官方 CLI 集群配置与三进程正常链路

- `dune web --cluster-config FILE` 读取私有集群配置，必须同时选择 PostgreSQL；复用上一检查点的 `host.Options.Cluster` 和 `App.ServePeer`，没有另写一套目录或授权链路。配置指定共同恢复代次及每实例 peer 的监听、直接地址、证书、密钥和 CA 文件，相对路径按配置文件目录解析。
- 配置与私钥限制为当前用户私有文件；所有 PEM 限普通文件及 64 KiB，不跟随符号链接，不阻塞读取 FIFO。沿用传输模块的证书双用途、主机匹配、CA 和期限检查，错误信息不包含 PEM 正文或配置秘密。公开 HTTP 与 peer 监听器分离，所有端口绑定完成后才开始服务；peer 端口占用使启动失败并释放已经取得的公开端口。
- 新回归启动三个独立官方服务进程及真实 fabricd，使用共享 PostgreSQL、各自 peer 证书与监听器。A 签发安装材料、B 消费并持有机器连接，A 和 C 分别通过 Web、人类 CLI 和 Runner SDK 访问 B；另一入口退出父会话后检查既有用户访问失效。测试证书只写入专用临时目录，使用默认 owner 策略，不代表企业权限 SDK 或真实 Agent 验收。
- 这一步证明正式配置与三进程正常运行链路，尚未覆盖三节点网络分区、配置漂移、时钟扰动、readiness/排空或 Linux 部署。下一步继续完成持续配置一致性准入及故障范围，不把正常运行测试视为完整 S3 交付。
- 验证：启用 PostgreSQL 17 的 `go test -race ./internal/config ./tests -run 'Cluster|PeerPEM|PrefixedWorkbench|HumanCLILoginAndExecution' -count=1 -timeout=240s` 通过，跨进程包 121.696 秒；配置全包 race、`make build check-go` 和差异检查通过。新增测试首次遗漏浏览器 Origin 头，被已有 CSRF 校验拒绝；补齐测试头后通过，最终端口冲突与单机拒绝回归也已通过。本次沿用上一功能点刚完成的全量证据，新增 CLI 改动执行其直接影响的配置和进程回归，没有宣称重跑了不相关的全量 Agent 测试。

### 已完成功能：PostgreSQL 配置准入与原输入期限隔离

- schema 12 为同一 PostgreSQL 后端中的所有宿主（含单机模式）保存非敏感配置指纹、独立启动身份、恢复代次与十五秒预约，五秒续约。同配置可增加副本；不兼容设置、并发混合模式、过期 boot 或错误恢复代次拒绝。恢复代次核对持有与旋转兼容的行锁，未知注册/续约不返回有效期限，也不自动重放。
- 已知指纹覆盖协议、挂载路径、身份 namespace、会话期限、注册开关与访问模式。自定义身份/策略的 PostgreSQL 宿主必须声明 `ConfigurationVersion`；CLI 为 `--configuration-version`。企业 SDK 内部设置由宿主的非敏感版本表达，Dune 不读取闭包、密码或私钥；每实例的公开 origin、peer 地址及兼容凭据轮换可不同。
- `gateway.AdmissionLease` 将数据库调用起点折算的保守本地期限传入连接上下文，限制应用 hook、发送与 fabricd 输入 grant，core 不理解 SQL 或业务配置。过期判定不依赖关闭定时器已获得调度；迟到续约不能复活旧 boot，新的续约也不延长已发出的原输入标识。准入失效关闭本次 App，已受理的工作不被回滚。
- Close 停止续约，但不提前删除尚未到期的预约；不兼容配置须等待原期限自然结束，防止缓冲输入跨越切换。首次升级需协调停站，未参与准入协议的旧二进制不能混跑。SQLite 迁移表结构但不启用共享准入，升级失败保留旧 schema 和业务记录，SQL 转库及原生备份保存实际预约。
- 新回归覆盖跨池并发、兼容与冲突注册、恢复代次、锁等待后过期、提交回执丢失、升级失败与恢复、定时器延迟和输入期限。宿主测试观察实际续约、关闭后的等待及自然到期变更；正式 CLI 进程暂停超过十五秒后恢复会退出，原终端缓冲输入未创建文件，新配置启动后原 fabricd 与 PTY 可继续使用。
- 既有身份迁移回归已按新准入流程等待旧配置期限，并为企业模块声明版本；失败时不再把空 App 交给清理函数。此处仅重试明确的配置冲突，不重试未知提交。当前验收仍不包括数据库时钟跳变、三节点分区、readiness/排空、Linux 集群或真实 Managed 提供方，完整方案继续推进。
- 验证：最终冻结源码启用 PostgreSQL 17 与备份工具的 `make test TEST_FLAGS='-p=1 -count=1 -timeout=480s'` 通过，跨进程包 314.925 秒；准入/升级/备份/转库定向 race、宿主续约与真实进程暂停 race、Gateway/host/Web 全包 race、构建与静态检查通过。首次全量因旧身份迁移夹具未声明版本且未等待原租约到期而失败，修复夹具和空指针清理后，先通过身份定向回归，再重新完整执行全量。未启用真实 Agent 或远端测试。
