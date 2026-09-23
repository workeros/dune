# Runner 在线升级实现与验收记录

目标合同：[Runner 升级需求](../../SandDance/docs/dune-runner-upgrade-requirements.md)。
本记录只描述 Runner 在线升级。当前合同的 API、独立 worker、平台核验和自动回滚已实现；下文列出 U01–U19 的测试证据及平台验证边界。

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
总期限 10 分钟，下载 2 分钟、预检 20 秒、重启 30 秒、目标确认 90 秒、回滚 2 分钟。到达总期限后不再开始新的安装切换。

### 原机器控制与正常路由核验

安装配置记录原宿主控制端点；机器凭据只允许解析宿主获准发行、核验原 binding 和发起只读探测。
探测通过现有 Gateway 授权及路由，Runner 检查持久任务、当前尝试、真实封闭、完整安装和内核映像。
宿主收到匹配连接代次的响应后才添加接纳和路由证明；执行方提交终态仍是独立步骤。
只读安装观察不竞争 worker 的安装锁，发现未记录物理变化明确拒绝；只读任务库不能创建或写入。

`TestWorkerControlConfirmsActualImageThroughNormalGatewayRoute` 已通过本机真实 Gateway 协议往返，
包含错误尝试、缺失组件及“取得证明尚未成功”的断言。授权/配置/Host/WebApp 回归通过。
此用例只覆盖单个 Gateway 的协议边界；完整服务切换由下文原生整链验收覆盖。

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
这些用例使用真实文件/SQLite/封闭和可控平台、服务效果；原生发行进程验收另列，不混算证据。
`TestWorkerSIGKILLAtDurableBoundaries` 另在下载完成、预检封闭、切换完成、重启完成、收到证明、
终态提交及解封前杀死实际 worker 测试子进程；不执行 Go 清理函数。恢复只执行一次所需重启。
这组测试的服务与平台效果可控，文件、SQLite、安装和封闭使用实际实现。

### 标准安装和独立服务

初次安装记录完整本地发行、安装身份和原配置，创建 `current` 相对指针、保留恢复发行及两个独立服务。
连接器服务只执行稳定 `current/dune`；恢复服务执行 `recovery/dune upgrade-worker`，由系统持续守护。
managed bootstrap 也走该安装器；当前基线要求 systemd 用户实例或 launchd 用户域，拒绝裸 nohup 降级。
删除旧本地 repair/upgrade 的独立切换和清理实现，避免绕过持久任务及安装修订。
`repair-services` 仅恢复完全匹配的原服务定义和启动状态，不改发行、不接纳新任务、不清除封闭。

`DUNE_TEST_SERVICE_MANAGER=1 go test ./internal/service -run 'Test(NativeService|ServiceDefinitions)' -count=1 -timeout=90s -v`
已在 macOS arm64 通过：连接器 PID 67708→67713，独立 worker 保持 67710；SIGKILL worker 后恢复为 67714。
临时 LaunchAgent 已卸载并删除。该测试验证原生管理器隔离与重启，不等于完整真实发行在线升级验收。
本地发行权限标准化、安装别名拒绝、bootstrap、安装事务和 worker 回归已通过。完整 Dune 原生升级现已由下面的独立用例执行。

### 公开 API 与宿主持久观察

Go `RunnerUpgrader()`、授权 HTTP 五个动作和 `pkg/client` / SDK 已接通同一 Runner 协议。
执行端持久接纳冻结原配置身份；宿主先保存提交再派发一次，重复请求只查询。
新数据库 schema 直接加入 `dune_upgrade_observations`，没有迁移分支；可通过公开
`upgrade.ObservationStore` 注入替代实现。旧修订不能覆盖新观察，已确认终态不能改写。

正常 Gateway 路由的公开检查、完整 `already_current`、持久 queued 接纳、按原键查询及历史、
断线后的最近观察、当前权限拒绝均已测试。SQLite 重开、并发保留原键、终态防倒退和未知提交
分页已通过；本次另启动隔离 PostgreSQL，执行了元数据回归及跨宿主升级整链。HTTP 五个动作及接纳未知的原键保留已通过。
接口和配置见 [公开 API](runner-upgrade-api.md)。

## 宿主接入与发行

宿主配置 `upgrade.Source`，负责从固定发行引用解析获准的不可变清单。`upgrade.NewCatalog` / `ReadCatalog` 提供内置不可变目录；CLI 使用 `web --upgrade-catalog`。打包脚本生成归档、完整清单、引用和目录，详见 [发行说明](releases.md)。Runner/worker 使用
安装时固定的宿主控制端点及原机器凭据；公开提交只接纳发行引用，不能让客户端注入下载命令
或绕过宿主发行策略。执行前再向宿主核验原有效 binding。

平台确认由 worker 请求宿主完成。宿主验证机器身份、原 binding 和当前操作后，通过正常
Gateway 路由向实际 Runner 发起只读探测，再把绑定本次挑战、操作、尝试、进程及连接代次的
证据返回 worker。worker 核对并持久提交结果；网页生命周期不参与确认。多宿主共用持久
观察存储，无法接通时只返回最近事实及其时间，不推断成功。升级和回滚使用不同尝试。

## U01–U19 证据索引

2026-09-23 复核修正：

- U18/U19：`TestWorkerRetainsUnexpectedlySelectedDownload` 覆盖下载完成和中断恢复时
  current 被改指候选；`TestWorkerChecksSelectionBeforeTerminalRelease` 覆盖确认前、
  确认后、解封前及重新打开任务；`TestMaintenanceRetainsUnexpectedlySelectedMaterials`
  验证维护不删除实际选中的发行。安装与 worker 包的普通/race 回归通过，配置丢失后的
  正常终态清理仍通过。这些故障注入使用真实文件、任务数据库和启动封闭，服务/平台效果模拟。
- U04/U16：`TestRunnerUpgradeLiveHistoryFindsUndispatchedSubmissions` 通过真实 Gateway
  路由发现 Reserve 后未派发的 unknown；`TestRunnerUpgradeHistoryKeepsPaginationAcrossAdmissionAndDisconnect`
  验证后续接纳去重及在线到离线的同游标续页。分页单测覆盖同时间不同提交、字节截断及
  独立活动任务；宿主观察存储在 SQLite 和隔离 PostgreSQL 的 race 回归通过。
- 本轮直接更新内部提交协议与游标格式，应用层 Go/HTTP 提交参数不变。原生服务管理器、
  整机断电及真实 Agent 未在本轮重新执行，不扩大为新的平台验收结论。

本轮 `make test`（含 IM）、`make check-go web-check web-build` 均通过；相关 race 命令为
`go test -race ./internal/installation ./internal/runnerupgrade -count=1`、
`go test -race ./pkg/upgrade ./pkg/access ./internal/upgradejob ./pkg/host -count=1 -timeout=180s`，
以及连接专用临时 PostgreSQL 的 `go test -race ./internal/metadata -run Upgrade -count=1 -v`。
临时 PostgreSQL 已停止并清理。SandDance 使用临时 modfile 指向本次 Dune 源码，应用包测试
通过；整仓 `go test ./...` 被其 `artifacts/acp-host-startup-3f7ac74/release-harness-e2e_test.go`
跨模块导入 Dune internal 包阻断，未修改该验收材料。

各行对应需求断言。原语故障注入、真实进程及原生服务管理器是不同层次的证据；
不是每个排列组合都在所有操作系统原生执行。测试源码随本次实现提交。

| 编号 | 已执行的主要验证 | 证据层次 |
| --- | --- | --- |
| U01 | 替换、删除磁盘程序后，内核映像摘要和启动身份保持；通过 Gateway 查询实际连接器 | `internal/runningprogram`、fabricd 真实子进程 |
| U02 | Dune/tmux/rg/许可证逐项核验；完整已就绪返回 `already_current`，旧目录的同 SHA 进程仍需重启 | release、upgradejob；标准安装原生无操作及同 SHA 发行升级 |
| U03 | 同键去重、异请求冲突、并发只有一个活动任务；接纳/下载后源修订变化被拒绝，回滚修订不复用 | upgradejob、installation、worker 屏障与 race |
| U04 | 宿主先持久保留原键，响应丢失后只查询；仅 Reserve 未派发的提交在在线历史中仍可发现且保持 unknown | 公开 Go/HTTP、宿主观察存储及原生任务重查；在线混合分页回归 |
| U05 | 下载/摘要/非法归档/候选核验失败，未切换源安装 | release、runnerupgrade 故障注入 |
| U06 | 源/目标/旧宿主/待启动宿主合同不符即拒绝，拒绝前后接纳记录不变 | fabricd 预检、sessionregistry、worker |
| U07 | 启动竞争进入同一闸门；已接纳待启动宿主被枚举；worker 死后新键持久拒绝 | 真实启动与原生升级 |
| U08 | v1 宿主和 PTY 跨 v2→v3 成功；待启动 v1 宿主在成功后进入并读写；后续重启仍保留约束 | LaunchAgent、真实 ACP 宿主、可控 Agent |
| U09 | connector/worker 是独立服务；重启 connector 不清理 worker，杀 worker 后原服务恢复 | macOS arm64 原生 launchd |
| U10 | WebSocket 已建立、目标程序已运行，Gateway 拒绝发布路由；始终不成功并自动恢复 v2 | 原生整链，专门复现注册拒绝 |
| U11 | 已取得实际路由证明，但确认响应持续不可交付；超时后回滚并保留错误原因 | 原生整链；路由不可达另有协议/worker 测试 |
| U12 | HTTP 入口和 Runner owner 分属两个 Gateway，经 mTLS peer 路由确认；旧尝试/回滚后的迟到证明被拒绝 | PostgreSQL 原生整链、证明状态机 |
| U13 | HFS+ 发行源→APFS 标准安装、升级及回滚；同 Dune SHA 许可证更新、组件缺失/部分切换恢复 | 跨盘原生整链、release/installation 故障注入 |
| U14 | v3 运行时旧 v1 宿主完成权限响应并新增 prompt；回滚 v2 后两份新记录可查询；待启动 v1 仍可进入 | 原生整链；恢复受阻/失败另有 worker 测试 |
| U15 | 七个持久边界 SIGKILL；原生目标在线时杀 worker 并延迟接管，新启动持久拒绝，恢复不重复重启 | 实际 worker 测试子进程、原生服务整链、owner fencing |
| U16 | SIGKILL 发起 HTTP 提交的 host，重开 PostgreSQL 连接后找回原任务；执行端确认不改变历史位置，断线后可用同一游标续页 | 两宿主进程原生整链、公开 API 在线/离线分页、宿主持久观察 |
| U17 | 后续升级不改写旧任务结果和原证明；当前检查独立于历史；未知任务不借当前版本补记成功 | 原生连续升级、宿主存储/worker 状态机 |
| U18 | 当前权限、原 binding/安装身份、修订与过期键分别校验；离线缓存不绕过权限；实际 current 与记录不一致时拒绝终结和解封 | 授权 API、元数据、installation/upgradejob、worker 指针异常回归 |
| U19 | 重开持久记录恢复原阶段；确认后解封/清理中断可继续；下载恢复和维护清理先核验实际 current；异常时保留封闭及恢复材料 | worker、installation、upgradejob；未执行整机断电 |

### 原生 macOS arm64 整链

```sh
DUNE_TEST_SERVICE_MANAGER=1 go test ./pkg/host \
  -run '^TestNativeRunnerOnlineUpgrade$' -count=1 -timeout=480s -v
```

可组合设置 `DUNE_TEST_RELEASE_SOURCE_DIR` 指向另一个文件系统的私有测试目录，
以及 `DUNE_TEST_POSTGRES` 指向专用测试库。测试创建、删除随机 schema，不使用业务库。
未配置 PostgreSQL 时使用 SQLite 和单宿主；配置后经宿主 B 的真实授权 HTTP API 发起升级，
由宿主 A 持有 Runner，正常确认通过 B→A 的 mTLS peer 路由。确认前杀死宿主 B 后重启，
同时重开 A 的装配，原任务、身份、证明规则及后续查询保持。

测试构建当前源码的 v1/v2/v3 和与 v3 Dune SHA 相同的 v3-notices，标准 managed 安装创建
真实 LaunchAgent 连接器及独立 worker。仅 v1 在构建时使用临时 Go overlay 加入“宿主进入前等待”
屏障，产品源码及发行不包含该钩子；其余安装、worker、宿主和 Agent 生命周期均执行实际实现。
两个原 v1 宿主在接纳后暂停，分别跨失败回滚和 v3 成功后继续进入；另一个 v1 宿主始终存活。

完整序列包含：无操作提交及原键重查、v1→v2 成功、v2→v3 注册拒绝回滚、v2→v3 成功、
同 SHA 辅助发行确认超时回滚，以及杀 worker/host 后恢复并成功。存活 v1 宿主始终只有
一次 initialize、一次 session/new、11 次 prompt、5 次 cancel；原 PTY shell PID 和 Runtime 身份不变。
目标运行时写入的新权限响应和任务记录在回滚后仍可查，没有恢复旧快照。
新启动在 worker 空窗持久拒绝，解封后旧键仍被拒绝；历史任务保留各自原证明。

2026-09-23，跨文件系统的 SQLite/单宿主运行通过，耗时 **231.73 秒**。
同日 PostgreSQL/双 Gateway/真实 host 中断组合通过，耗时 **232.96 秒**：

- 源 PID 99505 → v2 99558；Gateway 拒绝后回滚到 2889；随后 v3 3160。
- 同 SHA 辅助发行确认超时后回滚到 6297；下次目标 PID 6370。
- worker 99509、发起请求的 host 99495 被 SIGKILL；恢复后目标仍为 6370，连接代次 1→2。
- 保留宿主 99540、Agent 99542；原待启动 v1 宿主 99527、99535 分别按原接纳继续进入。
- 发行源和安装文件系统设备号为 16777237 / 16777230。配置字节保持不变。

`TestNativeRunnerUpgradePreview` 在双 Gateway/PostgreSQL 下另行通过，耗时 **34.20 秒**：
公开 HTTP 预览实际执行源/候选的 `upgrade-check`，包含一个存活及两个待启动 v1 宿主，
安装修订、连接器启动身份、启动封闭和升级历史均保持不变。

测试使用可控 ACP Agent，不调用厂商模型。测试服务、tmux、子进程及临时目录已由夹具清理；
验收使用的磁盘映像已卸载删除，隔离 PostgreSQL 已关闭并删除。日志保留于 `.local/runner-upgrade-acceptance/`。

### 回归与构建

- `DUNE_TEST_POSTGRES=… make test`：最终全量通过，主 Go module 和 IM module 均通过；
  fabricd 196.80 秒、跨进程 tests 255.85 秒。PostgreSQL 使用本次创建的隔离实例。
  前两轮分别暴露 Gateway 回调和 ACP 拒绝响应与测试发送完成的时序竞争；
  测试改为显式屏障或读取原拒绝响应，不重放握手。Gateway 连续 5 次包级 race、ACP 控制用例连续 200 次 race 通过。
- `make check-go web-check web-build`：通过，Web 打包只有体积建议警告。
- worker、状态机、公开 API 与发行目录的定向 `go test -race` 通过；匹配不到用例的包不计作 race 验收。
- `TestPublishedArchiveMatchesGoManifestAndDownloader` 验证 Python 发布清单摘要与 Go 一致，真实下载器完整展开归档。
- 四平台 `CGO_ENABLED=0 go build ./cmd/dune` 通过；从提交 `e121f78` 的源码归档构建，
  不包含工作区其他未提交修改。产物和 SHA-256 记录位于 `.local/runner-upgrade-builds/`。
- SandDance 用临时 modfile 将 Dune / IM 替换为本地 main 后，cmd、internal、provider 业务包通过。
  `go test ./...` 总命令因其 `artifacts/acp-host-startup-3f7ac74` 历史测试越界导入 Dune `internal/config` 而失败；
  未修改其 go.mod 或该历史测试。

### 平台与交付边界

已原生验证 macOS arm64、launchd、SQLite/PostgreSQL、同机双 Gateway、真实 host/worker 中断、
跨文件系统标准安装及当前合同内的多代宿主。Linux 的 systemd 和其他架构仅有构建及服务定义测试，
未在本次环境执行原生验收；四平台编译不能替代它们。同机多进程也不等于跨主机网络故障验收。
未执行整机断电或厂商 Agent 编码任务；不把它们记作通过。

Dune 的公开合同及发布工具已交付；发行 URL、发布和 SandDance 产品接入仍由宿主选择。
本次没有 push、发布远端发行或替换 SandDance 的现有升级 UI/执行器。
managed 恢复调用安装内 `recovery/dune repair-services`，安装根目录为 `/var/tmp/dune-managed`；
不迁移旧 bootstrap。接入方式见 [公开 API](runner-upgrade-api.md) 和 [发行说明](releases.md)。
