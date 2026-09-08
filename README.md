# Dune

Go 单机 Agent 执行服务。CLI / Go SDK 通过 fasthttp Gateway，以 WebSocket + Yamux 调用本机 fabricd；支持 PTY、ACP stdio、文件上传、结构化 Git 操作和 loopback TCP 转发。

MVP 范围见 [MVP](docs/mvp.md)，具体接口与限制见 [实现说明](docs/implementation.md)。默认单机模式仍不提供完整 Web 产品层。

个人 Web 工作台的产品边界见 [个人 Web 版本方案](docs/personal-web-plan.md)：统一托管站点、个人开发机接入、PTY 持久会话与 ACP 交互。实现状态以当前代码和自动化测试为准。

企业接入与多实例演进见 [开源扩展与企业私有部署方案](docs/enterprise-extensibility-plan.md)：定义 SSO、外部授权、服务发现、共享存储和 Gateway 集群的扩展边界。该文档是设计提案，不代表这些能力已实现。

## 启动

```sh
make build
./bin/dune init
./bin/dune
```

`init` 创建 `~/.config/dune/config.yaml` 和随机 token，文件权限 0600。默认使用 HTTP/WebSocket（`ws://`），在 WebSocket 升级前通过 HTTP Bearer token 鉴权，不生成或要求 TLS 证书。已存在配置不会被覆盖。默认监听 `127.0.0.1:7443`，可显式配置 `0.0.0.0` 对外监听。

当前线协议为 `dune-mvp/2`，执行输入使用有界租约。已有部署须协调升级 Gateway、fabricd、CLI 与 Go SDK，旧协议会在握手时拒绝；保留配置、机器凭据和 tmux PTY，重启 fabricd 会结束其托管 ACP。升级与回退步骤见[协议说明](docs/implementation.md#网络和协议)。

裸命令监督两个独立服务进程，异常退出以 100ms 至 5s 的退避自动重启。Ctrl-C 正常关闭服务与 ACP/Exec 子进程，tmux 托管的 PTY 会话继续运行。不要同时用同一配置启动多个 supervisor。

另开终端：

```sh
mkdir -p /tmp/dune-demo
./bin/dune capabilities
./bin/dune profile start samples/shell.yaml
# 在 shell 中输入 exit，或者另开终端操作：
./bin/dune runtime list
./bin/dune runtime attach RUNTIME_ID
./bin/dune runtime stop RUNTIME_ID
./bin/dune exec --cwd /tmp/dune-demo -- /usr/bin/uname -s
```

`profile start` 在进程启动前建立输出订阅，默认进入交互；`--detach` 返回 Runtime 后断开订阅。再次 attach 由 tmux 恢复当前画面，滚动历史使用 tmux copy-mode。PTY 自动转为 raw TTY 并同步终端尺寸。`Ctrl-C` 在 raw TTY 中发送给 Agent，结束服务请在 supervisor 所在终端操作。

真实 Agent 样例为 `samples/agent-pty.yaml`（本机 Codex）和 `samples/agent-acp.yaml`（本机 Gemini）。调整为已登录可用的命令即可；Dune 不配置 Agent 账号。ACP CLI 逐行收发 JSON-RPC，调用方负责初始化、权限请求应答与会话逻辑，不能把它当普通聊天终端。

## 使用工作台账号登录 CLI

```sh
dune login --site https://example.com/tools/dune/
# 在浏览器登录，核对终端与页面的代码，再确认。
dune --login ~/.config/dune/login.json runners
dune --login ~/.config/dune/login.json --runner RUNNER_ID exec --cwd /tmp -- /usr/bin/uname -s
dune logout
```

浏览器沿用站点已配置的本地账号或企业 SSO。`login` 默认打开浏览器，`--no-browser` 只打印确认地址；`--file /absolute/private/login.json` 选择另一份凭据，退出时使用相同的 `--file`。凭据文件不覆盖已有文件，须保存在当前用户拥有的私有目录，权限为 0600。自定义 CA 使用 `login --certificate /absolute/ca.pem`；站点必须为 HTTPS，仅数字 loopback 地址允许 HTTP。

CLI 会话最长八小时，也不超过确认它的浏览器会话期限；浏览器退出、用户停用或身份关联会撤销相关 CLI 会话及已建立访问。`logout` 只撤销这份 CLI 会话，不退出浏览器。每次执行交换一个三十秒内单次使用的目标凭据；不把用户会话交给 fabricd。登录确认或交换结果未知时不会自动重放，应重新发起登录或明确核对结果。

`--login` 与显式 `--config` 互斥，放在命令前；执行使用 `--runner` 选择逻辑环境，也可用 `machines` 和 `--target MACHINE_ID` 直接选择机器，两种选择不可同时传入。可用命令沿用下述 Runtime、Files、Git、Ports 等接口。机器注册及连接服务仍使用原机器配置。列表返回 `items` 和可选 `next_cursor`；用 `--limit 1..100` 和 `--cursor` 翻页，空页带游标时可继续查询。默认发现和访问采用 owner 策略，Go 宿主可注入企业检查器；集群自动路由仍在实施。

Runner 是稳定的产品身份；其绑定快照包含 Runner、Fabric、机器及单调修订。CLI 每次命令只解析一次，交换凭据时核对完整快照，绑定变化后拒绝旧请求并要求重新选择。SDK 的 `Client.Runners` / `Client.Runner` 查询 `pkg/runner.Runner`，`Client.DialRunner(ctx, session, *selected.Binding)` 仅连接该快照，不自动解析替代环境。新注册的 Runner 与机器 ID 不同，已有机器的原 ID、凭据和 Runtime 不改变。

## Files、Git、Ports

```sh
./bin/dune files samples/files-write.json
./bin/dune upload-file /absolute/local/file /tmp/dune-demo/uploaded
./bin/dune git samples/git-status.json
./bin/dune ports forward 8080 3000
```

Files/Git/upload 的 CLI 接收结构化 JSON 文件，`-` 表示从 stdin 读取。完整字段在 `pkg/api/types.go`，操作枚举与错误语义在实现说明中。JSON 的字节字段（`data`）使用 base64。`upload-file` 自动计算 SHA256 并分块上传；失败时 stderr 中的上传 ID 可用于 `upload` 的 query/chunk/commit 显式续传。默认不覆盖已有文件。

端口转发只监听本机 `127.0.0.1`，每个客户端 TCP 连接都经过 Gateway 转发到 fabricd 的 `127.0.0.1:3000`。

## Go SDK

```sh
go run ./samples/client
```

SDK 入口 `pkg/sdk`，请求与结果类型 `pkg/api`。调用方提供 Gateway URL、token 和目标。`ws://` 不需要 `TLSConfig`；显式使用 `wss://` 时才提供经过验证的 TLS 配置。`Start` 返回 Runtime 和交互 Stream，`Input` 返回请求 ID，需要从 `Recv` 收到对应 `written` 才确认输入写入。所有 API 均经真实网络链路，不直接访问 daemon。

宿主已有字节连接时可使用 `pkg/client.Connect(ctx, conn, target)`，该协议包不依赖 HTTP 或 WebSocket 实现。`pkg/sdk` 保留原有默认拨号及执行 API。

人类客户端使用 `pkg/login.New(login.Options{Site: "https://example.com/tools/dune/"})`；调用 `Begin(ctx)` 后展示返回的 `VerificationURL` 和 `Code`，由用户在浏览器明确确认，再用 `Attempt.Wait(ctx)` 取得 `Session`。`Client.Machines(ctx, session)` 查询目标，`Client.Dial(ctx, session, target)` 交换短期凭据并返回正常的 `*sdk.Client`。调用方私下保管 `Session.Token`、关闭两个客户端，并通过 `Client.Logout(ctx, session)` 撤销登录；`Client.Close` 只释放 HTTP 连接池。

公开的 `pkg/gateway` 和 `pkg/fabricd` 可分别嵌入服务端和执行环境，无需产品数据库；参考 [Gateway 示例](samples/gateway/main.go)和 [fabricd 示例](samples/fabricd/main.go)。应用负责连接认证、HTTP 挂载及监听；core 持有连接和流，关闭规则见对应 Go API 注释。

自有接入模块可通过 `access.Grant.Policy` 装配逐操作检查器，保留可信用户与绑定，按短期决定检查执行和持续输入。完整映射、授权期限及宿主装配状态见[执行流访问检查](docs/access-checks.md)。当前默认工作台仍使用 owner 授权，企业 HTTP/发现入口正在统一接入。

`pkg/host.Open(ctx, options)` 装配本地账号、Attached、Gateway 和默认工作台，官方 `dune web` 也使用此入口。返回的 `App` 可作为 `http.Handler` 挂载到已有服务，保留完整部署前缀；或调用 `App.Serve(listener)`，将 listener 的所有权交给 Dune。`Close` 取消请求与订阅、等待处理退出并释放存储，`Done` 表示释放完成；挂载模式下不关闭宿主的 HTTP 服务。HTTP 中间件须保留 Hijacker 与 ResponseController（可通过 Unwrap）能力。参考[独立工作台宿主](samples/workbench/main.go)。

`App.Shutdown(ctx)` 先停止新请求、新流和新的 Managed worker 迭代，等待已受理工作后关闭；排空边界前已领取的生命周期迭代可在同一 deadline 内完成，超时则取消当前提供方调用并保留持久动作供恢复。没有 deadline 时默认等待五秒，远端 PTY 保留。要优雅退出，应先调用 Shutdown，再取消传给 Open 的生命周期 context；父 context 取消和 Close 仍立即停止。官方 `dune web` 的 SIGINT/SIGTERM 已按此处理，`--drain-timeout` 默认 `5s`、`0` 表示立即关闭。部署前缀下的 `health/ready` 在排空时返回 503，`health/live` 在已有工作仍可服务时返回 200。

企业宿主可设置 `Options.Observer` 接收 `pkg/observe.Event`。事件经 256 项有界队列异步串行投递，覆盖最终访问检查、连接/路由生命周期、本地与跨实例流、容量拒绝及 Managed provider 调用耗时；固定字段不含外部 subject、内部地址、错误正文、命令、文件、终端、prompt、候选引用或凭据。慢 sink 不阻塞业务，`App.ObservationStatus()` 可读取累计丢弃数；sink 必须响应每次调用的有界 context。该出口不提供执行前可靠审计提交语义，完整契约见[结构化观测](docs/observability.md)。

Go 宿主可通过 `Options.Cluster` 在同一 PostgreSQL 后端装配连接目录和跨实例用户访问，使用独立 `App.ServePeer(listener)` 提供双向 TLS 入口。配置方式和当前交付边界见 [peer 传输接入](docs/peer-transport.md)；官方 `dune web --cluster-config FILE` 使用同一装配；PostgreSQL 配置一致性由有界准入租约校验；就绪探针和限时排空的使用方式见[就绪与退出](docs/peer-transport.md#就绪与退出)。

宿主默认经浏览器公开入口的 WS(S) 隧道连接同一应用；可用 `DialGateway` 提供保留认证的本地网络路由或 TLS 信任配置。机器 `GatewayURL` 覆盖不改变这条工作台链路。`DataDir` 选择私有 SQLite 目录；也可通过 `Database: &storage.Config{Postgres: ...}` 选择 PostgreSQL，并用 `BeforeConnect` 更新每条新连接的鉴权配置。应用持有并关闭同一个数据库连接池，领域 Store 不作为可独立替换的公开接口。企业身份、授权、集群及 Managed 生命周期的实现和剩余真实平台验收见[实施记录](docs/enterprise-implementation.md)。

Go 宿主通过 `Options.Managed` 注入公开模板和按 Fabric 命名的 `Create`、`Bootstrap`、`Inspect`、`Renew`、`Destroy`，以及只读的候选资源核验接口，并从 `DefaultManagedWorkerOptions()` 开始配置有界 worker。配置完整时，同一工作台开放模板发现、创建、生命周期状态、销毁与人工核对 API，后台 worker 独立于浏览器恢复持久操作；配置缺失时这些路由与入口不会发布。销毁接受时先关闭数据库访问，再把旧 machine 的断流请求持久分发给当时每个 live 实例；全部实例完成本机 Gateway 清理才确认关闭，失联时保留 `timed_out` 事实并按固定上限继续资源清理。人工核对只能查询原动作或提交由适配器验证的候选引用，没有强制标记成功或重发变更的入口。worker 默认保留终态历史 30 天，按数据库时间低优先级分批清理；unknown、残留资源、未确认关闭和近期幂等记录不会删除。`HistoryRetention` 可配置为 24 小时至 366 天，并进入 PostgreSQL 准入指纹。适配器保存私有 SDK 配置和凭据，Dune 只持有接口及公开模板。当前仓库没有随官方 CLI 提供真实平台适配器，测试夹具不能当作已支持的云环境。

可信宿主可调用 `App.SetPrincipalEnabled(ctx, principalID, false)` 停用 Dune 用户；宿主负责校验管理员权限。停用在同一事务中撤销该用户的全部会话与待消费安装命令，已有用户连接会关闭，机器身份和远端任务保留。传入 `true` 重新启用后必须重新登录，旧 Cookie 不会恢复。此接口没有对应的匿名或普通用户 HTTP 管理入口，也不等同于上游企业身份源的停用同步。

## 开发与验证

构建、依赖准备、按改动选择测试及真实 Agent/远端验收统一见 [开发流程](docs/workflow.md)。常用入口：`make test`、`make check`；前端依赖已安装时使用 `make web-check web-build`。`make web` 保留锁文件安装、类型检查和构建的完整流程。

Agent 协作约定见 [AGENTS.md](AGENTS.md)，验证任务可使用仓库技能 `dune-verify`。测试结果需区分本地回归、mock 协议、真实 Agent 任务与目标平台运行。

无需账号的 ACP 联调：

```sh
go build -o /tmp/dune-mock-acp ./samples/mock-acp
./bin/dune profile start samples/mock-acp.yaml
```

逐行输入 `samples/acp-initialize.json` 的请求，然后发送 `session/new` 和 `session/prompt`。prompt 文本包含 `permission` 时，mock 发出 `session/request_permission`，收到响应后发出 update 与最终结果。它明确标记为 mock，不调用模型。

## 从本机直连远端

服务器初始化并启动：

```sh
dune init --listen 0.0.0.0:7443 --gateway ws://gateway.example.test:7443/tunnel
dune
```

`listen` 是服务绑定地址；`gateway` 是 fabricd/SDK 使用的具体连接地址，不能填 `0.0.0.0`。本机只需一份客户端配置（无需 listen、证书或私钥）：

```yaml
gateway: ws://gateway.example.test:7443/tunnel
target: local
token: <服务器生成的 dev token>
```

```sh
./bin/dune --config .local/remote.yaml capabilities
./bin/dune --config .local/remote.yaml exec --cwd /tmp -- uname -s
go run ./samples/client --config .local/remote.yaml --cwd /tmp
DUNE_REMOTE_CONFIG="$PWD/.local/remote.yaml" go test ./tests -run TestDirectRemote -v -count=1
```

这些 API 调用直接连接远端端口；SSH 只用于部署。`--cwd` 是远端路径。

## 个人 Web 工作台

个人 Web 工作台支持远端站点与开发机控制。页面技术栈为 Rspack + React + Tailwind CSS + shadcn/ui，不使用 RSC。

```sh
make release   # 构建页面和 Linux/macOS × amd64/arm64 安装包（含 tmux）
./bin/dune --config /absolute/gateway.yaml init \
  --listen 0.0.0.0:7443 --gateway ws://YOUR_HOST:7443/tunnel
./bin/dune --config /absolute/gateway.yaml web \
  --data /absolute/private-accounts --assets web/dist --binaries bin \
  --url http://YOUR_HOST:7443
```

示例使用 HTTP/WS，无需证书。浏览器注册后选择接入开发机，执行网页给出的同站安装命令。安装脚本不需要 sudo 或预装 Agent；首次绑定写入私有机器配置，随后启动用户后台服务。

工作台以逻辑 Runner（开发环境）为入口。选择环境后，页面固定当时的机器、Fabric 和绑定修订；刷新可以更新在线状态，绑定变化则关闭旧工作区并要求明确进入当前环境。进入环境或刷新页面不会自动连接已有终端/Agent，会话仍需点选；解绑只撤销 Dune 接入，不删除开发机文件或停止远端任务。

会话选择同时固定 Runtime 的 ID、incarnation 和 generation；同 ID 的执行身份变化后需要重新点选，自动重连不能跟随新身份。Runner 与旧机器事件 API 都要求完整 Runtime 查询参数，升级时须同步后端、静态资源和自定义订阅客户端，详见[浏览器入口契约](docs/access-checks.md#浏览器-runner-入口)。

工作台默认使用 `--data` 目录内的 SQLite，无需安装数据库服务；同一目录只允许一个实例持有。使用 PostgreSQL 时传入 `--database-config /absolute/private/database.yaml`，文件须属于当前用户且权限为 0600，内容为 `postgres: {url: "postgres://…"}`，不能同时指定 `--data`。两种后端使用同一套业务事务。当前按完整结构初始化空库，不提供旧数据迁移；存储配置与同结构备份恢复见[元数据说明](docs/metadata-operations.md)。

`web --url` 可包含部署前缀，例如 `https://example.com/tools/dune/`。API、静态文件、Cookie、浏览器 WebSocket 和安装引导均使用该前缀；机器默认连接 `wss://example.com/tools/dune/tunnel`。反向代理须保留前缀交给当前官方 Web 入口，末尾缺少 `/` 的浏览器入口会跳转到规范地址。

机器使用独立入口时显式传入 `--gateway-url wss://machines.example.com/private/connect`，并将该入口代理到 Web 服务的 `/tools/dune/tunnel`。此覆盖只改变安装绑定返回的机器地址，不改变浏览器或内部工作台连接的路径。原有 `--url` 与机器配置中 `gateway` 不同的部署（例如额外 loopback 浏览器入口），升级时也应显式填写 `--gateway-url`。省略 `--url` 时仍从原配置的 `gateway` 派生浏览器地址。

工作台从部署目录下的 `api/bootstrap` 读取登录方式、注册状态、Attached/Managed 能力和公开地址。默认官方 CLI 装配本地账号与 Attached；Go 宿主提供完整 `Options.Managed` 时才显示托管模板、精确参数表单和持久生命周期状态。添加 `--disable-registration` 可关闭本地注册，已有账号仍可登录；服务端同步拒绝注册请求。启动信息读取失败时页面提示重试，不假定所有功能可用。

企业浏览器登录可添加 `web --identity-config /absolute/private/identity.yaml`，配置如下。文件须属于当前用户且权限为 0600；部署到 HTTPS，并在身份源注册精确回调地址，例如 `https://example.com/tools/dune/api/auth/external/callback`。仅数字 loopback 地址允许 HTTP 本地调试。

```yaml
oidc:
  issuer: https://identity.example.com
  client_id: dune
  client_secret: REPLACE_WITH_PRIVATE_SECRET
session_lifetime: 8h
```

配置后页面只显示企业登录，本地密码登录和注册均关闭。会话期限默认 8 小时，可设为 1 分钟至 24 小时；不会保存上游 access/refresh token。Dune 按 issuer 和 subject 识别用户，邮箱只作展示，不自动合并同邮箱账号。退出撤销 Dune 会话，不注销身份源会话。上游停用尚无自动同步，已有 Dune 会话以配置期限为界，宿主也可主动停用用户。身份源配置和恢复边界见[元数据说明](docs/metadata-operations.md)。

Go 宿主通过 `host.Options.Identity` 注入 `pkg/identity.Options`。内置 `pkg/identity/oidc.Open` 提供 OIDC 适配器，也可实现公开 `identity.Provider` 接入其他可信协议；提供方必须校验协议、响应 context，并支持并发和跨实例回调。关联已有账号时，可信管理员可按[身份关联流程](docs/metadata-operations.md#显式关联既有账号)调用 `App.LinkIdentity`，保留原用户和机器归属并记录决定。身份提供方不决定 Runner 访问权限。通过 `host.Options.AccessChecker` 选择企业检查器，统一控制发现、Attached 管理、Web/CLI 执行和持续流；拒绝或故障不回退到 owner 规则。公开契约、操作映射与分页规则见[访问检查](docs/access-checks.md)。

PostgreSQL 下使用自定义身份、权限或 Managed 提供方模块时，宿主须设置共同的 `Options.ConfigurationVersion`；官方 OIDC CLI 对应 `--configuration-version oidc-policy-v1`。Managed 的公开模板、提供方命名空间和 worker 行为另自动进入非敏感指纹，私有 SDK 配置变化仍由宿主推进声明版本。不兼容配置不能与旧实例同时运行，变更前协调停站并等待原准入期限到期；具体配置约定见 [配置准入](docs/peer-transport.md#配置准入与变更)。

企业检查器取得本次登录验证过的 `Namespace` / `Subject` 与 Dune principal，CLI、连接凭据和安装材料保留同一引用，不按邮箱或关联列表猜测身份。主体与会话的持久化边界见[身份说明](docs/metadata-operations.md#身份源与本次登录主体)。

也可在已安装 Dune 的机器手动绑定：

```sh
dune --config /absolute/machine.yaml enroll --site http://YOUR_HOST:7443 \
  --token ONE_TIME_TOKEN
dune --config /absolute/machine.yaml service install --name dune
```

Linux 需要可用的 systemd user；退出所有登录后继续运行取决于机器的用户服务/linger 策略。macOS 使用登录用户的 launchd。`service restart` 只重启 fabricd，tmux 会话继续运行。`runtime stop ID` 显式销毁对应 PTY 及其历史；自然退出的会话保留画面和历史，网页可单独删除。

进入或刷新页面只展示已有会话，点击会话才连接；断开连接保留远端进程。网页保存 Agent 命令、参数数组、环境变量和 PTY/ACP 模式。PTY 使用随包分发的 tmux 3.7c，提供原画面恢复与原生滚动历史；ACP 先完成能力协商，再通过页面新建对话或从 Agent 加载。离线授权保持等待，缺失历史能力会明确报错。源码与测试中的 mock 仅用于协议验收，不代表真实模型可用。

PTY 实现参考 Botmux 的轻量 attach/detach 封装，细节见 [tmux 后端](docs/tmux-backend.md)。开发构建下载并校验固定版本的官方 tmux 包；目标机器不需要预装 tmux。
