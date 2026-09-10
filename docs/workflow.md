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

## 按改动选择检查

| 改动 | 最小检查 |
| --- | --- |
| 身份、Session、Runner、enrollment | `go test ./internal/identity ./internal/authorization ./internal/metadata ./internal/webapp ./pkg/host` |
| Gateway、peer、协议 | `go test ./internal/wire ./pkg/gateway ./pkg/transport/... ./pkg/fabricd` |
| Web UI | `make web-check web-build` |
| CLI/宿主装配 | `go test ./cmd/dune ./pkg/host ./tests -run '^$'` 后运行相关端到端用例 |
| SandDance 公共边界 | 在相邻 SandDance 仓库运行 `go test ./...` |

交付前运行全量 Go、静态检查和 Web 构建。仅文档改动可按实际影响缩小。

## PostgreSQL 回归

设置 `DUNE_TEST_POSTGRES` 指向专用测试数据库。测试会创建并删除随机 schema，
账号需要 create/drop schema 权限；不要指向业务数据库。

```sh
DUNE_TEST_POSTGRES='postgres://…' go test ./internal/metadata ./pkg/fabricd ./tests -count=1
```

重点检查：

- 本地登录 schema 只有 users、sessions、runners、enrollments、routes；
- 企业 identity 模式只有 runners、enrollments、routes；
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
文件，API/downloads 反代后端，`/tunnel` upgrade 可用，而 backend 在
`--assets=` 下不返回静态页面。本地组合模式仍用 `--assets web/dist`。

## 失败处理

只因相关代码变化、失败或未解决疑点扩大检查范围。网络 EOF、提交确认丢失或
连接重建不能当作业务成功；测试和实现都不得自动重放写入或 Agent 请求。
