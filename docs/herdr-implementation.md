# herdr 借鉴方案实施记录

目标：[已确认方案](references/herdr-adoption-design.md)。逐功能实现、验证、提交，遵守 SOLID / KISS；不引入宿主队列协调系统，不保留原型旧合同的兼容层。

## 功能提交清单

- [x] 项目工作区共享后端：Owner / Tenant 项目、多个 Runner 目录、默认 Profile，数据库和 HTTP 合同；SandDance 外层路由随产品接入提交。
- [x] 个人工作现场共享后端：分屏树、焦点、审阅目标、已读位置，数据库持久化和并发更新检查；页面接入随两产品工作台提交。
- [x] Agent 摘要基础：Runtime 列表直接携带 ACP 活动与 PTY 前台信息，活动与进程状态分离，无需打开内容订阅。
- [x] PTY 画面活动适配：Claude / Codex 明确的工作 / 空闲 / 回应控件；未知画面保持 unknown，原生 ID 采集独立实现。
- [x] PTY 原生身份采集：直接启动的 Claude / Codex 注入 SessionStart，tmux 保留确认并接入数据库索引；厂商交互验收仍待完成。
- [x] PTY 原生回调收集器：绑定 Runtime、SessionStart 过滤、持久确认序号和有界静默子进程；启动接入另行完成。
- [x] Dune 并行工作台：项目和 Agent 导航、跨 Runner 分屏、焦点审阅联动、布局恢复。
- [x] SandDance 并行工作台：接入共享合同，Tenant 内自由分屏与个人布局。
- [x] worktree 准备能力：SDK / Gateway / fabricd 新建和列出 worktree，复用 Git 仓库锁；不覆盖目录 / 分支，不复制未提交内容。
- [x] 启动位置：两产品使用已有就绪 Runner，当前目录 / worktree 选择、项目默认配置。
- [x] 统一启动服务：固定配置解析、宿主环境默认值、当前目录 / 新 worktree、启动前快照与部分结果保留。
- [x] 恢复索引数据库：不可变实际配置、原生 ID 绑定、最近启动 attempt、跨连接继续去重与未知结果屏障。
- [x] 原生会话切换索引：Runtime 当前关联、分离进程 / 原生 cwd、迟到确认只补历史、重复 load 去重。
- [x] managed ACP 原生会话编排：启动自动 new、共享 new/load 入口、操作引用与确认采集。
- [x] managed ACP 继续后端：实际启动快照、显式 load、数据库 claim、并发去重与未知结果屏障。
- [x] 两产品 ACP 继续入口：保存 pane 显式恢复、并发 attempt 跟随、未知结果不重放，保持其他 pane 连接。
- [x] PTY 原生恢复后端：快照、精确 UUID / cwd、数据库去重、等待原生确认与失败重试；厂商验收另行完成。
- [ ] 两产品 PTY 继续入口：原生等待期间可打开终端，保留未确认状态。
- [x] fabricd ACP 操作：Runtime 串行队列、操作引用、wait / read、有界输出与失效语义。
- [x] fabricd PTY 投递：人工按键与文本 / Enter 统一排序、目标检查与投递状态。
- [x] Tenant / Owner Agent 发现服务：分页、固定 Agent 引用、SDK / Gateway 读取与原生确认采集；两产品已挂载共享 HTTP 路由。
- [x] 两产品共享 Agent 发现列表：按 Runner 分页、部分失败保留、拒绝访问断开、原生切换更新布局索引且不重连。
- [x] Tenant Agent 服务：发现、启动、投递、操作 / 活动等待、ACP 输出 / PTY 快照读取，复用 SDK / Gateway 路由。
- [x] MCP 会话凭据数据库：调用方与启动 attempt 绑定、哈希、轮换、撤销、过期及跨连接校验。
- [x] MCP HTTP 接入：九项工具、无状态协议请求、凭据与活 Runtime 校验、Tenant 隔离和部分结果保留。
- [x] ACP MCP 配置诊断脱敏：inspector 请求/结构化响应和操作错误隐藏连接配置，原生 RPC 保留真实值。
- [x] 原生 stdio MCP bridge：内部入口、环境凭据、官方协议转发、断线不重放和退出清理。
- [x] fabricd MCP 配置：首次原生会话前固定配置、能力选择 HTTP/stdio、所有 managed new/load 复用与启动门禁。
- [x] MCP 凭据原文回显脱敏：ACP 操作/错误和 stderr 字节流，覆盖 OS 分块边界。
- [x] managed ACP 自动 MCP 注入：确认 Runtime 后签发、新建/恢复前配置，原生切换复用，恢复轮换。
- [ ] MCP 注入后的运行凭据脱敏验收：实际厂商日志和其他输出位置。
- [ ] MCP 注入：managed ACP 配置、受支持 PTY 适配器与必要的 stdio bridge。
- [x] 两产品协作交互：共享 ACP new/load/prompt、pending、按操作查询和读取、输出不完整与引用失效提示。
- [ ] 集成验收：双入口同队列、宿主 / fabricd 重启、A / B 输出关联、恢复旧配置及真实 Agent 互操作。

清单按产品交付顺序排列；底层依赖可先实现。一个功能涉及两仓库时各自提交，记录对应提交与验证。未通过的外部验收明确记录，不以 mock 或文档检查代替。

## 验证记录

- 2026-09-17：实施开始。Dune 初始 HEAD `40d2c42`，SandDance 核对 HEAD `6a8ba0e`。工作区内已有本任务设计文档，先单独提交设计基线 `7ddee55`。
- 项目工作区：使用独立临时 PostgreSQL，`go test -race ./internal/metadata ./internal/webapp ./pkg/host ./pkg/workbench -count=1` 通过（同时覆盖 SQLite）。验证跨 Owner 隔离、Tenant 共享、两个数据库连接竞争修订、数据库重开、过期绑定拒绝、删除不影响 Runner。相同包的 `go vet` 和 `git diff --check` 通过。HTTP 合同见 [工作台 API](workbench-api.md)。
- 个人工作现场：`go test ./internal/metadata ./internal/webapp ./pkg/workbench -count=1` 通过；两个新增场景 `TestPersonalViewsAndReadMarkers`、`TestViewHTTPKeepsPersonalScopeAndValidatesTree` 的 race 检查通过。均配置独立临时 PostgreSQL，同时覆盖 SQLite；验证两个连接竞争修订、用户 / 身份命名空间隔离、跨数据库重开、已读不回退、事件 epoch / 执行实例隔离、保留失效 pane、重复控制 pane 拒绝。相关包 `go vet` 与 diff 检查通过。
- 已提交：项目后端 `ce9bf17`，个人工作现场后端 `6641f48`；SandDance 外层路由 `0af9a0a`。SandDance 使用临时 Go workspace 关联当前 Dune 和 IM 后，`go test ./... -count=1 -timeout=180s` 通过；最终远端 Dune 依赖更新仍待交付阶段处理。
- Agent 摘要基础：`go test ./pkg/fabricd ./pkg/api ./internal/tmux ./pkg/client ./pkg/sdk ./internal/webapp ./pkg/host -count=1 -timeout=180s` 通过。ACP 权限 / 完成状态、PTY 不猜测任务状态、前台进程发现的定向 race 检查和相关包 vet 通过；ACP 使用可控协议对端，PTY 前台读取使用真实本地 tmux。尚未验证真实 Agent 状态 hook。
- Dune 并行工作台：类型检查、前端单元测试及生产构建通过；七项 Chromium 交互用例覆盖四个混合会话跨项目 / Runner 分屏、稳定连接、输入目标、独立浏览器上下文恢复、固定 / 跟随审阅、过期绑定拒绝、保存冲突与迟到响应、多 Runner 项目、摘要失败及窄屏焦点。检查了 1600px / 600px 画面与长项目名。测试使用 HTTP / WebSocket 协议对端，数据库持久化另由上述集成测试覆盖；尚未据此宣称真实 Agent、MCP 或恢复验收完成。

- 两产品并行工作台提交：Dune `74514d4` / `8d06717`，SandDance `9ca8f0b`。SandDance 类型检查、139 项 Chromium 回归和显式 CDN 配置的生产构建通过；移动导航最终调整后相关五项交互用例再次通过。
- fabricd ACP 操作：新增 [操作合同](agent-operations.md)，所有 managed 生命周期 / prompt 共用 Runtime 队列，操作级 wait / read 与有界输出；IM 也改为按操作读取。定向 race 覆盖排队、输出关联、控制与关闭边界；多进程 SDK / Gateway 测试覆盖两个连接、提交方断线后查询。真实 MCP、原生恢复和双宿主 Pod 验证仍待整体验收。
- ACP 本轮检查：`go test ./pkg/fabricd ./pkg/api ./pkg/client ./pkg/sdk ./pkg/access ./pkg/host ./internal/webapp` 通过；新增队列用例的 race 和相关包 vet 通过；`go test ./tests -run 'TestAgentOperations|TestManagedACP'` 通过；IM module 全量 race / vet 通过；SandDance 使用本地 workspace 与独立 PostgreSQL 的全量 Go 测试通过。

- 2026-09-18：PTY 有序投递通过共享 tmux 输入客户端实现，浏览器按键与文本 / Enter 同队列；已有浏览器输入所有权保留。真实本地 tmux + 字节记录进程验证顺序、投递前置检查、队列上限、fabricd 重启后的旧操作失效。原生 Agent 状态 hook、厂商 CLI 与 MCP 仍待后续验证。
- PTY 检查：`go test ./internal/tmux ./pkg/fabricd` 通过；新增 PTY / ACP 队列与输出用例 race 通过；`go test ./tests -run 'TestPTY|TestTmux|TestTerminal|TestAgentOperations|TestManagedACP|TestInput'` 通过，覆盖 fabricd / Gateway 重连、原生历史和输入租约；相关包 vet 与 diff 检查通过。

- worktree 准备：新增 [API 合同](worktree-api.md)，定向 race 覆盖实际 Git 工作树、源目录脏文件保留、中文 / 空格路径、已有分支与目录拒绝、共享仓库并发创建；client / access 回归和相关包 vet 通过。`TestWorktreeCreationThroughGateway` 通过，覆盖 SDK 经 Gateway 到 fabricd 的真实 Git 创建与列出。两产品启动选择与实际配置快照仍随统一启动服务接入。

- 恢复索引数据库：新增 [存储合同](agent-recovery.md)。SQLite / PostgreSQL 测试覆盖不可变快照、Profile 修改和删除、数据库重开、Owner 隔离、两连接并发继续、旧 attempt 迟到上报、未知结果拒绝接替和提交回执丢失；定向 race 通过。identity / authorization / metadata / webapp / host 回归及相关 vet 通过。尚未接入启动服务，不代表真实原生恢复已验收。

- 统一启动服务：新增 [启动合同](agent-launch.md)，`App.AgentLauncher()` 通过 SDK / Gateway 启动。真实 tmux / Git 测试覆盖项目默认旧修订、合并环境与实际进程一致、cwd 覆盖、隔离工作树及脏文件保留、失效输入先拒绝、启动失败仍返回已建 worktree；定向 race 通过。fabricd / api / host / webapp 回归、相关 vet 和使用本地 workspace 的 SandDance 全量 Go 测试通过。页面尚未迁移到此入口，真实 Agent / MCP 验收仍待后续。

- 两产品启动入口：HTTP / UI 接入固定 Profile 修订和当前目录 / worktree 选择，保留部分结果，并从数据库会话摘要恢复项目关联。Dune 类型检查、前端单元测试、生产构建、十项浏览器场景通过（两个新场景修正测试定位器后重跑通过）；webapp / host Go 回归、HTTP / binding 定向 race 和 vet 通过。SandDance 全量 142 项浏览器测试、类型检查、生产构建、全量 Go 和相关 vet 通过；窄屏样式最终调整后三项相关浏览器场景及构建再次通过，已检查两产品宽 / 窄屏截图。SandDance 的真实本地 fabricd / Gateway / tmux 集成验证冻结后的环境进入 PTY / ACP 进程；ACP 使用 Python 协议夹具，不代表真实 AI Agent / MCP 验收。

- ACP 原生确认：成功 new/load 的操作及 Runtime 摘要保留不可变 ID / cwd / Agent 版本 / load 能力，确认序号防止迟到采集改变当前关联。fabricd / api / client / host 回归、新增确认与队列定向 race、相关 vet、IM duneagent race / vet、Gateway AgentOperations / ManagedACP 多进程用例通过；SandDance 使用本地 workspace / 独立 PostgreSQL 的全量 Go 回归通过。恢复索引采集与显式继续尚待接入。

- 原生会话切换索引：SQLite / PostgreSQL 及定向 race 验证两个连接重复采集、B 先于 A 入库、晚到记录只补历史、相同序号冲突整体回滚、旧 Runtime / Owner 隔离、索引和启动 attempt 原子提交、恢复时拒绝不同原生 ID / cwd、数据库重开。相关 identity / authorization / metadata / webapp / host 回归及 vet、SandDance 全量 Go 回归通过。两产品只用 selected 记录关联当前 Agent，类型检查、生产构建和各一项含历史记录干扰的浏览器回归通过；Dune 前端单元测试通过。宿主自动采集和实际继续尚未接入。

- Runner 发现分页修复：游标改用有界逻辑 Runner ID 校验，允许 SandDance 的 `runner_…` 标识，不再误用协议消息 ID 的十六进制约束。带前缀 ID 的授权扫描、跨数据库重开 / 跨连接续页、无权限项过滤及无效游标边界通过 SQLite / PostgreSQL 定向 race；metadata vet 通过。

- Agent 发现服务：[AgentDirectory](agent-directory.md) 复用 SDK / Gateway，自动采集可靠原生确认并保留历史。真实本地 fabricd + 协议夹具验证创建 / 切换采集、旧引用拒绝、退出后索引保留、无敏感配置泄露、跨 Tenant 拒绝、Runner 分页和部分故障；定向 race 通过。HTTP 鉴权与参数边界、identity / authorization / metadata / webapp / host / agentservice 回归和相关 vet 通过；SandDance 共享路由用例、全量 Go 回归和 app vet 通过。工作台发现列表尚未迁移此入口，MCP 工具与实际恢复继续推进。

- 工作台共享发现：Dune 类型检查、前端单元测试、十项原有浏览器场景、新增分页 / 原生切换 / 部分故障场景及生产构建通过。SandDance 类型检查通过；全量 143 项浏览器场景首次 141 项通过，新增场景的连接次数断言修正为等待 StrictMode 初始化完成，另一个文件编辑场景受 Monaco 开发错误浮层干扰；两类相关的七项场景重跑全部通过，生产构建通过。两端使用真实浏览器与 HTTP / WebSocket 夹具，不代表真实 Agent 互操作验收。

- ACP 原生目标补齐：prompt 同时固定 ID / cwd，入队和出队都拒绝目录变化；新增同 ID 切换目录场景和全部 ACPQueue race、api / client 回归、相关 vet 通过。

- 共享通信服务：[AgentMessenger](agent-messaging.md) 和两产品 HTTP 入口就绪，引用不绑定宿主实例，prompt 可选等待失败保留操作引用。agentservice / tmux / agents / api / client / host / webapp / fabricd 回归、通信与发现定向 race、相关 vet 全部通过；SandDance 使用本地 workspace / 独立 PostgreSQL 的全量 Go 回归及 app vet 通过。真实本地 Gateway/fabricd 验证两连接统一排队、输出隔离、原生切换、PTY 字节投递和跨 Tenant 拒绝；尚未验证多 Pod 故障及真实厂商 MCP 互操作。

- managed ACP 继续后端：真实本地 Gateway / fabricd 协议夹具覆盖冻结配置与 Profile 删除、setup 不重跑、双请求单次恢复、独立 cwd、无 list 的 load、失败 / unknown 分离、原生切换后重复请求拒绝及原 Runner / 存储校验。相关 Go 回归（含 SQLite / PostgreSQL）、恢复定向 race、vet 通过；SandDance 全量 Go 回归（本地 workspace / 独立 PostgreSQL）及 app vet 通过。页面与 PTY 原生恢复未据此计为完成。

- 两产品 ACP 继续界面：Dune 类型检查、4 项单元测试、生产构建和全部 14 项 Chromium 场景通过；SandDance 类型检查、生产构建和全部 146 项 Chromium 场景通过。新恢复场景覆盖显式提交、pending 共享确认和 unknown 不重发，宽 / 窄屏截图已检查。测试为浏览器协议夹具，厂商原生存储和多 Pod 验收仍待后续。

- 历史恢复迟到确认修复 `0f07034`：原 Runtime 切换出的历史记录被独立恢复后，旧观察不会重新选中它或误报索引故障。SQLite / PostgreSQL 定向 race 和 metadata vet 通过。

- managed ACP 原生会话编排：identity / authorization / metadata / agentservice / webapp / host 回归（配置独立 PostgreSQL）、新编排及恢复定向 race、相关 vet 通过。SandDance 使用本地 workspace 的全量 Go 与 app / integration vet 通过，实际 Gateway 集成夹具同步响应初始 new。新路由鉴权及部分结果保留已验证；两产品按钮迁移、MCP 注入、厂商 Agent 验收继续推进。

- MCP 凭据存储：SQLite / PostgreSQL 定向 race 通过；相关身份、授权、webapp、host 回归和 SandDance 全量 Go 通过。元数据全量首次暴露 schema 测试删表顺序未覆盖新外键，已调整快照表顺序并定向重跑初始化原子性；企业身份无需本地账号的用例也通过。HTTP / 实际注入尚未装配。

- MCP HTTP 工具接入：相关 Go 回归（独立 PostgreSQL）、MCP HTTP/Runtime 定向 race、相关 vet 通过；SandDance 全量 Go 与 app vet 通过。官方 MCP 客户端经两个独立 HTTP handler 轮流访问同一 Gateway/fabricd，完成发现、启动、投递、wait/read；鉴权撤销、退出、非法 Origin/cookie/query 和错误结果保留验证通过。真实多 Pod 与厂商 MCP 注入仍待后续。

- ACP MCP 诊断脱敏：fabricd / host 全量包回归、新增 inspector 原生 RPC 不变/URL/header/env/argv/结构化错误脱敏定向 race、fabricd vet 通过。生产注入与真实厂商日志仍待验证。

- stdio MCP bridge：Go/race 与 vet 通过；真实 HTTP MCP 服务和官方 stdio 客户端验证初始化后 context 释放不破坏连接、schema/call 转发、EOF/取消退出、断线仅投递一次、重定向不转发凭据。编译后的 Dune 程序在无机器配置的目录完成 stdio initialize/list/call，stdout/stderr 无测试凭据。尚未注入厂商 Agent。

- fabricd MCP 配置：api/access/client/fabricd/bridge 全量 Go 回归、配置/握手/并发/协议边界定向 race 和相关 vet 通过。验证 HTTP 能力解析、stdio argv/env、首次原生动作门禁、new/load 配置不变、并发唯一配置与公开状态/访问审计不包含凭据。宿主自动签发与生产启动接入另行提交。

- MCP 原文回显脱敏：fabricd/host 全量回归、定向 race 与 fabricd vet 通过；验证原生 RPC 保留真实 token、操作输出和纯文本错误遮盖 token，以及 stderr 在每个可能读取分界上的遮盖。初次测试发现 inspector 的 JSON 类型被转成字节数组，已修正并重跑，保持原有 JSON 展示合同。

- managed ACP 自动注入：身份/授权/metadata/agentservice/webapp/host 回归（独立 PostgreSQL）、注入/原生会话/恢复定向 race 与相关 vet 通过；SandDance 全量 Go（本地 workspace/独立 PostgreSQL）通过。本地 Agent 进程收到真实配置后分别通过 HTTP 和实际 stdio bridge 调用 `agents_list`，并在 load/恢复中重验；覆盖新 Runtime 先入索引、切换不轮换、恢复轮换、重复恢复不重发，以及配置拒绝/MCP 不可达保留 Runtime 和操作。未据此声称厂商 Agent 或真实多 Pod 验收完成。

- 两产品 ACP 操作交互：见 [界面合同](agent-operation-ui.md)。Dune 类型检查、4 项单元、生产构建、全量 16 项 Chromium 通过；最终 Runtime key / 输出输入校验 / 64 项上限调整后，恢复和操作场景分别重跑通过。SandDance 类型检查、生产构建通过；全量 148 项首次 145 项通过，两项导航测试因定位器竞态/多目标失败，一项文件搜索点击超时；修正导航测试并重跑操作、console、文件搜索的全部 42 项均通过。已检查两产品操作输出宽/窄截图。浏览器验证覆盖 A idle 不满足 B、共享新建/加载、unknown 保留引用且不重发、读取位置推进、缺口和失效提示；仍非真实厂商互操作证据。

- PTY 画面活动：新增 [状态合同](pty-agent-state.md)。agentdetect / tmux / fabricd 全量 Go 回归、画面 / 输入准入定向 race、相关 vet，以及跨进程 PTY / tmux / terminal / input 回归通过。使用真实 tmux 与可控字节进程验证实时屏幕不受历史浏览影响、旧授权和草稿不污染状态、提交前重新检查 blocked；未据此声称厂商 CLI 界面已验收。跨进程回归发现两处仍引用已删除的 `tmux.Capture`，已独立修正为共享 `api.TerminalSnapshot`，无兼容别名。

- PTY 原生回调收集器：agentintegration / fabricd / cmd/dune Go 回归、确认并发与实际回调子进程定向 race、相关 vet 通过。验证跨读取保留序号、重复与切换、错误绑定、子 Agent 过滤、损坏不重置、已删除目录不重建、私有字段不保存，以及输入管道不关闭时一秒退出。初次并发检查发现同时创建锁文件的竞态，改为启动前独占创建锁文件；初次管道检查发现关闭 stdin 不能可靠中断阻塞读取，改为有界等待后退出回调进程，两项均已重跑通过。尚未自动注入厂商 CLI。

- PTY 原生身份接入：直接 Claude / Codex 启动使用本次 hook 参数和环境，保留 helper 可执行文件，tmux 元数据恢复集成标记；SDK / Gateway 的 Runtime 摘要接入共享数据库索引。相关 agentintegration / tmux / agentservice / api / client / fabricd / host Go 回归通过（独立 PostgreSQL）；注入、离线切换、队列目标与并发清理定向 race / vet 通过，host 索引定向 race 通过。跨进程 PTY / tmux / terminal / input / AgentOperations 通过；SandDance 使用本地 workspace / 独立 PostgreSQL 的全量 Go 通过。验证了 A 到 B 切换保留历史、旧引用失效、ID / cwd 改变拒绝排队输入，以及粘贴后切换返回 unknown、不发送 Enter。安装的 Codex 0.140 接受生成的内联 hook 配置；厂商 CLI 回调、实际恢复及 MCP 尚未计为验收完成。

- PTY 原生继续后端：相关身份 / 授权 / metadata / agentintegration / agentservice / webapp / host 全量 Go（独立 PostgreSQL）通过，PTY / ACP 恢复定向 race 和相关 vet 通过；SandDance 全量 Go（本地 workspace / 独立 PostgreSQL）通过。验证 Claude / Codex 精确 resume 参数、原 Profile 修改 / 删除后仍使用旧配置、setup 一次、并发一次启动、错误或缺失原生确认、等待中的 Runtime 不重放、稍后退出后显式重试。原生 CLI 由本地可控进程模拟，真实厂商会话恢复仍待验收；页面 PTY 入口另行提交。
