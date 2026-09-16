# Dune：仓库工作约定

Dune 用 Go 提供 Agent 执行、PTY/ACP、文件、Git 和端口能力，包含个人 Web 工作台、Go SDK、Gateway 和 fabricd。

## 首要规则：原型阶段不考虑兼容性

> **本项目当前处于原型开发阶段。默认不保留任何向后兼容性。**

- 不为旧 API、旧类型、旧导入路径、旧配置、旧数据格式或旧行为保留兼容层、别名、适配器或迁移垫片。
- 重构时直接采用目标结构，删除已经失效的接口和实现；不要因为兼容性顾虑保留重复代码。
- 数据库 schema、Provider contract、HTTP API 和配置格式均可随原型设计直接调整，并同步更新调用方、测试和文档。
- 只有用户在当前任务中明确要求兼容某个具体对象时，才为该对象保留兼容性。

## 代码以人的理解为先

> **代码首先是写给人阅读、理解和维护的。实现必须清晰表达业务意图，并遵循 SOLID 等架构设计原则。**

- 使用准确、明确的命名，保持直观的控制流和数据流；不为缩短代码而使用晦涩缩写、隐式约定或炫技写法。
- 遵循 SOLID 原则（单一职责、开闭原则、里氏替换、接口隔离、依赖倒置），保持函数、类型和模块职责清晰，明确模块边界与依赖方向。
- 抽象和设计模式必须服务于实际需求与可理解性；优先采用简单、直接的实现，避免过度设计、无意义的分层和不必要的间接调用。
- 优先通过代码本身表达意图；注释用于解释不直观的业务规则、设计取舍和约束，不得用注释掩盖难以理解的实现。

## 工作方式

- 在用户请求与已有授权范围内完成实现、验证和交付。常规可逆选择自行判断；只有缺失信息会实质改变结果或权限范围时才提问，继续推进不依赖答案的工作。
- 用户明确要求优先于本文件和技能中的默认流程；仍遵守运行环境的更高优先级指令和权限。需要新增授权时，先完成可做的准备，提交具体可审阅的结果。
- 按任务需要读取文件、制定计划和使用工具。独立读取可并行；环境允许委派且有可独立验收的子任务时，可用子代理减少等待，避免多人同时修改同一文件。简单修改直接完成。
- 按改动影响选择验证，必要时补充能捕获行为回归的测试；检查通过后仅因新改动、失败或未解决疑点扩大或重跑。最终简述结果、验证和实质限制。

## 按任务找上下文

| 任务 | 优先入口 |
| --- | --- |
| 构建、测试、运行 | [开发流程](docs/workflow.md)、[Makefile](Makefile)、[README](README.md) |
| Web / SDK / API | `cmd/dune/`、`pkg/sdk/`（内部默认拨号）、`pkg/client/`（协议）、`pkg/api/types.go` |
| 隧道、鉴权、协议 | `pkg/gateway/`、`pkg/transport/`、`internal/gateway/`（启动装配）、`internal/wire/`、`proto/dune/dtp/v1/message.proto` |
| 产品身份与访问 | `internal/identity/`、`internal/authorization/`、`pkg/access/` |
| SQL 元数据与连接配置 | `internal/metadata/`、`pkg/storage/` |
| 执行、PTY、ACP、进程生命周期 | `pkg/fabricd/`、`internal/daemon/`（启动装配）、`internal/process/`、`internal/tmux/`、`internal/service/` |
| Web 功能与宿主装配 | `pkg/host/`、`internal/webapp/`、`web/src/`、[Web 方案](docs/personal-web-plan.md) |
| UI 样式和交互 | [DESIGN.md](DESIGN.md)、`web/src/components/`、`web/src/styles.css` |
| 跨进程回归 | `tests/`；针对具体模块再读对应测试 |

文档按适用范围理解：`docs/spec.md` 和 `docs/runner-tunnel-protocol.md` 是底层目标设计，`docs/mvp.md` 与 `docs/implementation.md` 描述单机 MVP，后续 Web 扩展见 Web 方案。MVP 的“无 UI / 无历史”不限制已确认的 Web 功能。实现事实查代码和测试；方案不等于已交付，历史验收不等于当前环境已验证。发现矛盾时修正相关说明，不据此恢复旧架构。仅做设计研究时再读 `docs/references/`。

## 工程约定

- Go 版本以 `go.mod` 为准，修改 Go 文件后用 `gofmt`。Web 使用 npm 锁文件、Rspack、React、Tailwind 和本地 shadcn/ui；沿用现有组件与样式。
- Web 宿主 / SDK 操作走 Gateway 网络链路。当前传输是 fasthttp WebSocket + Yamux + 长度前缀 protobuf，schema 源文件在 `proto/`；用 `make proto` 更新生成文件。
- 保持目标、incarnation/generation、Runtime 和账号归属校验。断线、EOF 或传输确认不代表业务成功；结果未知时不自动重放写入或 Agent 请求。
- tmux 与 fabricd 生命周期不同：重启连接服务保留 PTY；`runtime stop` 销毁对应 tmux 会话及历史。原始 ACP 透传与 Web 托管 ACP 是两个入口，修改一方时检查另一方是否受影响。
- `.local/` 存放本机配置和验收材料，可能含凭据；只读取任务需要的内容。构建产物在 `bin/`、`web/dist/`，工具在 `.tools/`，均不作为源码提交。

## 验证入口

常用命令为 `make build`、`make test TEST_PKGS=./internal/wire`、`make check-go`、`make web-check web-build`。首次依赖准备、变更类型与检查的对应关系、真实 Agent/远端验收条件统一见 [开发流程](docs/workflow.md)。需要选择或执行一组 Dune 验证时使用仓库技能 `dune-verify`；普通编辑不必加载该技能。
