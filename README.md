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

公开的 `pkg/gateway` 和 `pkg/fabricd` 可分别嵌入服务端和执行环境，无需产品数据库；参考 [Gateway 示例](samples/gateway/main.go)和 [fabricd 示例](samples/fabricd/main.go)。应用负责连接认证、HTTP 挂载及监听；core 持有连接和流，关闭规则见对应 Go API 注释。完整产品的宿主 SDK、SQL、企业身份与集群进展见[实施记录](docs/enterprise-implementation.md)。

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

也可在已安装 Dune 的机器手动绑定：

```sh
dune --config /absolute/machine.yaml enroll --site http://YOUR_HOST:7443 \
  --token ONE_TIME_TOKEN
dune --config /absolute/machine.yaml service install --name dune
```

Linux 需要可用的 systemd user；退出所有登录后继续运行取决于机器的用户服务/linger 策略。macOS 使用登录用户的 launchd。`service restart` 只重启 fabricd，tmux 会话继续运行。`runtime stop ID` 显式销毁对应 PTY 及其历史；自然退出的会话保留画面和历史，网页可单独删除。

进入或刷新页面只展示已有会话，点击会话才连接；断开连接保留远端进程。网页保存 Agent 命令、参数数组、环境变量和 PTY/ACP 模式。PTY 使用随包分发的 tmux 3.7c，提供原画面恢复与原生滚动历史；ACP 先完成能力协商，再通过页面新建对话或从 Agent 加载。离线授权保持等待，缺失历史能力会明确报错。源码与测试中的 mock 仅用于协议验收，不代表真实模型可用。

PTY 实现参考 Botmux 的轻量 attach/detach 封装，细节见 [tmux 后端](docs/tmux-backend.md)。开发构建下载并校验固定版本的官方 tmux 包；目标机器不需要预装 tmux。
