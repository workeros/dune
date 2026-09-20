# ACP 生命周期：本地验证与 SandDance 交接

需求来源：相邻 SandDance 仓库 `docs/dune-acp-session-lifecycle-requirements.md`，
L01–L55。用户确认的本阶段范围是 Dune 本地实现、回归与交接；随后由 SandDance
Review、接入并完整验收，真实 Agent 和外部平台环境稍后提供。本报告不代表完整
矩阵已验收，不修改 SandDance 依赖或业务 Runner。

## 版本与合同入口

实现分支 `feat/acp-session-lifecycle`，基线 `bd17acc`。四平台构建对应干净源码
`03632d1d61e866971c17cc7f7ca0d64f6dddb6ba`；其后的验收说明提交只改变文档。
按 Tracer Bullet 切片的决策、失败及修复过程见
[实施记录](acp-session-lifecycle-implementation.md)。本项目不提供旧协议兼容层。

| 接入内容 | 入口 |
| --- | --- |
| caller-owned ID、启动/Runtime 作用域、回执、stop/forget 和所有默认预算 | [提交合同](acp-submissions.md)、`pkg/api/submission.go`、`pkg/client/submission.go` |
| 运行期异步操作和 ACP 读模型 | `pkg/api/agent.go`、`pkg/api/acp_conversation.go`、`pkg/agents/` |
| 启动响应完全丢失后的应用查询 | `agents.Launcher.QueryLaunch`；HTTP `agents/launch-submission`；MCP `agents_launch_submission` |
| SDK 原始字节接管、输入和续读 | [raw 合同与示例](raw-acp.md)、`pkg/client/raw_acp.go`、`samples/raw-acp/` |
| Runtime 列表完整性、局部错误和原身份 | [发现合同](runtime-discovery.md) |
| 升级/回滚前置校验及保留旧程序 | [升级合同](acp-upgrades.md)、`dune upgrade-check`、`fabricd.PrepareUpgrade` |
| 真实进程版本、日志、额度 | [诊断](acp-diagnostics.md)、`dune version --runner`、`machine.info` |
| IM 持久目标/ScopeResolver 合同变化 | [IM 接入](../im/README.md) |

`acp.persistent` 声明能力，Runtime 的 `persistent_acp` / `acp_mode` 描述实际实例。
当前独立宿主 IPC 仅支持版本 1；传输协议仍为 `dune-mvp/2`。

## 本机执行记录

本机 macOS arm64、Go 1.27.1、固定 tmux 3.7c / ripgrep 15.1.0。测试使用私有
临时目录、实际 tmux、独立宿主/fabricd 和可控 Agent。安装测试使用服务管理器替身，
没有调用真实 launchctl/systemctl。`DUNE_REAL_AGENT`、`DUNE_TEST_POSTGRES`、
`DUNE_NATIVE_CODEX_HOME` 均未配置，相关案例跳过；它们不计入通过数。
两项显式开启的资源测量测试也未启用（`DUNE_CONVERSATION_RESOURCE_TEST`）；
常规运行中的子进程 helper 跳过由其独立进程用例另行调用。

| 检查 | 实际执行与结果 |
| --- | --- |
| 主 module 全量 race | `go test -race -json ./... -count=1 -timeout=900s`；首轮发现并修复下述两项失败，其他 31 个有测试包通过；修复后 tmux、fabricd、完整 tests 包复验通过 |
| IM 全量 race | `go -C im test -race -json ./... -count=1 -timeout=900s`；4 个包全部通过，feishu 26.1 秒 |
| 静态/schema | `make check-go check-proto` 通过，包含两个 module 的 vet 和 protobuf lint；最终 tmux 修复后再次通过 |
| Web | TypeScript、14 个 unit、生产构建通过；完整 Playwright 29 项通过（12.2 秒）；已查看新增恢复界面的宽/窄截图 |
| 四平台 | `make release` 通过；归档 hash、成员类型/路径、可执行权限、程序内容和 VCS build metadata 核验通过 |

原始本地证据位于 `.local/acp-lifecycle-delivery-20260921/`：
`go-race.jsonl`、`im-race.jsonl`、`static.log`、`web-e2e.log`、`release.log`、
`artifacts.json`、`version.json`，以及 `process-followup.jsonl`、`static-followup.log`、
`tmux-followup.log`、`hostloss-followup.log`、`cli-followup.log`。它们和 `bin/`
属于本机验收材料，不作为源码提交。

全量主 module 首轮发现帮助文本测试把解释中的 “executable” 错当成 `exec` 命令；
`fd722b6` 修正为命令位置匹配，定向 race 回归通过（4.8 秒）。另一处失败发现
tmux server 暂停时，已超时的 client 留在 SCM_RIGHTS 中的管道描述符会阻塞 Go
输出回收；`03632d1` 为查询及版本探测设置有界 WaitDelay。完整 tmux race
通过（5.4 秒），原宿主丢失进程用例通过（11.6 秒）。随后复验完整 `tests` 包与 `fabricd`（跳过首轮已通过的两个高成本容量用例）。
L53、L54 的首轮实际耗时分别为 63.7 秒、362.5 秒，保留该运行证据；
新一轮 `fabricd` 通过（132.5 秒），包含新增离线期限用例；完整 `tests` 包
通过（278.4 秒）。原始失败日志保留，未把首轮写成全绿。修复后的实际命令为：

```sh
go test -race ./internal/tmux -count=1 -timeout=90s
go test -race -json ./pkg/fabricd ./tests \
  -skip '^(TestEachOrdinaryCapacityPreservesOriginalControlsAcrossConnectorCrash|TestCompletedControlEvidenceAndCompetingSubmissionsPreserveOtherTargets)$' \
  -count=1 -timeout=900s
```

Rspack 仍有现有的 bundle-size 提示。

## L01–L55 证据对照

“本地进程”表示所列测试在本机实际覆盖该边界，不外推到其他平台或真实 Agent。
“部分”明确保留尚未组合验证的条件；“外部待验”需后续隔离环境。表中测试名均为
仓库中的可执行入口，不能用仅编译成功代替运行证据。

| 编号 | 本地证据 | 尚需补充的完整验收 |
| --- | --- | --- |
| L01 | 本地进程：`TestManagedACPOriginalProcessAcrossConnectorRestart`、`TestOriginalHostRetainsProgramAcrossReleaseDeletionAndExplicitOpen` 保留原进程/身份及 RPC 计数 | 四平台空闲场景复验 |
| L02 | 本地进程：原 prompt 屏障跨 SIGKILL、离线完成；installer 屏障跨升级和回滚 | 真实长任务 |
| L03 | 部分：guardian 所有权、离线 prompt/工具更新归属已有回归 | 实际外部本地工具子进程跨重启及最终结果 |
| L04 | 本地进程：上述 managed、raw、cleanup、discovery 测试均实际 SIGKILL fabricd | 其他目标平台 |
| L05 | 外部待验；生成 Linux unit 的 `KillMode=process` 和预检行为已检查 | 真正 `systemctl --user restart`、cgroup 子进程证据 |
| L06 | 外部待验；生成 plist 的独立进程组策略及替身安装回归已检查 | 真正 LaunchAgent restart/bootout/bootstrap |
| L07 | 部分：已有 managed Runner/host 全链，恢复不调用 enrollment/setup | 实际 Managed Provider 的仅连接器重启 |
| L08 | 本地进程：managed 主回归的 running/queued 原操作离线完成；额度回归包含多个排队操作 | 多项排序及原 Agent RPC 顺序在接入环境复验 |
| L09 | 本地进程：权限请求跨 SIGTERM 和无浏览器时段保留原 ID | 真实 Agent 权限 |
| L10 | 本地进程：`TestCompletedControlEvidenceAndCompetingSubmissionsPreserveOtherTargets` 竞争/重复/冲突答案 | 真实 Agent 晚到旧答案 |
| L11 | 部分：显式打开、进程替换/旧回调测试与 connector 重连测试均存在 | 在 new/load 替换每个阶段杀 fabricd 的完整组合 |
| L12 | 本地进程：`TestManagedSubmissionAdmissionThroughGateway`、Web 丢首响应回归、原键查询 | SandDance 自己的首响应丢失注入 |
| L13 | 本地合同/网络：`TestSubmissionUsesOriginalSharedOperationAndRejection`、Gateway submission 测试 | 接入层不得生成替代 ID |
| L14 | 部分：64 项结果表、完成后 15 分钟 TTL、原 key 持久保留；`TestOrdinaryResultsRejectNewWorkUntilHostRetentionExpires` | 实际等待到期与多次进程重启组合；无 TTL 自动解封 |
| L15 | 本地合同：`TestSessionHostControlFencesOlderConnectorsAndProbesAreReadOnly`，实际重连 control term 递增 | 两个实际 connector 进程重叠与晚到写组合 |
| L16 | 本地合同/进程：资源 inode/实例固定、socket 替换、binding 拒绝、IM 目标固定 | 其他平台 PID/路径复用组合 |
| L17 | 本地进程：`TestLaunchAdmissionSurvivesMissingFirstResponse`、`TestLateOriginalHostRegistrationRecoversWithoutAnotherLaunch` | 宿主发布前后更多故障时点组合 |
| L18 | 本地进程：`TestDiscoveryRecoversEightHostsBesideUnresponsiveEndpointAndCorruptRegistration` | 目标机同等恢复时限 |
| L19 | 本地进程：离线生成后读原模型；分页/按 ID/独立 cursor 另有 Gateway 回归 | 离线分页及按 ID 的组合复验 |
| L20 | 本地进程/合同：raw 超窗口、managed 有界模型/遗漏标记、慢诊断不阻塞 | 真实 Agent 连续输出超模型预算 |
| L21 | 本地合同/进程：流分类预留、阻塞 writer 可 stop、慢诊断和多宿主恢复 | 全部快慢订阅/多会话压力组合及 RSS 观测 |
| L22 | 本地进程：`TestRawOriginalPipeAcrossConnectorCrashAndInputTakeover` | 四平台 raw stdio |
| L23 | 本地进程/合同：原字节比较、UTF-8 分片、严格完整 JSON、无 PTY | 真实 Agent 大消息 |
| L24 | 本地进程：`TestRawIncompleteNetworkInputAndOfflineOutputGap` 明确 STREAM_GAP | 原始客户端协议内存丢失后的产品处理 |
| L25 | 部分：实际离线管道/接管 + 确定短写 writer 单测，精确比较字节 | 确定短写与 connector SIGKILL 的同一进程组合 |
| L26 | 本地回归：Agent 自然退出、尾部与未确认操作、模型保留 | 真实 Agent 尾部输出 |
| L27 | 本地进程：`TestStopReceiptSurvivesLostResponseAndConnectorRestart`、清理进程测试；相邻会话不受影响 | 原生平台进程组 |
| L28 | 本地进程：`TestHostLossUsesIndependentEvidenceAndDoesNotReplay`、丢失宿主 forget | 真实 tmux server 整体被杀的混合会话组合 |
| L29 | 外部待验；本地已测试 boot identity 和进程缺失证据分类 | 真正重启机器/Sandbox 恢复 |
| L30 | 部分：现有 Access/host/MCP 撤销测试、IPC 私有身份、原 binding 校验 | 重连时 Tenant/成员/凭据到期全组合 |
| L31 | 部分：MCP 配置失败保留 Runtime、RPC 错误不冒充 Runtime 退出 | 实际远端工具断线跨 connector 重启 |
| L32 | 本地进程：两个不同 build stamp/hash 升级/回滚，原在途 prompt、两代宿主、旧 release 删除 | 历史发行版本和真实服务管理器；本地两份程序来自同一源码 |
| L33 | 本地进程：不兼容/缺依赖在 stop 前拒绝；launch gate、晚到原宿主注册 | 其他协议版本程序的真实互操作 |
| L34 | 部分：PTY、managed/raw 各自回归，私有目录隔离和相邻会话清理保护 | 四平台多安装目录的完整混合矩阵 |
| L35 | 本地进程/合同：16 活动 Runtime、键/控制硬限额、日志窗口、 retained program 限额 | 长时间离线的实际 RSS/磁盘曲线；计费预算不等同 RSS |
| L36 | 部分：两个无状态 MCP HTTP handler 交替查询原引用 | PostgreSQL、多真实 host Pod/进程；当前相关测试跳过 |
| L37 | 部分：SDK/host/HTTP/MCP、Web 29 用例、IM 全链；CLI 示例构建和版本读取 | 各真实消费者端到端断线矩阵，尤其 SandDance 接入 |
| L38 | 本地合同/进程：独立 read 配额；原键查询不封闭、probe 不接管、RPC 计数不增 | 接入层重新订阅不能隐式 new/load |
| L39 | 本地合同/进程：私有路径/链接/硬链接/实例/inode、长路径 socket 映射和发现异常 | 各 OS 实际不同 UID 场景 |
| L40 | 外部待验 | 至少一个真实 ACP Agent 的任务、工具、权限及升级证据 |
| L41 | 本地进程：`TestOriginalACPDeadlineExpiresWhileConnectorIsDead` 同时覆盖 raw/managed，在启动新 connector 前确认原宿主 timeout；PTY 另有旧目录删除回归 | 四平台休眠/时钟条件 |
| L42 | 本地进程：8 健康宿主 + 坏端点，独立 2 秒 probe、8 并发、30 秒恢复上限 | 目标机恢复时限复验 |
| L43 | 本地合同/进程/Web：调用前 ID、首响应丢失/cancel、错误留键、非法 ID 无副作用 | SandDance 存储故障/刷新/各错误路径 |
| L44 | 部分：读先于接纳的 registry/网络案例不封闭，随后原键可 accepted | 请求滞留在 HTTP/Gateway 上游的确定屏障组合 |
| L45 | 本地合同：Runtime/launch 不同作用域隔离，同作用域摘要冲突；宿主网络调用固定目标 | 双 Runtime/双 binding 的实际同时执行计数 |
| L46 | 部分：网络半包零写入；确定不可恢复前缀错误封锁队列/新输入 | 确定错误与实际换 connector 的完整组合 |
| L47 | 部分：显式 new/load 继续替换进程，旧代回调隔离；保留程序测试跨重连再次显式 new | 同 session/cwd 的各晚到回调和重启组合 |
| L48 | 本地进程：setup 中崩溃不重跑、caller cancel 后原启动完成、原宿主晚注册 | worktree 前后所有屏障组合 |
| L49 | 本地合同/进程：拒绝 key 持久封闭、升级 gate 拒绝跨替换、满表不回收解封 | 上游晚副本 + 完整回执回收的组合；当前最低证据不自动回收 |
| L50 | 部分：等价 load 两键映射同 operation 的合同测试 | 合并 load 在真实 connector 重启后的 RPC 计数 |
| L51 | 本地进程：`TestForgetConfirmedLostHostNeedsNoStopOrReplacement` | 目标平台丢失证据复验 |
| L52 | 本地进程：`TestForgetResumesOnlyOriginalCleanupAfterProcessCrashes` 各持久/删除屏障、只读暂停查询、旧执行器和路径复用 | 目标文件系统故障条件 |
| L53 | 本地进程：`TestEachOrdinaryCapacityPreservesOriginalControlsAcrossConnectorCrash`，独立打满各普通额度后读/权限/cancel/stop/forget | 真实 Agent 和目标机资源用量复验 |
| L54 | 本地进程：`TestCompletedControlEvidenceAndCompetingSubmissionsPreserveOtherTargets`，默认控制硬限额、冲突/重复/过期、有效目标预留 | 真实 Agent 竞争控制 |
| L55 | 本地合同/进程：同 Runtime 跨接纳者摘要冲突、存活/丢失验证、合法新键清理后封闭 | 接入环境完整组合 |

本表故意保留“部分”：单测、不同用例分别覆盖的事实不能写成已经执行过同一个故障
组合。完整验收可直接复用本地用例，并按最后一列补齐环境和屏障。

## 四平台匹配产物

全部归档包含同级 `dune`、tmux 3.7c、rg 15.1.0 及许可证；Dune 使用
`CGO_ENABLED=0`，VCS revision 为上述源码，`vcs.modified=false`。原始归档放在
本仓库 `bin/`，相邻 `.sha256` 可用于目标机核验。未上传或发布远端 release。

| 归档 | SHA-256 |
| --- | --- |
| `dune-linux-amd64.tar.gz` | `616105e0462c20c11dd60edb54f6cb178ea1f820b82194c2a538b3453eab9b23` |
| `dune-linux-arm64.tar.gz` | `200c4f51afe5d82d34e1c5863a42a935b9ca8ffcf5dd3326c6f224f66ec89280` |
| `dune-darwin-amd64.tar.gz` | `244327efd64f3123c3bb80299f601116d23c9a39a13903eb1ae16268de26b500` |
| `dune-darwin-arm64.tar.gz` | `6066b32221241d300a6cb60a8ebd5c08d62c5f6a54da5b3d19df8cd42eb40fdf` |

本机只验证 macOS arm64 的实际进程。其他三种目标为构建/归档核验，不能算进程验收。
随包 Linux arm64 rg 依赖 glibc ≥ 2.18；详见[发行依赖](releases.md)。

## SandDance Review 与接入顺序

Dune 本地代码和匹配产物作为本阶段交付；本表中的部分覆盖和外部待验项随交接
明确保留。SandDance 接入后的完整矩阵结果另行记录，不把本地 mock 证据外推为
业务集成已通过。

1. 先 Review schema 和错误语义，尤其 `Start`、`Stop`、`Forget`、`ACPSubmit`
   的调用方键及返回值变化；不保留旧入口。再同步 Dune Go 依赖与目标机匹配归档。可用本地
   `replace github.com/aiomni/dune => ../dune` 审阅，不把未发布 commit 冒充 module tag。
   若使用 IM，同时替换 `github.com/aiomni/dune/im`，填写新的完整目标字段。
2. 启动和每次用户意图都在发送前持久保存 ID、准确 binding/Runtime；区分传输
   request ID、admission、operation execution、conversation generation。
3. 首响应未知时用原键只读查询；启动查询不需要 agent_ref。页面刷新也不能改键、
   自动重放或用新 Runtime 替代原目标。`not_accepted` 只能来自持久封闭证据。
4. 用列表的 `complete/issues` 保留暂不可用的原面板，按完整身份去重；恢复订阅后
   读原模型/操作/权限。显式 new/load 才触发既有进程替换，不让重连触发它。
5. stop/forget 也先保存键；`accepted/stopping`、`accepted/cleaning` 是未完成。
   清理后仍查询独立索引，不能依赖活动目录。不要把 IPC 超时当作宿主已死。
6. 升级前从目标程序运行 `upgrade-check`，正式切换使用带 gate 的 service install
   或 repair/upgrade；核验旧宿主 actual build 和 retained digest。验收通过后再修改
   SandDance 当前“升级会结束 ACP”的产品说明。

## 后续环境验收步骤

为每个目标 OS/架构准备独立安装根、session_dir、工作目录和配置，不能选择业务
Runner。先核验归档 hash 和 `dune version`；保留 tmux/rg 同级布局。源代码回归命令
为 `make test-race check` 和 `make web-check web-build`；外部变量只指向专用测试环境。

对实际用户服务，使用唯一服务名，通过 `dune --config FILE service install --name NAME`
安装，再以平台实际 `service restart` 执行。分别在生成中、等待权限、原任务已完成
但页面离线时重启；记录重启前后 connector/host/Agent/guardian/tmux PID、build、
Runtime 身份、operation_ref、conversation_id/revision，以及 Agent 侧 RPC 次数。
Linux 另外记录 cgroup/KillMode，macOS 记录 LaunchAgent 与进程组。SIGKILL connector、
杀宿主、杀 tmux server 和机器重启必须分开记录。

升级和回滚使用两个实际待支持的发布版本，保留正在运行的 Agent；检查预检不支持
协议时先拒绝、原调用仍可继续。再加入本地工具进程和远端 MCP 断线；原 operation
的结果必须可归属，不能只检查 PID。结束后按原键 stop/forget，确认本次测试的资源
清理完成，再停止并移除专用测试服务。没有提供外部环境前不执行这些操作。

真实 Agent 验收应使用已登录的隔离配置和能实际完成的任务，记录 Agent 版本、权限、
工具、工作产物与次数。`TestRealAgentACP` 只证明初始化；它和 mock 进程回归均不能
替代 L40。PostgreSQL/跨 Pod 使用专用数据库（创建/删除随机 schema），当前仍待配置。

## 已知边界

- 只保留同机存活宿主的内存模型/队列；宿主或机器死亡没有磁盘任务恢复，不重跑。
- raw 客户端必须保留自己的协议状态；STREAM_GAP/input_unrecoverable 不透明修复。
- 未完成 setup 不恢复执行；原 launch 阶段和预留身份保持可查。未验证的残留只分类，
  不依路径猜测认领/清理。被卡住的预留可能占用容量，需要受控人工核查。
- 最低接纳证据和身份墓碑不自动回收；到硬上限后限制新工作。默认活动 Runtime 16、
  保留身份 256，详细普通/控制/结果/模型/日志限额见提交合同；正文预算不等于 RSS。
- 显式 forget 才清除已退出宿主及其缓存。原项目文件和 Agent 原生历史不在清理计划内。
- Web 的恢复标识保留在账号隔离的 sessionStorage；关闭标签页/清除站点数据后不保证保留。
- 本地两份升级二进制来自同一源码，未证明历史版本互操作；真实 Agent、原生服务管理器、
  PostgreSQL/跨 Pod、机器重启与其他三平台执行均未验收。
