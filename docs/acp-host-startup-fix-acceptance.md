# ACP 宿主启动修复与交接

2026-09-21。在 `main` 上从 `366ed68` 实现，保留该基线的 Gateway limits 修复。
固定源码为本报告所在提交；构建版本和 SHA-256 记录在交付目录的 manifest 中。
本报告覆盖 Dune 的隔离验收，不代表线上 Runner 已升级。

## 修复

- `CreateHost` 在重定向标准输入输出之前打开只读 fd3 `/dev/tty`。宿主整个生命周期保有该描述符，进入 Go 宿主后设为 close-on-exec，返回时关闭。guardian 的 fd3/fd4 和 Agent 的独立 stdio 管道保持各自归属。
- 执行文件与 `program` 以打开文件的身份匹配。Linux 使用 `/proc/self/exe` 锚定实际执行 inode；macOS 使用执行路径打开文件。祖先目录符号链接不影响匹配。最终文件 no-follow、SHA-256、大小、所有者、0700 权限和单链接检查仍有效。
- tmux 启动之前在原登记库保存最小资源与实例证据。宿主单次进入、成功登记和 Agent group 登记与确定失败使用同一事务边界。启动诊断只含固定阶段、错误码和确认超时标记，见 [诊断合同](acp-diagnostics.md#startup-evidence)。
- 已接纳回执始终保持 accepted。托管 ACP 在 initialize 成功后记录 started；原始 ACP 只启动独立 stdio owner。十秒确认期限不变，超时不封死晚到宿主、不重放请求。
- 已确认失败可按原 Runtime stop/forget，保留原启动回执并释放容量。旧版尚未登记的失败启动也有显式生命周期入口；需原 bootstrap/实例/保留程序、已退出窗格、内核进程缺席及事务内无宿主证明。证据不足仍未知。
- 没有改写 state_dir、installation ID、socket 或 Runtime 身份算法，没有迁移现有 SQLite schema。现存原键继续有效。

## 环境与命令

本地为 macOS arm64 / Go 1.27.1，固定 tmux 3.7c。远端为
`n37-106-250`（10.37.106.250）、Linux amd64 / Go 1.26.5 / tmux 3.7c。
远端使用 `/home/zhangmingyuan.mervyn/dune-startup-verify.79rvxQ` 临时源码目录，
测试各自创建私有 SQLite、tmux server 和本地 TCP Gateway/connector。
这些是目标机上的同机多进程测试，不是跨主机 HA 验收。

```sh
make test
make check-go web-check web-build
go test -race ./internal/sessionregistry ./pkg/fabricd \
  -run '^Test(StartupFailure|FailedACPStartup|UnregisteredFailedStartup|LateOriginalHost|ACPHostWithSymlinked)' \
  -count=1 -timeout=180s

go test ./internal/retainedprogram ./internal/tmux ./internal/sessionregistry ./pkg/fabricd ./tests \
  -run '^Test(Executing|LinuxDetached|HostSurvives|StartupFailure|FailedACPStartup|UnregisteredFailedStartup|LateOriginalHost|ACPHostWithSymlinked|ManagedACPOriginalProcess|RawOriginalPipe|ForgetResumesOnlyOriginal|ForgetConfirmedLostHost|OriginalHostRetains)' \
  -count=1 -timeout=240s -v

DUNE_REAL_AGENT=1 go test ./tests \
  -run '^TestRealAgent(Managed)?ACP$' -count=1 -timeout=150s -v
```

真实 Agent：本地 Gemini CLI 0.34.0；Linux 为临时目录安装的 codex-acp
0.16.0，使用专用空配置目录。Linux 通过 `DUNE_REAL_ACP_COMMAND` 指定命令
JSON 数组。这两个测试只发送初始化握手，不发送模型 prompt，也不证明真实
编码任务或生产账号可用。使用目标机日常 Codex 配置的首次探针未完成初始化；
隔离配置的原始与托管入口均成功，未改写日常配置。

## H01–H07 证据

| 编号 | 实际覆盖 | 结果 |
| --- | --- | --- |
| H01 | Linux / tmux 3.7c：旧三标准描述符重定向命令得到 signal=1；fd3 保活版本运行并正常退出；真实宿主完成登记 | 通过 |
| H02 | Linux codex-acp、macOS Gemini，两者的原始与托管入口均完成 initialize；tmux capture-pane 为空；Linux `/proc` 检查宿主 fd3 保有终端、Agent/guardian 无终端描述符 | 通过，真实初始化 |
| H03 | 实际保留程序经过祖先软链接启动、重连保持原 PID；独立 inode、最终软链接、摘要、权限、硬链接异常被拒绝 | 通过，Linux 与 macOS |
| H04 | mock ACP 在途及排队任务跨 fabricd SIGKILL/SIGTERM；原始管道和托管任务保留，原 PID/Runtime 不变，无额外 initialize/new/prompt | 通过，进程回归；未以真实模型任务复测 |
| H05 | 进入前退出、bootstrap/权限异常、exec 失败、initialize 拒绝分别保留确定失败；确认超时后原宿主晚到成功；原键及重复提交不重放 | 通过 |
| H06 | 新旧失败启动 stop/forget；正常宿主清理在六个故障点恢复；原键保留、live 容量释放、用户文件保留；邻接 tmux 与 PTY 回归 | 通过 |
| H07 | Linux amd64 实机及 macOS arm64 回归；Linux/macOS × amd64/arm64 构建 | 构建结果见交付 manifest；Linux arm64、macOS amd64 待实机验收 |

清理回归原先用 `/bin/sleep` 充当托管 Agent。由于 started 现在要求完成 initialize，
已改用真实协议 mock；六个崩溃恢复断点保持不变。Web 构建仅有既有的 bundle
大小提示。未配置的真实 PostgreSQL、其他 opt-in Agent 测试不计为通过。

## 交付与线上边界

交付物包含固定提交、四平台程序/发行归档、SHA-256 和命令日志。源码测试与
发布程序版本应分别核对，不以构建或在线状态代替 ACP 初始化证据。

本次没有替换线上 Runner，没有处理现场那四个原提交，也没有停止现有 ACP/PTY。
SandDance 后续锁定 Dune/IM 提交，按常驻宿主预检升级发行包，然后以原键查询、
使用原 Runtime 身份执行明确的生命周期清理，并以新提交键进行接入验收。
旧启动缺失或冲突证据时保留 unknown，不能通过删库、删 bootstrap、批量杀 tmux
或重新执行已接纳请求恢复。
