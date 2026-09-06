# 企业扩展方案实施记录

实施依据：[企业扩展方案](enterprise-extensibility-plan.md)。按独立功能提交，阶段只有取得对应证据后才标记完成。S0a、S0b 已通过本地验收；S0c–S3 尚未交付。

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

- `pkg/host.Open` 显式装配现有本地账号、owner 检查、Attached、Gateway 和工作台。公开参数不暴露应用配置、内部 Store 或企业依赖；过渡 JSON 留在内部，后续 S0c 替换运行时存储。
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
