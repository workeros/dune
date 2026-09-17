# herdr 借鉴方案实施记录

目标：[已确认方案](references/herdr-adoption-design.md)。逐功能实现、验证、提交，遵守 SOLID / KISS；不引入宿主队列协调系统，不保留原型旧合同的兼容层。

## 功能提交清单

- [x] 项目工作区共享后端：Owner / Tenant 项目、多个 Runner 目录、默认 Profile，数据库和 HTTP 合同；SandDance 外层路由随产品接入提交。
- [x] 个人工作现场共享后端：分屏树、焦点、审阅目标、已读位置，数据库持久化和并发更新检查；页面接入随两产品工作台提交。
- [x] Agent 摘要基础：Runtime 列表直接携带 ACP 活动与 PTY 前台信息，活动与进程状态分离，无需打开内容订阅。
- [ ] PTY 原生状态适配：受支持 Agent 的工作 / 空闲 / 权限状态及可靠会话 ID 采集。
- [x] Dune 并行工作台：项目和 Agent 导航、跨 Runner 分屏、焦点审阅联动、布局恢复。
- [x] SandDance 并行工作台：接入共享合同，Tenant 内自由分屏与个人布局。
- [x] worktree 准备能力：SDK / Gateway / fabricd 新建和列出 worktree，复用 Git 仓库锁；不覆盖目录 / 分支，不复制未提交内容。
- [x] 启动位置：两产品使用已有就绪 Runner，当前目录 / worktree 选择、项目默认配置。
- [x] 统一启动服务：固定配置解析、宿主环境默认值、当前目录 / 新 worktree、启动前快照与部分结果保留。
- [x] 恢复索引数据库：不可变实际配置、原生 ID 绑定、最近启动 attempt、跨连接继续去重与未知结果屏障。
- [x] 原生会话切换索引：Runtime 当前关联、分离进程 / 原生 cwd、迟到确认只补历史、重复 load 去重。
- [ ] 原生会话恢复：实际启动快照、可靠 ID 采集、恢复索引与并发继续去重。
- [x] fabricd ACP 操作：Runtime 串行队列、操作引用、wait / read、有界输出与失效语义。
- [x] fabricd PTY 投递：人工按键与文本 / Enter 统一排序、目标检查与投递状态。
- [ ] Tenant Agent 服务：发现、启动、投递、等待、读取，复用 SDK / Gateway 路由。
- [ ] MCP 接入：工具合同、会话凭据、跨 Tenant 拒绝、凭据脱敏。
- [ ] MCP 注入：managed ACP 配置、受支持 PTY 适配器与必要的 stdio bridge。
- [ ] 两产品协作交互：pending、操作进度、输出不完整与引用失效提示。
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
