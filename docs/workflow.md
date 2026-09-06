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
| 并发、订阅或状态生命周期 | `make test-race TEST_PKGS='./pkg/fabricd ./internal/webapp ./pkg/gateway ./pkg/transport/tunnel ./internal/tmux'`，缩小或调整为实际涉及包；涉及进程边界再选 `tests/` 回归 |
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

在 Make 变量中使用正则结尾 `$` 时写成 `$$`，避免被 Make 当作变量展开；直接运行 `go test` 时不需要这一层转义。确认选中的测试实际执行，`[no tests to run]` 不算行为验证通过。

`tests/` 即使按 `-run` 过滤也会执行 TestMain 的 race 构建；包内 Go 测试的 race 检查仍需 `test-race`。需要明确排除已继承的外部测试开关时：

```sh
DUNE_REAL_AGENT= DUNE_REMOTE_CONFIG= make test
```

## 外部验收

以下操作会使用已配置的 Agent 账号或指定远端，仅在任务包含对应验收且已有授权时运行。沿用明确指定的账号、模型和机器；历史文档里的地址与登录状态不是当前授权或可用性证据。环境缺失时报告具体缺口，继续本地检查。

```sh
# 本机真实 PTY 编码任务；默认使用已配置的 codex
DUNE_REAL_AGENT=1 go test ./tests -run '^TestRealAgentPTY$' -v -count=1 -timeout=180s

# 已配置的 Gemini ACP，仅验证 initialize
DUNE_REAL_AGENT=1 go test ./tests -run '^TestRealAgentACP$' -v -count=1 -timeout=180s

# 指定已有客户端配置；该测试会在远端 /tmp 创建并清理测试资源
DUNE_REMOTE_CONFIG=/absolute/client.yaml \
  go test ./tests -run '^TestDirectRemote$' -v -count=1 -timeout=180s
```

PTY 测试支持 `DUNE_PTY_AGENT=claude`，Dune 不代办登录或修改模型配置。上述测试超时还受测试内部 context 限制；增加 Go 的 timeout 不会延长内部期限。

`TestRealAgentPTY` 经 Dune 创建代码并独立运行检查；`TestRealAgentACP` 只测试握手。完整 Web 编码验收还需要从页面提交任务、看到实际结果并独立验证产物。mock ACP 可验证协议与权限状态机，不能替代真实模型任务。

发布构建不自动部署。对指定环境的操作应使用独立配置和测试目录，保留已有会话；连接服务重启只替换 fabricd，终止 tmux 会话需显式执行 `runtime stop`。

## 完成与续接

审阅本次差异并报告结果、实际验证和剩余限制。跨阶段任务可在现有进展文档中记录完成项、关键决定和下一步；小改动无需新建计划或验收档案。只有影响实现语义或验收状态时更新对应文档，避免多份清单重复维护。

当前目录若没有 Git 元数据，用修改清单和文件快照审阅；无需为执行开发流程创建 Git 仓库。将来接入 CI 时复用 Makefile 入口，外部账号/远端验收保持显式启用。
