# 优化交付记录

日期：2026-09-17。方案见 [optimization-plan.md](optimization-plan.md)。代码、消费者迁移和本地集成已完成；下文单独列出目标平台尚未实际验证的项目。

## 版本与消费者

- Dune 实现提交：`1a80d2abd104bbe6ef8631dc6d23d96cf7b249da`，分支 `feat/optimization-plan`，已发布至 `github.com/workeros/dune`。
- SandDance 按用户要求 rebase 到 `origin/dev` 的 `ffd3f41`，实现提交为 `52783e2`、`3a2b884`，分支 `feat/dune-optimization`。
- SandDance 的 Dune 主模块与 IM 模块均通过远端 replace 固定到 `v0.0.0-20260917074412-1a80d2abd104`；`go.sum` 已更新。最终 Go 检查及打包使用 `GOWORK=off`，没有依赖邻接目录替换。
- 原型接口直接替换，没有保留旧 machine HTTP、files/list、Force/Upload.Overwrite、File.Query 或 pkg/renewal 兼容层。

## 交付内容

| 范围 | 已实现行为 |
| --- | --- |
| 授权 | runner.unbind 使用权威 owner/creator/binding 与宿主策略；pending 取消保持 pending-only；认证结果带凭据期限；pkg/access 是持续流许可的唯一缓存，最多 5 秒，新 request/action 实时检查，peer owner 独立检查 |
| 文件 | 内容与权限修订号、描述符一致读取、create/conditional/unconditional、Files/Upload 同一提交锁、原子不覆盖创建、保留权限、提交返回本次字节的 revision |
| 读取界面 | SandDance 完整 EOF/字节长度/SHA256 校验、BOM 保留、截断只读、冲突与未知结果不转为强制写入；新合入的 Git 新文件预览共享读取实现 |
| PTY | 有 timeout 的 Runtime 使用独立 helper；fabricd 离线及旧程序删除后仍能 TERM/KILL；SessionDir 保存小记录，恢复 timed_out 和历史；stop/forget 清理 |
| 搜索 | 固定 rg 15.1.0；路径/内容、regex/case/word、glob/ignore/hidden；UTF-8/BOM 验证；数量、字节、时间、文件大小和并发预算；取消贯通 Gateway，结果不进入大型响应缓存 |
| 搜索界面 | SandDance 结果分组、过滤开关、不完整原因、UTF-8 字节位置转换、上下文核对、dirty draft 保护、取消与迟到响应丢弃 |
| 接入与升级 | 稳定 pending Runner ID；已有配置直接拒绝；未知结果不重放；统一个人/宿主 Attached 安装脚本；独立 repair/upgrade；本次启动回执通过后清理拥有的旧 release；持久路径与程序目录分离 |
| 性能与边界 | 个人内置工作台；按需目录分页；Git common-directory 锁；单 reducer 的有界 ACP 状态；可信代理与账号限流；终端/ACP 按需加载 |
| 发行 | 四平台 dune/tmux/rg/许可证和 SHA256 归档；SandDance build.sh 与新合入的 runner-updater 均处理 rg；进程识别支持启动回执参数和带空格路径 |

SandDance rebase 引入的在线 runner-updater 保留宿主已有的失败恢复流程，成功后删除临时包和旧程序备份；状态存于配置同级目录。它与 Dune install.sh 的本地升级是两个入口。后者按本方案通过本地初始化回执切换，不提供自动回退；在线升级还要求工作台重新连接并核对运行中二进制。没有用保留旧 API 的适配层连接这两条流程。

## 实际验证

本机为 macOS arm64。普通测试禁用真实 Agent；PostgreSQL 使用新建的独立临时实例和随机 schema，完成后停止并删除。安装器组合测试使用私有服务命令替身和真实 fabricd/tmux/PTY，不操作开发机的真实服务。

| 命令或验证 | 结果与证明范围 |
| --- | --- |
| `env -u DUNE_REAL_AGENT make test` | Dune 主模块和 IM 全量通过；包括本地 Gateway/SDK、假 ACP Agent 和真实子进程。后续配置/安装/搜索/bootstrap 小改动已定向重跑 |
| `make check-go` | Dune 与 IM 的 go vet 通过；最终源码已 gofmt |
| 授权/access、文件/搜索/Git、PTY/安装相关 `go test -race` | 通过；覆盖许可缓存与迟到刷新、条件写竞争、Gateway 搜索取消、进程超时和删除旧程序后的生命周期 |
| `go test -race ./tests -run '^TestInstaller' -count=1 -timeout=120s` | 通过，21.47 秒；正确回执才清理、错误 nonce 保留旧目录，以及升级→fabricd 离线→PTY 到期→恢复历史组合流程 |
| Dune PostgreSQL metadata/authorization/Web/Host、peer ownership、cluster Web、enterprise access | 通过；包含入口事实过期时 owner 独立撤销检查。一次空闲终端撤销观测约 487ms，不作为所有拓扑的延迟承诺 |
| `npm --prefix web test`、`make web-check web-build` | 两个 ACP 状态回归、类型检查和生产构建通过 |
| Dune 生产构建浏览器 smoke | mock API/WebSocket 下登录、个人 Managed scope、无企业表单、目录按需分页、终端/ACP 延迟加载通过，零页面错误 |
| `make release` | 四平台 Go 构建及固定依赖下载/校验通过；逐包检查 dune/tmux/rg、可执行位、许可证、路径和 SHA256 |
| SandDance `GOWORK=off go test ./...`、`GOWORK=off go vet ./...` | rebase 后通过，包含新 IM、终端设置与升级模块；升级适配另经 race 检查 |
| SandDance 专用 PostgreSQL：`go test ./internal/storage ./internal/integration ./internal/lifecycle -count=1 -timeout=180s` | 最终远端依赖版本下通过，包含 lifecycle/访问截止时间、IM、终端设置等 SQL 路径；真实 TAE 测试明确跳过 |
| SandDance `pnpm e2e`、`pnpm typecheck`、`pnpm build` | 最终 125 个浏览器用例全部通过（41.6 秒），类型检查与生产构建通过 |
| SandDance `env -u SANDDANCE_DUNE_DIR GOWORK=off ./build.sh` | 远端固定模块构建成功；四平台 SandDance/updater/Dune 归档与校验文件完整 |

浏览器回归中发现并修复了路径搜索携带内容模式参数、xterm 初次布局尺寸非有限值，以及 rebase 后 Git 预览测试夹具缺少强摘要的问题。校验没有通过降低文件一致性要求来绕过。

## 性能证据与保留成本

Dune 同一构建工具下的基线 `cd9513a` 与当前生产产物：

| 项目 | 基线 | 当前 |
| --- | ---: | ---: |
| 入口 JavaScript，未压缩传输 | 654,375 B | 348,488 B（减少 46.7%） |
| 入口 CSS | 45,215 B | 45,598 B |
| 按需 JavaScript | 无独立终端/ACP包 | 终端依赖 288,022 B；终端组件 6,101 B；ACP 19,092 B |

本地生产构建 smoke 初始只请求一个 JS，打开终端后累计三个，打开 ACP 后累计四个。mock 链路下终端和 ACP 可见分别约 822ms、814ms；这些数值只记录该次本地交互，不是生产 SLO，也不表示总代码体积下降了 46.7%。Dune 与 SandDance 构建仍有 Rspack 包体建议警告，SandDance 的 Monaco 资源仍较大。

流权限回归验证连续同 action 输入不再逐帧查询；不同 Git common directory 可并发，同仓库及共享 worktree 仍串行。目录内存有界，但每页仍可能扫描全目录。文件条件摘要仍占用提交锁；搜索的预算是保守工程上限，尚未取得生产仓库分布下的吞吐或尾延迟数据。

## 尚未验证与明确边界

- 未配置真实 Agent/TAE，本轮没有声称真实编码任务、供应商实际资源创建或真实飞书投递已验收。
- 未执行实际主机休眠/唤醒；已使用 Linux BOOTTIME、Darwin MONOTONIC_RAW 并验证普通经过时间和离线超时。唤醒后终止仍需现场验证。
- 四平台产物检查与交叉编译已通过；实际 systemd/launchd 安装升级、跨主机运行以及 Linux 专属 SandDance Managed 升级进程用例未在这台 macOS 上执行。
- Linux arm64 随包 rg 使用 glibc，最低 2.18；不覆盖原生 musl/Alpine。具体依赖见 [releases.md](releases.md)。
- 撤销窗口只覆盖权威源可观察的失效；离线 JWT 上游注销不可观察。已经受理的写入/Agent 不因断线自动重放或回滚。
- 外部写入不受 Engine 文件锁协调，分块读取和搜索不是快照；脱离受管进程组的进程不在 PTY 强终止保证内。

使用说明：[授权](access-checks.md)、[搜索与文件](file-search.md)、[安装升级](install-upgrade.md)、[PTY](tmux-backend.md)、[验证流程](workflow.md)。

## Review 修复与复验（2026-09-17）

上述初次交付后修复了三处边界问题。Dune 提交 `d36b2323322eb32af97fd3b942384eae8464ed5f` 保留搜索 root 的目录别名，使搜索结果与目录列表、文件读取使用相同路径。SandDance 同步锁定主模块与 IM 到 `v0.0.0-20260917081151-d36b2323322e`，升级程序通过目标目录内的临时副本原子替换，并统一取消搜索、目录树、标签切换和编辑器关闭引起的过期导航。

本次重新通过 Dune 主模块与 IM 全量测试、`make check-go`、`make build`、搜索 race 回归，以及 SandDance `GOWORK=off` 全量 Go 测试和 vet、升级模块 race、Web 类型检查和生产构建、135 个浏览器用例。跨文件系统安装已在 macOS 的临时 HFS+ 映像与本机数据卷之间实测通过，测试映像已卸载删除。以上不替代前文尚未执行的真实 Agent、休眠或真实服务管理器验收；本次未重跑 PostgreSQL 和四平台归档构建。
