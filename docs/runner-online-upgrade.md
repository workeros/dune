# Runner 在线升级实现与验收记录

目标合同：[Runner 升级需求](../../SandDance/docs/dune-runner-upgrade-requirements.md)。
本记录描述正在实现的在线升级，不能将 ACP 标题等既有交付算作在线升级交付。

## 实现边界

按 Tracer Bullet 推进，每个切片连通实际入口和相应回归后提交。

1. 启动封闭和当前共享状态合同：封闭跨 worker 死亡保留，现存及待启动宿主的合同随其生命周期保留。
2. 安装检查与冻结发行：实际执行映像、全组件清单、持久安装修订、只读预览。
3. 持久幂等接纳、独立 worker、安装切换及崩溃恢复；安装/修复/在线入口共用安装原语。
4. 正常 host → Gateway → Runner 路由证明、执行方持久确认和自动回滚。
5. 公开 Go/HTTP/客户端、宿主可注入存储、离线查询/历史、诊断、保留与平台验收。

只支持一个明确的当前共享状态合同。直接调整 schema 与调用方；不同合同在写入前拒绝，
不迁移、重置或恢复旧数据库快照。不同发行可以共存，但必须遵守相同的读写语义。

成功判定由执行方持久提交：目标实际程序及全组件、原身份 Gateway 路由接纳、正常路由往返
全部确认，且证明匹配操作、尝试、进程和连接代次。单独的本地启动、认证或 PID 不能成功。

## 已实现的切片

### 持久启动封闭

`launchgate` 在持有共享锁时检查私有 `upgrade-seal.json`；文件损坏或不能读取时拒绝启动。
升级方在排他闸门内持久写入安装、操作及 owner，文件与目录都同步后才允许切换。
关闭文件锁不解除封闭。恢复方必须引用原封闭并更换 owner；陈旧或已关闭的 owner 不能清除。
清除必须发生在终态持久提交之后；两者之间崩溃保留封闭，供恢复方完成清理。
普通本地服务安装预检遇到封闭返回 `UPGRADE_RECOVERY_REQUIRED`，不能越过在线恢复。

该原语已由独立 worker 使用；完整交付以本文最后的验收范围为准。

### 运行映像与共享状态合同

`machine.info.running_program` 和 `upgrade-check.program` 返回内核选定的程序摘要、字节数、
进程启动身份和构建信息。Linux 固定 `/proc/self/exe`；macOS 核对自身代码映射的内核 vnode，
并在服务启动前保留文件描述符。替换或删除路径不改变运行事实；无法核验时拒绝，不读 `current`
冒充正在运行的程序。保留 ACP 程序也从同一已核验映像复制。

当前读写合同的完整语义位于 `internal/statecontract/contract.txt`，以其摘要标识。
注册库直接采用包含合同的新 schema，在 tmux 元数据、恢复和后台维护写入前核对；不迁移旧库。
接纳待启动宿主时固定程序摘要及合同，宿主注册继承它，预检枚举待启动及存活参与者。
注册库合同及宿主记录独立于升级历史保留，重启不会解除义务。

### 全发行与安装事务原语

`pkg/upgrade` 定义冻结清单、组件属性、实际观察和独立的更新/重启计划。
清单摘要使用 Go JSON 编码及按路径排序的组件；内容包含平台、下载地址/摘要、状态合同和全部组件。
`internal/release` 有界下载、核对归档再解包；只接纳清单声明的普通文件，写入并同步规定权限。
安装本地暂存以复制完成，不依赖暂存目录与安装目录在同一文件系统。

`internal/installation` 在独占安装锁下检查完整源修订并执行 `current` 切换。
先持久写入原/目标目录与操作身份，再原子替换相对链接，最后提交实际观察与新修订。
恢复读取原意图和实际链接，不重做安装命令；无法归属的目录变化报告不可核验。
辅助文件外部修改会推进观察修订；恢复原发行也产生新修订，旧请求不能重新有效。
每次切换及恢复要求匹配当前安装、操作与 owner 的持久启动封闭。

这些原语已装配到独立 worker 和标准安装入口；不会将原语测试等同于完整在线升级验收。

### 持久任务与执行状态约束

`internal/upgradejob` 用独立 SQLite 库持久接纳、冻结请求/清单和进度。原键去重先于源前置条件；
拒绝也持久保存。部分唯一索引保证只有一个活动升级，恢复 owner 和操作修订同时约束每次写入。
`succeeded` 需要完整、当前尝试的证明；平台拒绝、路由失败、旧进程、旧清单、旧挑战及超时
均不能提交成功。回滚必须另建尝试，且保留原失败原因；迟到的目标确认不能结束回滚。

终态确认和封闭清理是有序步骤：终态已落盘但封闭未清理的任务仍占活动位置，恢复可以只完成
清理，不能重新升级。历史保留最新 128 个终态及至少 24 小时详情，随后保留原键/摘要墓碑。
总提交证据最多 4096 条，不回收键容量重新执行；满额时明确拒绝继续接纳。活动和受阻恢复不回收。
状态机默认总期限 10 分钟，worker 还需落实每阶段和回滚期限。

### 原机器控制与正常路由核验

安装配置记录原宿主控制端点；机器凭据只允许解析宿主获准发行、核验原 binding 和发起只读探测。
探测通过现有 Gateway 授权及路由，Runner 检查持久任务、当前尝试、真实封闭、完整安装和内核映像。
宿主收到匹配连接代次的响应后才添加接纳和路由证明；执行方提交终态仍是独立步骤。
只读安装观察不竞争 worker 的安装锁，发现未记录物理变化明确拒绝；只读任务库不能创建或写入。

`TestWorkerControlConfirmsActualImageThroughNormalGatewayRoute` 已通过本机真实 Gateway 协议往返，
包含错误尝试、缺失组件及“取得证明尚未成功”的断言。授权/配置/Host/WebApp 回归通过。
此证据未覆盖多个 Gateway 或完整服务切换；worker 和原生服务管理器的证据单独列出。

### Worker 执行与回滚

`internal/runnerupgrade` 已贯通持久任务、下载、源/目标检查、安装切换、重启、路由证明及终态提交，
CLI `upgrade-worker --root …` 由独立进程运行。默认总升级期限 10 分钟、下载 2 分钟、预检 20 秒、
服务重启 30 秒、目标尝试 90 秒、回滚 2 分钟；恢复不延长原尝试预算。

收到路由证明后再核对实际安装修订和配置，才提交终态。回滚另建尝试并复核当前共享状态；
原发行文件恢复与平台恢复分别证明，原先缺失的辅助文件按原观察保留，不能伪称完整目标发行。
恢复经过已提交切换时不重复切换；原预期进程已路由可见时不再次重启。
已确认但未解封的任务只完成清理；回滚不可确认时保留封闭和材料，不无限重试。

显式 `upgrade-recover` 用原操作及当前修订重新给予一次有界回滚预算，旧修订重复调用被拒绝。
worker 初始化失败会持久报告切换前失败或切换后恢复受阻；终态清理不依赖控制端可用。
解封后清理本次非当前发行和中断归档，定期回收已收敛材料及历史；预览只保留一个串行缓存。
同一 Dune 摘要的辅助组件更新额外要求启动时固定的尝试和选中目录证据，不能把升级前的其他
重启当作本次重启。相同摘要的回滚也必须匹配新的回滚尝试。

`go test ./internal/runnerupgrade -count=1` 已通过可控副作用测试：成功、平台注册拒绝后回滚、
切换/重启/确认边界中断、下载期间辅助组件变化、接收证明后安装变化，以及回滚无法确认。
这些测试使用真实文件/SQLite/封闭和可控平台、服务效果；完整原生发行升级进程验收仍需补齐。

### 标准安装和独立服务

初次安装记录完整本地发行、安装身份和原配置，创建 `current` 相对指针、保留恢复发行及两个独立服务。
连接器服务只执行稳定 `current/dune`；恢复服务执行 `recovery/dune upgrade-worker`，由系统持续守护。
managed bootstrap 也走该安装器；当前基线要求 systemd 用户实例或 launchd 用户域，拒绝裸 nohup 降级。
删除旧本地 repair/upgrade 的独立切换和清理实现，避免绕过持久任务及安装修订。
`repair-services` 仅恢复完全匹配的原服务定义和启动状态，不改发行、不接纳新任务、不清除封闭。

`DUNE_TEST_SERVICE_MANAGER=1 go test ./internal/service -run 'Test(NativeService|ServiceDefinitions)' -count=1 -timeout=90s -v`
已在 macOS arm64 通过：连接器 PID 67708→67713，独立 worker 保持 67710；SIGKILL worker 后恢复为 67714。
临时 LaunchAgent 已卸载并删除。该测试验证原生管理器隔离与重启，不等于完整真实发行在线升级验收。
本地发行权限标准化、安装别名拒绝、bootstrap、安装事务和 worker 回归已通过。

### 公开 API 与宿主持久观察

Go `RunnerUpgrader()`、授权 HTTP 五个动作和 `pkg/client` / SDK 已接通同一 Runner 协议。
执行端持久接纳冻结原配置身份；宿主先保存提交再派发一次，重复请求只查询。
新数据库 schema 直接加入 `dune_upgrade_observations`，没有迁移分支；可通过公开
`upgrade.ObservationStore` 注入替代实现。旧修订不能覆盖新观察，已确认终态不能改写。

正常 Gateway 路由的公开检查、完整 `already_current`、持久 queued 接纳、按原键查询及历史、
断线后的最近观察、当前权限拒绝均已测试。SQLite 重开、并发保留原键、终态防倒退和未知提交
分页已通过；PostgreSQL 用例因环境未配置而跳过。HTTP 五个动作及接纳未知的原键保留已通过。
接口和配置见 [公开 API](runner-upgrade-api.md)。

## 后续装配决策

宿主配置 `upgrade.Source`，负责从固定发行引用解析获准的不可变清单。Runner/worker 使用
安装时固定的宿主控制端点及原机器凭据；公开提交只接纳发行引用，不能让客户端注入下载命令
或绕过宿主发行策略。执行前再向宿主核验原有效 binding。

平台确认由 worker 请求宿主完成。宿主验证机器身份、原 binding 和当前操作后，通过正常
Gateway 路由向实际 Runner 发起只读探测，再把绑定本次挑战、操作、尝试、进程及连接代次的
证据返回 worker。worker 核对并持久提交结果；网页生命周期不参与确认。多宿主共用持久
观察存储，无法接通时只返回最近事实及其时间，不推断成功。升级和回滚使用不同尝试。

## 实际验证证据

| 场景 | 命令与证据 | 范围 |
| --- | --- | --- |
| U07/U15 部分 | `go test ./internal/launchgate -count=1`：杀死真实持锁子进程，在恢复前拒绝新启动；陈旧 owner 被拒绝 | 本机文件锁/持久记录，不是服务管理器验收 |
| U07/U15 部分 | `go test ./pkg/fabricd -run '^TestUpgradeSealRejectsLaunchAcrossConnectorRestart$' -count=1`：真实 connector 及 Gateway 路由返回持久拒绝，connector 重启、解除封闭后原键仍被拒绝；无启动副作用 | 本机真实进程；此用例未启动升级 worker |
| U01 | `TestInspectPinsActualExecutingImageAcrossPathReplacement`、`TestMachineInfoReportsOriginalImageAfterInstallationReplacement`：替换及删除原程序路径，内核运行摘要和启动身份不变 | 本机 macOS arm64 真实进程，后者经过 Gateway |
| U06 部分 | `TestUpgradePreflightRejectsPendingHostWithDifferentWriteSemantics`、`TestSharedContractRefusesReopenWithoutChangingAcceptedEvidence`：不同合同在预检/打开数据库时被拒绝，提交证据不丢失 | 待启动宿主及当前 schema，无历史迁移 |
| 受影响回归 | `go test ./internal/sessionregistry ./internal/launchgate ./internal/runningprogram ./internal/retainedprogram ./pkg/fabricd -count=1 -timeout=900s` 全部通过 | 包含本机真实宿主与可控 Agent；不等于厂商 Agent 或原生服务管理器 |
| U02/U03/U05/U13 部分 | `go test ./internal/release ./internal/installation ./pkg/upgrade -count=1`：完整组件核验、非法归档、源变化、回滚修订、原先缺失组件保留；切换意图/链接替换/记录提交边界恢复 | 本机文件与状态测试；跨文件系统及服务管理器待验收 |
| U02/U03/U04/U10/U11/U12/U16/U17/U18/U19 部分 | `go test -race ./internal/upgradejob -count=1 -timeout=120s`：跨数据库连接并发去重、修订冲突、重开后原键查询、证明不全不能成功、恢复 fencing、回滚迟到确认、分页与过期键不重放 | SQLite/状态机测试；未替代真实平台拒绝或 HTTP/worker 整链 |
| 静态/竞争检查 | `go test -race ./internal/launchgate ./internal/installation ./internal/release ./internal/upgradejob ./pkg/upgrade -count=1 -timeout=180s`；`go vet ./internal/launchgate ./internal/runningprogram ./internal/installation ./internal/release ./internal/upgradejob ./pkg/upgrade` 通过 | 当前新增原语 |

U01–U19 的完整闭环、原生 systemd/launchd、四平台运行和真实厂商 Agent 尚未验收。

managed 恢复入口按原 bootstrap 身份调用安装内 `recovery/dune repair-services`，恢复连接器和 worker 的原注册服务。安装根目录使用 `/var/tmp/dune-managed`，避免依赖重启时通常被清空的 `/tmp`；不迁移旧 bootstrap。
