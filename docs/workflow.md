# 开发与验证流程

## 常用命令

```sh
make build
make test
make check-go
make web-check web-build
```

Go 版本以 `go.mod` 为准。Go 源码修改后运行 `gofmt`；Web 使用仓库锁文件。
protobuf schema 修改后运行 `make proto`，再运行 wire、Gateway、fabricd 和
client 的相关测试。

`make test` / `make test-race` 默认每个测试包最多 900 秒。ACP 的 L53/L54
进程回归会实际打满普通及必要控制证据预算；race 下可持续数分钟，不能用原
180 秒期限截断后视作功能失败。定向快速检查可覆盖 `TEST_FLAGS`。

`make build` 同时准备固定版本的 tmux 和 ripgrep。四平台归档、搜索依赖与授权说明
的打包方式见[发行说明](releases.md)。

`im/` 是独立 Go module，直接运行根目录的 `go test ./...` 不会覆盖它。
默认 `make test` / `make test-race` 包含两个 module，`make check-go` 同时执行两者的 vet。
指定 `TEST_PKGS` 时只检查主 module 的指定包；IM 定向检查使用
`make test-im IM_TEST_PKGS=./feishu` 或 `make test-im-race`、`make check-im`。
IM 的本地 Gateway/ACP 整链测试需要与主仓库测试相同的 `bin/tmux`（`make tmux` 准备），
全部只使用本地假 Agent 和假飞书服务。接入和平台验收步骤见 [IM 说明](../im/README.md)。

## 按改动选择检查

| 改动 | 最小检查 |
| --- | --- |
| 身份、Session、Runner、enrollment | `go test ./internal/identity ./internal/authorization ./internal/metadata ./internal/webapp ./pkg/host` |
| Gateway、peer、协议 | `go test ./internal/wire ./pkg/gateway ./pkg/transport/... ./pkg/fabricd` |
| Web UI | `make web-check web-build`、`npm --prefix web test`；交互变化另做浏览器回归 |
| CLI/宿主装配 | `go test ./cmd/dune ./pkg/host ./tests -run '^$'` 后运行相关端到端用例 |
| 可选 IM module / 飞书 / IM ACP | `make test-im-race check-im`；修改 host 边界时另跑 `go test ./pkg/host` |
| SandDance 公共边界 | 在相邻 SandDance 仓库运行 `go test ./...` |

交付前运行全量 Go、静态检查和 Web 构建。仅文档改动可按实际影响缩小。

ACP 常驻宿主的本地交付证据、L01–L55 覆盖边界与后续 SandDance/平台验收入口见
[生命周期交接报告](acp-session-lifecycle-acceptance.md)。

## PostgreSQL 回归

设置 `DUNE_TEST_POSTGRES` 指向专用测试数据库。测试会创建并删除随机 schema，
账号需要 create/drop schema 权限；不要指向业务数据库。

```sh
DUNE_TEST_POSTGRES='postgres://…' go test ./internal/metadata ./pkg/fabricd ./tests -count=1
```

重点检查：

- 本地登录 schema 包含 users、sessions、runners、enrollments、routes、profiles、profile_revisions、projects、views、agent_credentials；
- 企业 identity 模式包含 runners、enrollments、routes、profiles、profile_revisions、projects、views、agent_credentials；
- route 并发竞争只有一个 owner，失租后以更高 epoch 接管；
- 旧 owner 的发布、续租和释放均被拒绝；
- peer owner 重新验证 Session、Runner binding 与策略；
- 入口或 owner 故障时不重放结果未知的请求，fabricd 重连后原 tmux 可 attach。

SQLite 回归始终使用临时私有目录，检查独占锁、文件权限、外键、单次 enrollment、
Session/Runner 撤销和重开后的当前结构。Dune 不提供备份恢复验收。

## 真实 Agent 与远端验证

mock ACP 只验证协议，不证明真实 Agent 可用。涉及 ACP 行为、PTY Runtime 或授权
交互时，按任务配置 `DUNE_REAL_AGENT_*` 运行 `tests/real_agent_test.go` 对应案例。

跨主机验证必须记录目标提交、OS/架构、数据库位置、Gateway/connector 拓扑、
测试命令与清理结果。同机多进程不能表述为跨主机 HA；没有真实 PostgreSQL 时
被跳过的测试也不能计为通过。

## 前端与部署

前端变更运行 TypeScript/lint 与生产构建。生产验收同时检查 Nginx 直接返回静态
文件，API/downloads 反代后端，`/api/v1/ws/` upgrade 可用，而 backend 在
`--assets=` 下不返回静态页面。本地组合模式仍用 `--assets web/dist`。

## 失败处理

只因相关代码变化、失败或未解决疑点扩大检查范围。网络 EOF、提交确认丢失或
连接重建不能当作业务成功；测试和实现都不得自动重放写入或 Agent 请求。

原生 Codex 验收使用专用且已登录的 `CODEX_HOME`；测试会信任其临时工作目录和唯一注入的 SessionStart hook，不修改日常 Codex 配置。两个入口均需显式设置 `DUNE_REAL_AGENT=1 DUNE_NATIVE_CODEX_HOME=/绝对路径/隔离且已登录的目录`，普通回归默认跳过。

- 启动与工具发现：`go test ./pkg/host -run '^TestRealNativeAgentMCPStartup$' -count=1 -timeout=120s -v`。实际 CLI 启动注入的 bridge，完成宿主鉴权并读取包含 `agents_list` 的 MCP 工具目录；只使用原生初始化和 `/mcp`，不提交模型请求。

启动测试通过不代表模型已经调用工具；工作台不支持进程退出后的恢复。完成验收后清理专用目录中的复制凭据。
