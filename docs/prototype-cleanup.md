# 原型阶段兼容残留清理

2026-09-21，基于 `3f7ac74` 审查当前 Dune 主 module、IM module、Web、CLI、SDK 与相关测试。结合符号调用、当前启动链路、数据库初始化和消费者代码判断清理范围。

## 已清理

| 发现 | 当前实现 |
| --- | --- |
| `pkg/sdk` 重导出 `Client`、`Stream`、`Port`，并转发 `Connect` | 类型和自定义连接统一使用 `pkg/client`；`pkg/sdk.Dial` 只负责 WebSocket 拨号，返回 `*client.Client`。 |
| `internal/identity` 和 `internal/webapp` 重导出身份类型、错误和机器类型 | 直接引用定义所在的 `pkg/identity`、`internal/metadata`；本地密码实现仍由 `internal/identity` 提供。 |
| 旧 ACP 启动缺少准备记录时，从 bootstrap、tmux 和文件补建失败宿主记录 | 删除补建、补救 stop/forget、旧版专用测试及故障注入。宿主只能按准备、单次进入校验、激活的顺序登记。 |
| `RegisterHost` 在没有准备记录时直接插入活动宿主 | 删除直接插入分支；新增断言覆盖缺少准备记录和未经进入校验的注册拒绝。其他宿主测试改走当前流程。 |
| 升级预检根据旧 connector 锁处理首次架构转换 | 删除旧架构转换分支；注册表目录残缺或 ACP 资源无登记时拒绝升级，只有两者都不存在才按新安装处理。 |
| 多个授权构造器、metadata 可变参数包装、配置初始化包装 | 每个内部入口保留一个明确签名；同步全部调用方。运行时独立预留包装退出生产代码，测试通过当前启动准入或包内预留测试夹具准备状态。 |
| 旧 machine 查询、归属判断、enrollment 用户包装、模板策略和废弃启动结果分类等无调用代码 | 删除实现及失去用途的导入；清除 ACP 旧读取包装、未使用字段和前端导入。 |
| YAML 中只保存、不生效的 `log_level` | 删除字段及其行为；加载时忽略未知设置，只校验当前核心字段。 |

同步修正 SDK 使用说明、升级/诊断合同和 README 数据表列表。历史验收报告明确标注已经删除的旧版用例，避免把历史记录当作当前交付能力。

## 保留的当前功能

原始 ACP 与 Web 托管 ACP、PTY 与独立 ACP 宿主、SQLite 与 PostgreSQL、本地与宿主身份、Web 与可选 IM 都有当前调用链和测试，不属于兼容残留。

协议版本、完整 Runner/Runtime 身份、账号归属、磁盘资源身份、旧 schema 拒绝和结果未知时不重放的校验继续生效。升级门禁、独立宿主保留程序及 tmux 历史服务于当前会话生命周期；清理兼容代码不删除用户会话、数据库或工作目录。

## 调用方影响

Go 调用方应从 `pkg/client` 导入执行类型和 `Connect`，继续用 `pkg/sdk.Dial` 拨号。机器配置只校验当前使用的地址、身份、监听和路径字段；已删除的 `log_level` 或后续新增的未知字段可以原样保留。读取不修改配置，没有提供旧 ACP 记录的迁移适配。

## 验证

环境：macOS arm64，主检查使用 Go 1.27.1；未使用代码检查使用 Go 1.26.5（Staticcheck 2026.1 尚不能解析 Go 1.27 的导出格式）。

| 检查 | 结果 |
| --- | --- |
| `make test` | 主 module 已运行全量包，fabricd 181.5 秒、跨进程 tests 251.5 秒通过。该次命令因下述 PTY 测试竞态退出非零；其余包通过。 |
| `go test ./internal/process -count=1 -timeout=120s` | 修复 PTY 测试读取竞态后全包通过，11.7 秒。主 module 全量包至此均取得通过结果。 |
| `make test-im` | 四个 IM 包全部通过，包含本地 Gateway/ACP/飞书假服务整链。 |
| `go test -race ./internal/sessionregistry ./pkg/fabricd -run 'Test(PendingHost\|StartupFailure\|UpgradePreflight\|FailedACPStartup\|StopDispatch\|StopAccounts\|ReservedACPControls\|CleanupKeeps)' -count=1 -timeout=180s` | 通过；覆盖当前注册顺序、失败退场、容量隔离、清理锁与残缺状态升级拒绝。 |
| `make build check-go` | 构建及两个 module 的 vet 通过。 |
| `GOTOOLCHAIN=go1.26.5 go run honnef.co/go/tools/cmd/staticcheck@2026.1 -checks U1000 ./...` | 在主 module 和 IM module 分别通过。 |
| `make web-check web-build`、`npm --prefix web test` | TypeScript、生产构建和 14 个前端测试通过；构建仍提示现有 bundle 大小超过建议值。 |
| TypeScript 额外开启 `--noUnusedLocals --noUnusedParameters` | 通过。 |
| `git diff --check` | 通过。 |

PTY 测试以前在状态文件已写完时立即断言终端输出，两个读取通道没有完成顺序保证。现在保留原退出码、截止期限与 TERM 断言，并有界等待终端读取器收到证据。首次验证还出现过复制测试可执行文件的 `exec format error`，后续全量与该包检查均未复现，未据此修改产品执行逻辑。

真实 Agent、PostgreSQL、远端主机和原生服务管理器验收未配置或未执行，不计为通过。本次 Web 修改只有删除无用导入，没有增加浏览器交互验收。

## SandDance 消费者修复与验证

已同步修改相邻 SandDance 工作区，采用以下最终合同：

- 删除 `unhosted.go` 和旧 connector 转换逻辑，保留当前宿主身份、资源及启动锁检查。
- 候选程序始终预检；有宿主或已接纳的启动时，才检查回滚程序的宿主协议能力。没有宿主时不做额外回滚协议检查，启动锁仍覆盖替换与回滚，防止期间出现新宿主。
- 机器配置忽略未知字段，`3f7ac74` enrollment 的空 `log_level` 和 init 的 `log_level: info` 均可保留。删除字段专用错误解析和一次性清理脚本，不要求维护者预先清理存量配置。
- 当前接入地址、身份、监听地址、会话和证书路径仍须有效；类型错误和无效 YAML 仍拒绝。预检不修改安装、配置或进程，错误诊断不向 Web 转发配置值。
- 同步 Web 用例和升级文档。

使用 `/tmp/dune-cleanup-consumer.work` 将 SandDance 的 Dune/IM 依赖指向本次工作区，未修改依赖版本。SandDance 的 `go.mod` 仍锁定 `3f7ac74`，默认构建尚未消费本次清理，发布时需统一锁定已发布的新 Dune 版本。

本轮检查均通过：

| 检查 | 结果 |
| --- | --- |
| Dune `make build`、`go test ./internal/config ./cmd/dune ./internal/install ./internal/webapp -count=1 -timeout=180s` | 通过；覆盖未知字段忽略、核心字段校验、CLI、安装与 enrollment。 |
| Dune `go vet ./internal/config ./cmd/dune ./internal/install ./internal/webapp` | 通过。 |
| SandDance `go test ./cmd/... ./internal/... ./pkg/... ./provider/... -count=1 -timeout=900s` | 全部源码包通过，使用上述临时 workspace；排除非法导入 Dune internal 包的历史 artifacts 目录。 |
| SandDance `go test -race ./internal/runnerupdate -count=1 -timeout=180s` | 通过；开启真实二进制环境变量，包含升级、回滚和重试，35 秒。 |
| SandDance `go vet` 同一源码树、`pnpm --dir web typecheck` | 通过。 |
| SandDance `pnpm --dir web e2e e2e/runner-upgrade.spec.ts` | 17 项通过，49 秒；包括核心配置错误的页面反馈。 |

真实二进制验证使用独立目录构建的 `3f7ac74` 和当前 `bin/dune`。`TestUpgradeWithRealConnector` 使用旧程序生成的配置原样完成升级、注入失败后的回滚与重试，同时验证核心路径错误不会替换或重启。旧 CLI 向本机假 enrollment 服务生成的配置也单独验证无需删字段即可通过新版预检，新增未知设置同样被忽略。

这些验证使用 macOS arm64 的临时进程及本机假服务，没有修改线上 Runner 配置，未证明原生 launchctl/systemctl、Linux guardian、真实 Agent 或业务 Gateway 接纳。
