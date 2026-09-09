# Dune

Dune 是面向个人开发环境的 Web 工作台。浏览器负责登录、环境选择、终端与 Agent 交互；Go Web 宿主内嵌 Gateway，开发机上的 connector 主动连回站点并运行 PTY、ACP、文件浏览和 Git diff。

产品不再提供人类 CLI 登录、远程命令执行或文件、Git、端口操作客户端。`gateway` 与 `fabricd` 仍是独立启动的服务，不再由裸 `dune` supervisor 一键拉起；`dune` 还保留 Web 站点以及网页接入开发机所需的 `enroll`、`service` 支撑命令。底层 `pkg/client`、`pkg/sdk` 和 `pkg/gateway` 继续供 Web 宿主与内部协议使用，不是人类远程操作入口。

## 分别启动 Gateway 与 fabricd

```sh
./bin/dune --config /absolute/gateway.yaml init \
  --listen 0.0.0.0:7443 --gateway ws://YOUR_HOST:7443/tunnel
./bin/dune --config /absolute/gateway.yaml gateway

# 在执行机使用包含 Gateway 地址、机器凭据和 session_dir 的配置：
./bin/dune --config /absolute/machine.yaml fabricd
```

两个进程应由各自的服务管理器独立部署和重启。裸 `dune` 不再同时启动它们；Gateway 重启不应被当作 PTY 销毁操作，fabricd 重连后仍按原 Runtime 身份恢复可用会话。

## 启动 Web 工作台

```sh
make release
./bin/dune --config /absolute/web.yaml init \
  --listen 0.0.0.0:7443 --gateway ws://YOUR_HOST:7443/tunnel
./bin/dune --config /absolute/web.yaml web \
  --data /absolute/private-accounts --assets web/dist --binaries bin \
  --url http://YOUR_HOST:7443
```

示例使用 HTTP/WS。对外部署应在可信反向代理后使用 HTTPS/WSS，并保留完整部署前缀。`web --url` 可以是 `https://example.com/tools/dune/`；API、静态资源、Cookie、浏览器 WebSocket 和安装引导都会使用该前缀。

机器使用独立入口时传入 `--gateway-url wss://machines.example.com/private/connect`，并把它代理到 Web 服务的 `/tools/dune/tunnel`。该选项只改变 connector 地址，不改变浏览器入口。

浏览器注册或登录后，可生成一次性接入命令。安装脚本下载平台包，在开发机上完成 `enroll` 和用户级后台服务安装；无需 sudo 或预装 Agent。Linux 使用 systemd user，macOS 使用登录用户的 launchd。

## Web 能力

- 以逻辑 Runner 选择 Attached 或 Managed 开发环境。
- 固定 Runner、Fabric、机器和绑定修订，绑定变化后要求用户重新选择。
- 启动与恢复 PTY/ACP 会话；关闭网页不会停止远端任务。
- 在开发机保存 Agent 命令、参数和环境变量。
- 浏览目录并查看 Git diff。
- 显式停止、遗忘会话或解绑开发机。

会话选择同时固定 Runtime 的 ID、incarnation 和 generation。同 ID 的执行身份变化后必须重新选择；自动重连不会跟随新的执行身份，也不会重放结果未知的写入或 Agent 请求。

## 数据与身份

Web 登录、浏览器 Session、Runner/Machine 绑定、短期连接票据、发现游标和 Managed 生命周期都需要事务存储。默认使用 `--data` 目录中的私有 SQLite；同一目录只允许一个实例持有。

PostgreSQL 配置使用：

```sh
./bin/dune --config /absolute/web.yaml web \
  --database-config /absolute/private/database.yaml \
  --url https://example.com/tools/dune/
```

数据库配置文件必须属于当前用户且权限为 0600，内容为 `postgres: {url: "postgres://…"}`。`--database-config` 与 `--data` 不能同时使用。多实例可再提供 `--cluster-config`；连接目录、peer TLS、配置准入和排空边界见[集群接入](docs/peer-transport.md)。

企业浏览器登录可添加 `--identity-config /absolute/private/identity.yaml`：

```yaml
oidc:
  issuer: https://identity.example.com
  client_id: dune
  client_secret: REPLACE_WITH_PRIVATE_SECRET
session_lifetime: 8h
```

启用后只显示企业登录，本地密码登录与注册关闭。Dune 不保存上游 access/refresh token，按 issuer 与 subject 识别用户。配置与恢复边界见[元数据说明](docs/metadata-operations.md)。

## Web 宿主

`pkg/host.Open(ctx, options)` 装配账号、Attached、内嵌 Gateway 和 Web API。返回的 `App` 可作为 `http.Handler` 挂载，也可调用 `App.Serve(listener)`。挂载时中间件必须保留 Hijacker 和 ResponseController 能力。参考[工作台宿主](samples/workbench/main.go)和[企业宿主](samples/enterprise/enterprise.go)。

`App.Shutdown(ctx)` 停止新请求和新 worker 迭代，并在 deadline 内排空已受理工作；`Close` 立即取消并释放存储。官方 Web 进程对 SIGINT/SIGTERM 使用 `--drain-timeout`，默认五秒。排空时 `health/ready` 返回 503，仍可服务时 `health/live` 返回 200。

企业宿主可注入：

- `Options.Identity`：OIDC 或其他可信身份提供方。
- `Options.AccessChecker`：统一控制发现、Attached 管理、Web 执行和持续输入。
- `Options.Observer`：接收去敏后的结构化访问、连接、路由和 Managed 事件。
- `Options.Cluster`：在 PostgreSQL 上装配多实例连接目录和 peer 路由。
- `Options.Managed`：提供模板、创建、引导、检查、续期、暂停、恢复、销毁和核对能力。

Managed provider 通过 binding ID 与 revision 固定实际适配器。Create 首次接受后，Bootstrap、Inspect、Renew、Pause、Resume、Review 与 Destroy 都只恢复同一 revision。Pause 先关闭访问并持久分发断流请求；Resume 只在 provider 报告 Ready 且 connector 恢复健康后开放访问。未决动作、未知结果和历史记录均由 Web 数据库持久化，后台 worker 不依赖浏览器保持在线。

可信宿主还可调用 `App.SetPrincipalEnabled` 停用用户：该事务会撤销浏览器 Session 与待消费安装命令，关闭已有用户连接，但保留机器身份和远端任务。重新启用后必须重新登录。

## Connector 支撑命令

这些命令由网页安装流程使用，不是人类执行客户端：

```sh
dune --config /absolute/machine.yaml enroll --site https://example.com/tools/dune/ \
  --token ONE_TIME_TOKEN
dune --config /absolute/machine.yaml service install --name dune
dune --config /absolute/machine.yaml fabricd
```

`service restart` 只重启 connector；tmux 会话继续运行。结束或删除远端会话应在 Web 工作台中显式操作。

## 开发与验证

```sh
make build
make test
make check
make web-check web-build
```

按改动选择检查及外部 PostgreSQL、真实 Agent、远端环境的验收边界见[开发流程](docs/workflow.md)。UI 约定见 [DESIGN.md](DESIGN.md)，访问与浏览器绑定语义见[访问检查](docs/access-checks.md)。
