# Dune

Dune 是面向开发环境的 Web 工作台。浏览器负责登录、选择 Runner、终端与
Agent 交互；开发机上的 connector 主动连接 Gateway，并在本机运行 PTY、ACP、
文件浏览与 Git diff。

## 本地启动

本地模式由一个进程同时提供静态页面、API 和 Gateway：

```sh
make release
./bin/dune --config /absolute/dune.yaml init \
  --listen 127.0.0.1:7443 --gateway ws://127.0.0.1:7443/tunnel
./bin/dune --config /absolute/dune.yaml web \
  --data /absolute/private-dune-data \
  --assets web/dist --binaries bin \
  --url http://127.0.0.1:7443/
```

浏览器注册或登录后生成一次性接入命令。安装流程在 Linux/macOS 上写入
connector 配置并安装用户级后台服务，不要求目标机器开放入站端口。

## 生产部署

生产环境由 Nginx 直接服务 `web/dist`，将 `/api/`、`/downloads/` 和 `/tunnel`
反向代理到不提供静态文件的 Dune Gateway/API 进程。启动后端时传空 assets：

```sh
./bin/dune --config /absolute/dune.yaml web \
  --database-config /absolute/database.yaml \
  --cluster-config /absolute/cluster.yaml \
  --assets= --binaries=/absolute/releases \
  --url https://dune.example.com/
```

最小 Nginx 路由如下；`/tunnel` 必须保留 WebSocket upgrade，部署前缀存在时
同样保留完整前缀，不使用 `StripPrefix`：

```nginx
location /api/       { proxy_pass http://dune_backend; }
location /downloads/ { proxy_pass http://dune_backend; }
location /tunnel {
    proxy_pass http://dune_backend;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
}
location / { root /srv/dune/web/dist; try_files $uri /index.html; }
```

`--gateway-url` 可为 connector 指定独立的公网 WSS 入口。PostgreSQL 多副本还
需要每实例独立、直接可达的 mTLS peer 地址；peer listener 不经公开 Nginx。

## 数据与身份

Dune 只维护五种逻辑数据：

| 部署 | 表 |
| --- | --- |
| 本地 SQLite | `dune_users`、`dune_sessions`、`dune_runners`、`dune_enrollments` |
| 本地登录 PostgreSQL | 上述四张 + `dune_routes` |
| SandDance 企业 PostgreSQL | `dune_runners`、`dune_enrollments`、`dune_routes` |

SQLite 由单实例独占。PostgreSQL 的 `dune_routes` 保存短期 owner 租约和单调
epoch，允许多个 Gateway 在旧 owner 失租后自动接管。接管会短暂断开浏览器流，
connector 自动重连，tmux 进程仍在；结果未知的写入和 Agent 请求不会自动重放。

Dune 不实现数据库备份、恢复、历史回滚或恢复协调，也不维护兼容旧 schema 的
迁移。只在空 schema 中原子创建当前结构。

`pkg/identity.Service` 是公开浏览器会话接口：本地默认实现使用密码与上述两张
身份表；企业宿主验证自己的 opaque cookie，并直接返回企业用户唯一标识。
Dune 不保存企业用户、企业 Session、OIDC token 或身份映射。

## Managed

前端与 Web API 保留 Managed Runner 功能，集成边界是
`pkg/managed.Service`。Dune 不包含 provider 编排、worker、续期、恢复、review
持久化或生命周期表；SandDance 等宿主自行实现这些能力并注入
`host.Options.Managed`。`host.Open` 会通过 `BindRunnerAccess` 注入一个窄能力，供
服务创建/查询 Dune 的逻辑 Runner 与一次性 enrollment；Dune 只持久化这些接入
事实。pause/resume/destroy 被外部服务接受后，Dune 再持久冻结、解冻或撤销对应
Runner，并主动关闭本实例上的旧连接。

## 撤销与访问

Runner 解绑会在 `dune_runners` 中持久保存 disabled 状态、清除机器凭据，并
主动关闭当前 Gateway 的连接。其他 Gateway 通过约一秒一次的有效性复核
和 owner 租约到期兜底，不写逐实例关闭回执。

浏览器连接票据、peer 委派 nonce 和分页游标都只存在进程内或请求本身，不落库。
目标 owner 经 mTLS 收到 peer 请求后仍会重新验证当前企业/本地 Session、Runner
绑定及访问策略。传输断开或收到 EOF 不代表业务成功。

## 宿主接口

`pkg/host.Open(ctx, options)` 装配登录、Attached、Managed Web API 与 Gateway。
`App` 可作为 `http.Handler` 挂载，也可接管 listener；`ServePeer` 接管独立 mTLS
peer listener。企业宿主可注入：

- `Options.Identity`：企业 Session 验证；nil 使用本地密码。
- `Options.AccessChecker`：发现、连接和每项执行操作的策略。
- `Options.Managed`：宿主拥有的高层 Managed 服务。
- `Options.Cluster`：PostgreSQL route 目录与 peer 传输。
- `Options.Observer`：去敏结构化事件。

## Connector 支撑命令

以下命令服务网页安装链路，不是远程执行客户端：

```sh
dune --config /absolute/machine.yaml enroll --site https://dune.example.com/ --token TOKEN
dune --config /absolute/machine.yaml service install --name dune
dune --config /absolute/machine.yaml fabricd
```

重启 connector 不销毁 tmux 会话；停止 Runtime 才销毁对应会话与历史。

## 开发与验证

```sh
make build
make test
make check
make web-check web-build
```

按变更选择检查见[开发流程](docs/workflow.md)，集群边界见
[peer 传输](docs/peer-transport.md)，授权语义见[访问检查](docs/access-checks.md)。
