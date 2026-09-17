# herdr 调研：工作区、并行 Agent 与通信机制

> 调研日期：2026-09-17。面向 Dune 个人工作台与 SandDance 企业工作台。本文是源码调研与产品建议，不代表方案已经实现。
>
> herdr 固定提交：`e7e3dfa60e359404def46503dd105c165d02a561`，该提交的 Cargo 包版本为 `0.9.1`；不将包版本等同于已发布稳定版。Dune 基线：`e3e18b0a8b32aa5d895b545e6322e50abc9eb530`；SandDance 基线：`e5a7db8148aa7d8a3b6aed1db1a44c81b90e8da6`。两份本地代码在开始调研时均无未提交改动。
>
> 方法：核对固定提交的源码、同提交 `docs/next` 文档及仓库截图，并对照两个本地项目的实现。未安装或运行 herdr，未执行真实 Agent、SSH、故障恢复或性能实测。文中的耗时和交互目标若未另有说明，均为建议验收目标。

## 结论与优先级

herdr 最值得借鉴的是：**让人围绕项目组织工作，随时看见哪些 Agent 正在做事、哪些需要回应，并能直接进入相应的工作现场。** 后台进程托管、状态识别、分屏和可编程控制共同支撑了这个体验。[H01] [H06]

对 Dune、SandDance，建议优先做四件事：

1. **把项目工作区保存下来。** 记住目录、常用 Agent、打开的会话和布局；已配置好的项目提供直接继续入口，减少每次选择 Runner、Profile、cwd 的操作。
2. **把全局 Agent 列表和并排会话做出来。** 单独显示“工作中、需要回应、结果未读”，不要用进程的 `running` 代替 Agent 的工作状态。第一版支持双栏和四格即可。
3. **利用现有 ACP 能力做定向协作。** 提供“交给审查 Agent”“发送补充要求”“查看执行结果”等操作；先明确接收者、请求身份和结果，再扩展自主协作。
4. **由宿主承担持久协作状态。** Dune 提供执行、状态与目标校验，个人 Web/SandDance 保存项目和协作关系。SandDance 复用现有队列设计经验，避免把任务库放进 Gateway/fabricd。

第一批产品价值无需等待完整任务编排系统。另一方面，herdr 的终端投递和状态等待也不能直接承担企业级任务完成保证：**`done` 表示尚未看过的空闲状态；`agent prompt --wait` 等待生命周期变化，不关联某一条 prompt 的唯一完成结果。** 这两点决定了哪些交互可以借鉴、哪些协议需要我们自己补齐。[H14] [H16]

## 1. herdr 的产品形态与“打开就能用”

herdr 是运行在现有终端里的 Rust TUI，不是 Electron 桌面应用，也不是自带模型循环的 Agent 框架。它用 `ratatui` 等组件组织界面，以 PTY 托管原生 Agent CLI，使用 libghostty-vt 处理终端语义；用户仍使用已安装、已登录的 Agent。[H01] [H22] [H28]

| 使用过程 | herdr 的实现或行为 | 值得借鉴的原因 |
| --- | --- | --- |
| 在项目目录运行 `herdr` | 检查既有后台 server；不存在时启动，等待就绪后连接 | 用户不必先理解 server、socket 或配置文件 |
| 第一次进入空 session | 根据启动 cwd 建立首个 Workspace、Tab、Pane | 第一个可操作现场自动出现 |
| 在终端直接启动 Agent | 识别前台进程和状态；可选 integration 提高特定 Agent 的能力 | 可以先工作，再完善集成 |
| 同时运行多个 Agent | 项目列表与独立 Agent 列表；可点击跳转、分屏、拖动边界 | 人无需逐个打开终端确认是否卡住 |
| 切换项目或机器 | 保留各自工作现场，非当前机器继续更新摘要 | 导航不承担“结束当前工作”的含义 |
| 关闭客户端再打开 | 重连同一个存活的后台 server | 浏览界面的生命周期与执行分开 |

来源：[启动流程][H02]、[空工作区初始化][H03]、[快速开始][H23]、[多机器行为][H12]。

这里的“开机即用”应落实为**打开入口就能继续工作**。源码证据支持一条命令启动/重连，并不意味着默认安装了 OS 登录启动服务，也不意味着机器重启后原进程仍然存在。Agent 安装、账号登录以及远端 SSH 授权也并未消失。[H11] [H12]

### 1.1 界面为什么容易理解

仓库[官方截图][H21]能看到两个稳定的导航维度：左侧上半部分是项目，下半部分是 Agents；主区域保持原生终端，两个 Agent 并排显示。截图只用于理解布局，具体能力与状态语义以本次源码基线为准。

其交互原则可以迁移到 Web：

- **项目列表回答“工作在哪里”，Agent 列表回答“现在谁需要我”。** 两者用途不同，不必强行合并成一棵很深的树。
- **状态变化带来导航价值。** 看见 blocked 后直接进入对应 pane；未读结果与已看过的 idle 区分。
- **焦点明确。** 点击哪个 pane，键盘输入就进入哪个 pane；创建后台工作可使用 `--no-focus`，避免打断当前输入。
- **常用操作直接可见，高级操作放在上下文菜单。** 鼠标可完成分屏、切换和调整大小，键盘是加速路径。
- **分屏保留 CLI 自身的交互。** herdr 不要求把每一种 Agent 的终端 UI 重写一遍。

对 Dune/SandDance 应借鉴信息组织和行为，不必复制深色终端外观；现有纸张主题、ACP 对话、文件/Git 审阅仍有自己的价值。

## 2. herdr 的实现方案

### 2.1 对象模型：布局与运行对象分开

```text
Machine / Server
└── Session：一个独立运行命名空间
    └── Workspace：项目、任务或一次调查
        └── Tab：一套布局，如 agents / logs / review
            └── Pane：布局中的终端位置
                └── Terminal：实际终端状态与运行对象
                    └── Agent：当前识别出的前台 Agent
```

| 对象 | 关键事实 | 对我们设计的启发 |
| --- | --- | --- |
| Session | server 命名空间，隔离 socket 和持久状态 | 不等同于 ACP conversation 或 Runtime |
| Workspace | 有稳定 ID、cwd、Git 元数据、Tab 集合、可选 worktree provenance | 项目归属和显示名称不依赖当前选中的 Agent |
| Tab | 持有布局和 pane 集合 | 同一项目可以有多种视图 |
| Pane | 通过 `attached_terminal_id` 关联终端，保存布局/展示状态 | 移动视图不必重启进程 |
| Terminal | 保存运行状态、Agent 身份和 integration 信息 | 执行身份应独立于界面位置 |
| Agent name | 当前存活 Agent 的人类可读别名；退出/替换后释放 | 别名用于选择，提交前仍需解析实际执行对象 |

Workspace/Tab/Pane 的公开 ID 包含 `w1`、`w1:t1`、`w1:p1` 等形式，作用域是一个 server。跨 Workspace 移动 Pane 会改变公开 pane ID；移动后的返回值才是下一次操作的目标。herdr 还维护启动环境里的旧 pane ID 到同一终端的别名，以便既有进程使用 `--current`。这属于 herdr 的实现取舍，不是 Dune 需要复制的兼容层。[H04] [H24] [H29] [H16]

### 2.2 运行架构

```mermaid
flowchart LR
    UI["TUI 客户端\n工作区 / Agent 列表 / 分屏"] -->|本地 IPC 或 SSH bridge| Server["每台机器的 herdr server"]
    Caller["用户脚本或 Agent"] --> CLI["herdr CLI"]
    CLI --> API["本地 JSON socket API"]
    API --> Server
    Server --> Layout["Workspace / Tab / Pane 布局"]
    Server --> PTY["TerminalRuntime / PTY"]
    PTY --> Agent["原生 Agent CLI 或 shell"]
    PTY --> Detect["前台进程 + manifest / integration"]
    Detect --> Events["状态汇总 / 内存事件"]
    Events --> UI
    Server --> Snapshot["session.json\n布局与恢复元数据"]
```

源码中可区分两种接口：自动化使用按行 JSON 请求/响应及事件订阅；客户端终端协议采用带长度前缀的二进制消息。Unix API socket 设置为 `0600`。这是一种依赖本机用户权限和 SSH 的控制模型，不等同于 Dune/SandDance 的 Tenant、Runner、操作级授权。[H17] [H19] [H20]

### 2.3 启动：默认值和恢复路径比配置页面更重要

`auto_detect_launch` 将“找到 server → 必要时创建 → attach”封装为默认启动路径。新 server 的 socket readiness 最长等待 15 秒；首次启动 cwd 通过内部环境信息传入，空 session 才据此生成工作区。[H02] [H03]

这不是无条件“每次启动都新建项目”。已有 session 继续使用已有工作区。首次 onboarding 重点解释鼠标、前缀键和可选 Agent integration，集成不是使用普通终端的前提。[H25]

迁移到 Web 时，关键是把 **Runner 可用、项目可用、Agent 可输入** 三个阶段接起来。简单地隐藏 Profile 字段，但仍把用户停在空工作台，无法得到相同体验。

### 2.4 工作区与 worktree：展示分组和文件隔离是两件事

普通 Workspace 将 cwd、项目名、Git 分支与多个终端组织在一起；它自身不保证 Agent 之间的文件隔离。herdr 另外实现了 `worktree.list/create/open/remove`：[H18] [H17]

- `create` 创建 Git checkout，同时返回 Workspace、Tab 和 root Pane。
- 分支已存在时 checkout 该分支；不存在时从指定 base 或 `HEAD` 创建。
- `open` 可以打开既有 checkout，也可以返回已经打开的 Workspace。
- `remove` 移除 linked worktree，不顺带删除 Git branch；脏目录有显式失败/强制处理路径。
- Workspace 保存 worktree 分组来源，侧栏据此把主仓库与相关 checkout 关联起来。

**多个 Agent 可并行运行，不代表可以安全地同时修改同一目录。** 对并行写代码，值得借鉴的是“从项目直接创建隔离 worktree 并启动 Agent”的连贯操作；普通分屏可以继续服务日志、终端、只读审查等场景。

### 2.5 Agent 状态：从运行事实构建注意力视图

herdr 首先识别 Pane 的前台进程，再根据该 Agent 的能力选择状态来源。[H06]

1. 有完整生命周期 hook/plugin 且正在报告时，由 integration 决定状态；不会同时启用另一套 screen fallback 争夺同一个状态。
2. 没有完整生命周期信号时，读取终端缓冲区的实时底部，按 TOML manifest 匹配屏幕、OSC 标题等信号。用户滚动查看历史不改变检测窗口。
3. 状态层避免部分短暂 UI 变化导致 working/idle 抖动；`agent explain` 提供匹配规则、来源和 fallback 原因，便于诊断。[H07] [H26]

特别需要纠正一个容易产生的印象：**当前 Claude Code、Codex 的状态权威仍是 screen manifest；它们的 integration 主要提供原生 session 信息。** “装 integration”不等于“所有 Agent 都有精确生命周期回调”。manifest 未匹配的新式提示可能落为已知 Agent 的 idle fallback，所以不能拿此状态自动放行敏感动作。[H06]

| 显示状态 | herdr 的含义 | 能否证明任务成功 |
| --- | --- | --- |
| `working` | 检测到 Agent 正在处理工作 | 不能 |
| `blocked` | 检测到需要回答、授权或决策的 UI | 不能；还要检查具体问题 |
| `done` | Agent idle，但该结果尚未被相应观察者看过 | 不能，不是业务成功状态 |
| `idle` | 已看过的空闲/等待输入状态 | 不能 |
| `unknown` | 无法可靠分类或非已识别 Agent | 不能 |

Workspace 汇总优先级在代码里明确为：`blocked > idle且未读（done） > working > idle且已读 > unknown`。独立 Agent 面板也支持按关注优先级、状态变化序号排序。[H08] [H09]

`done` 涉及观察者：每个 TUI 客户端各自记录已看过的 completion，CLI/API 使用 server 的 seen 状态。因此同一 Agent 在不同窗口里可以显示不同的未读状态。Dune/SandDance 应继承“执行状态与已读状态分开”的原则，具体选择按用户还是按窗口已读则由产品决定。[H16]

### 2.6 分屏：BSP 布局树加独立终端

核心布局是一棵二叉空间分割树：[H05]

```text
Node = Pane(pane_id)
     | Split(direction, ratio, first, second)
```

分屏就是把叶子替换为 Split；调整边界修改 ratio；移动、交换、关闭等操作更新树及焦点。树和 terminal runtime 分开，使 Pane 可以在既有进程继续运行的情况下重排。生产拆分还需要处理新终端创建失败后的布局回滚。[H05] [H24]

多机器下，当前机器提供可见终端画面，其他机器继续提供 Workspace/Agent 摘要而不持续传输 pane 屏幕。断开的机器保留灰显缓存，直到新连接及匹配的画面到达才重新接收输入；远端重连不抢当前选择。[H12]

这对 Web 有两层价值：一是分屏布局独立于 Runtime；二是**全局状态订阅不应要求为每个 Agent 打开完整终端/对话流**。后者比直接复制任意深度 BSP 更值得优先实现。

### 2.7 恢复：四种能力分别成立

| 情况 | herdr 的实际保证 | 对 Dune/SandDance 的借鉴 |
| --- | --- | --- |
| 客户端 detach/重连 | server 存活时，原进程与终端保留 | 关闭视图不等于停止 Agent |
| server 冷重启 | 恢复 Workspace、Tab、Pane、cwd、布局等；原进程不存活 | 保存布局不能声称恢复了执行 |
| 原生 Agent session 恢复 | 仅对符合条件的官方 integration 引用，调用 Agent 自己的 resume 入口 | 需要 Provider 能力和 session 身份，不能盲目重新提交上一条 prompt |
| experimental live handoff | 尝试交接存活 PTY/进程；进行中的 API、wait、subscription 仍可中断 | 不把进程交接和请求完成保证混为一谈 |

`session.json` 保存布局及恢复元数据；屏幕历史是单独的 `session-history.json`，默认关闭。当前原生 Agent session 恢复默认开启，但只有有效、受支持的引用才适用。[H10] [H11]

Dune 的现状还有一项独立优势：tmux 与 fabricd 分离，重启 fabricd 保留 PTY；fabricd 自己拥有的 ACP 进程则会停止。不能为了模仿 herdr 统一生命周期，反而丢掉已有的 PTY 持续运行能力。[D06]

## 3. Agent 通信究竟如何实现

### 3.1 主路径：可编程地操作另一个 Agent

在本次基线中，最清晰的协作路径是：**Agent A 调用 herdr CLI/API，定位 Agent B，投递 prompt，等待状态，读取终端结果。** 它提供协作原语，规划、分工、结果判断仍由调用者负责。[H13] [H16] [H17]

| 原语层 | 代表能力 | 边界 |
| --- | --- | --- |
| Layout | Workspace/Tab 创建，Pane split/move | 负责安排位置 |
| Pane | run、send-text、send-keys、read、wait-output | 面向原始终端，不理解里面一定是 Agent |
| Agent | start、list/get、prompt、wait、read | 解析当前存活 Agent 并检查状态 |

`agent start` 要求一个已经存在、前台是 shell 且可输入的 Pane，不会隐式创建布局。创建 Workspace/Tab 已返回 root Pane，调用者无需再多分一个空 Pane。Agent 也可以是用户手工启动后被自动识别的。[H16]

例如，在已有 `reviewer` 后，可使用下面这条真实 socket API 请求提交并等待：

```json
{
  "id": "review-1",
  "method": "agent.prompt",
  "params": {
    "target": "reviewer",
    "text": "审查当前改动，列出问题、文件位置和验证建议。",
    "wait": { "timeout_ms": 120000 }
  }
}
```

该请求的 `id` 用于请求/响应关联，不能据此宣称获得了持久幂等或业务 Task ID。默认等待 `idle/done/blocked`；等待成功之后还要检查状态并读取结果。CLI 对应 `herdr agent prompt reviewer '…' --wait --timeout 120000`。[H16] [H17]

### 3.2 投递和等待：值得认真借鉴的细节

1. **解析当前目标。** Agent name 或 pane ID 解析到实际 Agent/Terminal，并检查它仍占据前台；目标已被 shell、编辑器或另一 Agent 替换时拒绝输入。[H13]
2. **已 blocked 则拒绝普通 prompt。** 返回 `agent_blocked`，不把一段任务文本误送进授权对话框；具体交互通过单独的 send-keys 完成。[H13]
3. **文本与 Enter 有序提交。** 遵守实时 bracketed-paste 模式，文本与延迟 Enter 作为一组排入终端输入；代码中还存在平台/Agent 特例。[H13]
4. **prompt 和 wait 同请求。** 捕获投递前事件位置、状态及身份，避免分开调用时漏掉快速的状态变化。[H14]
5. **观察本次投递是否产生活动。** 从非 working 状态提交后，最多给五秒观察 working/blocked；否则是 `agent_prompt_stalled`，而非把原来就 idle 误判为完成。[H14]
6. **再等所需状态。** wait 钉住原目标身份；目标消失、移动或替换会终止相关等待，不能让另一 Agent 满足它。[H14] [H16]

五秒活动门槛与终端提交延迟是 herdr 的实现参数，不是建议直接复制给 Dune 的通用协议保证。

### 3.3 它仍然不是逐请求的完成协议

herdr 明确允许给 working Agent 投递 prompt。但是，如果它本来就在 working，当前旧 turn 的结束也可能满足新请求的 `--wait`。终端投递成功最多证明输入写入；状态变 idle 不能证明某条具体 prompt 已被处理，更不能证明代码正确。[H16]

结果读取来自终端快照/历史，不是一个统一的结构化 Agent response。部分全屏 Agent 将历史放在 alternate screen，herdr 会在满足空闲等条件时借助应用滚动采集并还原视口；这种读取有应用依赖，不能保证拿到全部答案。文档也提供了让 Agent 将结果写到文件、再读取文件的兜底方式。[H16]

通信事件同样有明确限制：`EventHub` 只在内存中保留最多 512 条；订阅从受理时刻开始，不自动回放此前保留事件。外部客户端应先订阅并缓冲，再获取 `session.snapshot`；断线后重新获取快照。[H15] [H17]

因此，对本次源码能作出的结论是：**herdr 已提供很实用的 Agent 定向控制及状态协作接口，但这条主路径不是持久邮箱、任务数据库或 exactly-once 消息系统。** 也未见在这一接口上定义跨重启的消息送达/处理回执合同。

### 3.4 跨机器：显式路由，不能依赖当前 UI 选择

`herdr --machine <label-or-id> …` 通过保存的 SSH 机器配置转发到目标 server；目标 server 必须已经运行且支持相应 API。自动化不会因为失败而改投 Local，也不自动安装/重启远端 server。不同 server 可以都有 `reviewer` 或 `w1:p1`，因此 machine 和 session 是完整目标的一部分。[H12]

这一点直接适用于 Dune：跨 Runner 的调用必须带完整目标和授权，Agent A 不能因为用户在侧栏切换了环境，就突然获得另一环境的控制权。超时或断线后先查询状态；不得把“没有收到回执”解释成“没有执行”而重发。

## 4. 对照当前 Dune 与 SandDance

以下是本次代码基线的事实，不沿用旧方案文档对“尚未实现”的判断。

| 能力 | Dune 当前实现 | SandDance 当前实现 | 相对 herdr 的主要差距 |
| --- | --- | --- | --- |
| 启动与接入 | 本地组合 Web/API/Gateway；接入命令、用户级 connector 服务；可保存 Profile | 企业登录、模板选择、Environment Profile 初始化、开发机接入；TAE OAuth 按需触发 | 从可用环境到项目/Agent 可工作仍需多次选择，缺少项目级默认项 |
| 项目工作区 | Web `Workspace` 基本围绕当前 Runner，维护 cwd、Session 列表和一个 selected Runtime | `WorkspaceHost` 按 Tenant/Runner 缓存工作现场，cwd 按 Runner 保存在浏览器 | 都没有与 herdr Workspace 对等的独立项目对象及可恢复多 Agent 布局 |
| 多会话并行 | 后端可运行多个 Runtime，Web 主区一次展示一个 | `opened[]` 保持多会话组件，但 `hidden={selected !== key}` 只显示一个 | 已有并行执行；缺少并排观察与输入的界面 |
| 可调布局 | 终端/对话与 diff 视图 | Agent、编辑器、文件/Git 面板可调整宽度，尺寸保存到 localStorage | SandDance 的 resize 已有基础，但不是多个 Agent 的 split layout |
| Agent 列表/状态 | `Runtime.State` 主要是进程 running/exited；ACP 另外有 ready/busy/permissions/revision | 会话按环境组织，runningCount 仍按进程统计；ACP 状态留在会话组件中 | 缺少跨环境 Agent 摘要、未读完成和关注优先级 |
| 后台执行 | PTY 由 tmux 托管；fabricd 重启不销毁 PTY | 复用 Dune，页面关闭不结束远端会话 | 无需重造 terminal server，应把现有保证呈现清楚 |
| Agent 程序化调用 | `host.AgentExecutor` 已支持受信宿主的 Start/Get/Observe/State/Action/Stop | IM 接入使用此能力；无通用 Agent 协作 UI | 缺少面向协作的目标目录、请求关系和统一结果入口 |
| 持久消息 | 独立 `im` module 有 Inbox/Conversation/Delivery 及 SQLite 实现 | `internal/imstore` 有 PostgreSQL inbox、去重、lease、投递和 unknown 处理 | 有可借鉴的实现基础，但 IM conversation 不等于通用 Agent 协作对象 |
| worktree | Git 操作支持普通分支和差异，内部认识共用 Git 目录锁 | 文件/Git 审阅较完整 | 当前 Dune capability/Git action 中无专门的 worktree 分配、登记和清理合同 |

依据：[Dune Web][D01]、[Runtime 类型][D02]、[ACP controller][D03]、[宿主 Agent 接口][D04]、[Dune 生命周期][D06]、[Git 操作][D07]、[SandDance 工作区][S01]、[布局偏好][S02]、[ACP UI][S03]、[IM 存储][S04]。

### 4.1 现有 ACP 能力比终端状态推测更适合做协作

Dune 的 managed ACP 已经能够得到 `ready`、`busy`、`permissions`、`revision`、`session_id` 和 `stop_reason`。`acp.action` 在忙碌时拒绝新的普通动作，prompt 经 ACP RPC 发出，结果通过 state/update 传递。[D03]

`host.AgentExecutor` 是可复用的受信宿主入口，仍走 Gateway 并复核 Runner 归属和能力。它当前围绕 IM 使用，Action 限定为 `new/load/prompt`，没有替别的请求确认权限或取消无关 turn 的能力；公开 `AgentState` 也未暴露底层完整 permissions。因此不能声称拿它就已具备完整的全局 blocked 操作面板。[D04]

`im/duneagent/prompt.go` 已实现一条重要的闭环：校验 Runtime → 先 Observe → 读当前 State → 提交 prompt → 收集相同 ACP session 的输出 → 根据后续 State 判断结束；遇到订阅错误、内容省略或退出时保留结果可能未知的语义。[D05]

建议从这条路径提炼与 IM 无关的 ACP 调用能力，而不是先为 ACP 也做 screen scraping。但该路径目前仍不是完整的通用 Turn Store；新增协作时还需处理 Web、IM 和其他 Agent 同时向一个 Session 写入的所有权、请求关联问题。

### 4.2 已有 IM 不应被误当成“已经有 Agent 间通信”

当前 IM 模块解决的是“外部消息 → 固定 Bot/Conversation → ACP → 对外回复”。SandDance 使用 HTTP callbacks；PostgreSQL 保存队列与投递事实，同一 conversation 按序处理，running/unknown 不自动接管或重放。[D09] [S05]

这为 Agent 协作提供了值得复用的代码经验：持久受理后再执行、lease fencing、固定目标快照、结果未知时阻止重放。**不能直接把 Agent ID 填进飞书 Chat ID，然后认为完成了协作模型。** Agent 间请求还需要发送方身份、接收方授权、任务关系、artifact 引用及明确的响应归属。

### 4.3 与已有多 Agent 调研的关系

仓库的[多 Agent 协作调研](multi-agent-communication-research.md)强调 Workspace、任务状态、Evidence 和有界异步交接，这一方向仍然适用。但其 2026-09-08 对 Managed/metadata 等职责的描述属于当时基线；当前 Managed 生命周期由 SandDance 等宿主拥有，Dune 已增加独立 IM module 和 `host.AgentExecutor`。[D08] [D09]

herdr 补充了一个更近的产品入口：**先交付多 Agent 可见、可切换、可并排工作的工作台，再按真实协作需要增加任务控制。** 不需要把完整任务 DAG、调度器、版本化文件系统作为分屏的前置依赖。

## 5. 推荐的目标体验

### 5.1 保留企业空间，在其下面增加项目工作区

SandDance 已将 Tenant 称作“工作空间”。建议新增对象显示为“项目工作区”，避免同时存在两个含义不同的“空间”。Dune 个人模式省去 Tenant 这一层。

```text
工作空间：研发团队                         项目：dune · devbox-01 · main
┌──────────────────────┬───────────────────┬───────────────────┬───────────────┐
│ 项目                 │ 实现 Agent        │ 审查 Agent        │ 文件 / Git    │
│  dune                │ 工作中            │ 需要回应          │ 当前焦点目录  │
│  service-api         │                   │ [查看请求]        │               │
│                      ├───────────────────┴───────────────────┤               │
│ Agents · 需要关注 2  │ 辅助终端 / 测试输出（按需打开）       │               │
│  审查者 · 需要回应   │                                       │               │
│  测试者 · 结果未读   │                                       │               │
│  实现者 · 工作中     │                                       │               │
└──────────────────────┴───────────────────────────────────────┴───────────────┘
```

这是布局建议，不是当前产品截图。第一版允许同一项目、同一 Runner 下两个或四个 Agent 并排；全局列表可以跨已授权 Runner 导航。跨 Runner 混排和任意嵌套分屏留给后续实际需要，先避免文件审阅区的目标变得难以辨认。

### 5.2 “开始工作”与“继续工作”各有清晰入口

**首次使用 Dune 个人模式：** 单入口完成本地组合服务启动并打开浏览器；完成必要身份和 Runner 接入后，选择目录，发现或选择可用 Agent，直接开始。默认值可以自动准备，但不能把“命令存在”显示为“账号已登录、模型调用已就绪”。启动装配仍使用现有 Gateway 路径。

**首次使用 SandDance：** 管理员准备可用环境模板和 Agent Profile；研发选择项目模板与名称，看到分阶段进度：环境创建、连接就绪、项目初始化、Agent 准备。保留已有按需 OAuth，不在入口要求研发填写 PSM、provider endpoint 或底层命令。[S06] [S07]

**日常继续：** 项目卡片显示上次目录、Agent 状态和“继续工作”。先验证 Runner binding 与 Runtime 身份；仍是同一实例时恢复视图，身份变更时显示原现场失效和进入新现场的明确操作。恢复布局本身不应重发 prompt，也不自动取得其他人的输入权。

**启动 ACP 的一次操作：** 当前 UI 先启动 Runtime，再点击“开始对话”，随后输入任务。[S01] [S03] 可以把首条任务输入作为入口，依次完成 prepare/start → ready → new session → prompt，并展示阶段。仅在没有待恢复 session 且前一阶段已确认成功时推进；`new` 或 prompt 的结果未知时停在原尝试查询，不重新开始。

**Profile 是高级配置的承载，不必是每次工作的表单。** 常用 Profile 及其固定修订可成为项目默认项；修改 Profile 后是否用于下一次启动应明确，不能悄悄改变已有 Agent Session。

### 5.3 全局 Agent 列表

每行保留：名称/角色、Agent 类型、项目、Runner、活动状态、最近变化时间、未读标记。默认优先呈现“需要回应”和“结果未读”，提供按项目查看和搜索；详情打开后再展示 Runtime ID、协议等信息。

执行状态、连接状态与注意力状态分别表达：

- 执行：运行中、已退出，以及退出原因。
- 活动：准备中、工作中、等待回应、空闲、未知；附上来源为 ACP、hook 或终端推测的诊断信息。
- 注意力：某用户是否看过这次完成/问题；不能把“读过”写成全局 Agent 生命周期变化。
- 连接：实时、重连中、离线缓存；缓存不得继续显示为确认在线。

先覆盖 managed ACP 的可确认状态。对 PTY Agent，先提供进程事实；随后为高频 Agent 增加 hook 或 manifest 适配，无法分类时保留 unknown。不能仅凭“几秒没有输出”推断任务完成，也不能把 PTY 输出忙碌程度当作可靠 Agent 状态。

### 5.4 分屏的交互合同

“在右侧打开”“四格查看”“聚焦此会话”“移出视图”应独立于“新建 Agent”和“停止 Agent”。关闭一个视图只移除视图；停止和清理历史延续现有明确动作。

同一个 Runtime 在一个工作台里复用一个连接/controller；若需要第二个视图，采用观察视图或跳到原位置，不应同时创建两个会争抢输入租约与 resize 的可写连接。对 ACP 也需要同样的单一操作所有权，避免用户输入与协作请求竞争。

焦点 pane 决定文件/Git 的跟随目标，界面显示项目、worktree/branch 和目录；用户可以固定审阅目录。某个 Agent 状态变化不自动切换焦点或审阅目标。隐藏 pane 不应以零尺寸 resize 远端终端；可见 pane 在容器尺寸改变时更新尺寸。

宽度不够时收敛为单 pane 加切换列表；保留布局和会话，不以缩小字体维持四格。键盘移动焦点与调整分隔线沿用现有可访问组件，快捷键不得吞掉终端原生输入。

## 6. 推荐的实现边界与数据模型

### 6.1 谁负责什么

| 层 | 推荐职责 | 不应顺带承担 |
| --- | --- | --- |
| Dune fabricd | Runtime/PTY/ACP 执行；当前实例的活动摘要；如有必要，结构化 worktree 操作 | 企业项目库、任务 DAG、持久消息正文、每个用户的未读状态 |
| Dune Gateway / SDK | 目标路由、身份与操作授权、订阅和调用；保持 generation/input lease 校验 | 以连接成功代替业务成功；跨重启无限重放 |
| 可复用宿主服务 | 项目/会话引用、ACP 请求执行合同、协作目标解析；与存储和具体 UI 分离 | 在底层协议里固化 Planner/Coder/Reviewer 等角色 |
| Dune 个人 Web 宿主 | 个人项目与布局、默认 Profile、个人 Agent 列表；需要持久化时使用个人宿主存储 | 为个人使用引入企业 Tenant 编排 |
| SandDance | Tenant 下的项目与共享权限、环境模板/初始化、持久协作请求、配额与审计 | 复制一套 Dune Agent 执行后端或绕过 Gateway |
| 前端工作台 | 多 pane 布局、关注排序、焦点、已读和审阅联动 | 充当唯一后台调度器；关闭浏览器后丢掉已受理的持久任务 |

共用服务可以在 Dune 的宿主侧包中提供，两个 Web 前端分别接入；不必为了共享概念立即把两个 UI 重构为一个大组件库。原型阶段直接采用目标接口，不为本次被替换的旧对象设计兼容层。

### 6.2 最小对象

以下为建议模型，不是现有 API/schema：

| 对象 | 最小字段 | 生命周期 |
| --- | --- | --- |
| `ProjectWorkspace` | ID、owner scope、名称、Runner 引用、项目 root、默认 Profile 修订、可选 worktree 来源 | 项目级持久元数据；Runner 重新绑定不自动变成同一执行现场 |
| `WorkspaceView` | user、workspace、布局、pane 引用、焦点、审阅目录 | 用户自己的视图；共享项目不共享输入焦点 |
| `AgentSessionRef` | workspace、Profile 修订、完整 Runtime identity、可选 ACP session ID、显示名称 | 业务会话引用；可失效，不伪装成永久进程句柄 |
| `AgentActivity` | Runtime identity、活动状态、来源、状态序号/epoch、观测时间 | 可重建的实时摘要；执行实例变化后旧摘要过期 |
| `AttentionReceipt` | user、目标身份、已读的事件 epoch/seq | 读者状态；与 Agent 状态分离 |
| `AgentRequest`（协作阶段再加） | request ID、发送方、接收方、目标实例、输入、artifact refs、状态、deadline、结果引用 | 宿主持久工作对象；不因页面关闭而消失 |

第一个 UI 版本可在按账号和环境隔离的浏览器存储中保存布局偏好；需要跨设备恢复时，再由宿主持久保存 `WorkspaceView`。文件草稿、完整 transcript 和 UI 布局分别确定保留策略，不能因为保存布局就默认上传全部内容。

### 6.3 摘要流与内容流分离

Dune 目前 `runtime.list` 返回进程事实，ACP 细节在单 Runtime `acp.state`/事件中。建议新增可批量读取和订阅的活动摘要，由 fabricd 产生当前事实，宿主只聚合已授权目标；**不要改变 `Runtime.State` 的含义，把 working/blocked 塞进进程状态。**[D02] [D03]

首版可以有界并发读取摘要，但不应为列表上的 N 个 Agent 各创建一套无限轮询和完整输出流。持续演进为：

1. 打开工作台先建立摘要订阅，再读取快照，按 epoch/revision 合并。
2. 活动摘要驱动侧栏；只有用户打开的 pane 或宿主正在执行的请求消费内容流。
3. 订阅溢出、断线或实例变化时标记摘要过期，重新取快照；不假设事件无限可回放。
4. server 重启后，实时摘要从当前 Runtime 重建；持久请求状态从宿主数据库恢复。两者不能互相替代。

这借鉴了 herdr 的快照/增量和非当前机器仅传摘要的原则；具体线协议仍沿用 Dune 的 WebSocket/Yamux/protobuf，并按新增能力更新 schema 和合同测试。

## 7. Agent 通信的落地方案

### 7.1 先做三个明确的交互

| 操作 | 用户看到的行为 | 最小实现 |
| --- | --- | --- |
| 交给另一个 Agent | 选接收 Agent，附任务、当前 commit/diff 或文件引用；在旁边查看进度 | 定向请求和结果关联；默认新建或选择空闲的专用 ACP session |
| 发补充要求 | 发给明确的 Agent/请求，能看出尚未投递或已接收 | busy 时保留在宿主队列，或者明确拒绝；不把文本直接灌进权限弹窗 |
| 查看/等待结果 | 显示工作中、需要人工回应、完成、失败或未知，结果可打开 | 依据请求身份和执行事实等待；读取对应结果/Artifact |

Agent 自主调用时使用同一套宿主接口，初始范围限制为同一项目、明确加入协作的 Agent。CLI 或 MCP 是这套接口的入口形式，不能因为 herdr 提供了 CLI 就把现有 `dune` connector 命令扩展成无限权限的控制入口。

### 7.2 ACP 请求闭环

```mermaid
sequenceDiagram
    participant A as 用户或 Agent A
    participant H as 宿主协作服务
    participant G as Dune Gateway / fabricd
    participant B as Agent B（ACP）
    A->>H: 提交 request_id、目标、任务与产物引用
    H->>H: 授权、固定目标、登记 queued
    H->>H: 领取会话执行权，先登记 submitting
    H->>G: 建立观察，检查实例与空闲状态
    H->>G: 投递关联到本请求的 prompt
    G->>B: ACP session/prompt
    B-->>G: update / permission / RPC result
    G-->>H: 相关状态、结果或连接中断
    H->>H: 持久完成 / 失败 / unknown
    H-->>A: 状态通知及结果引用
```

建议请求状态至少为：`queued → submitting → running → completed/failed/cancelled`，另有 `unknown`。等待权限可作为 running 的子状态 `waiting_input`，不抢占已有请求或创建第二个 turn。

几个不能省略的约束：

- **同一 ACP Session 单请求执行。** 调度队列、Web 输入和 IM 操作必须经共同所有权/序列化规则；只在协作服务里加锁、而 Web 仍可自由写入，不足以建立请求归属。
- **请求关联需要实际实现。** 现有 `AgentConnection.Action` 成功只表示底层 action 已 accepted，不能直接拿它冒充完成接口。应扩展 managed ACP 调用合同，让请求 ID/执行 attempt 与返回结果、事件对应；或首期给每次协作独占 Session，并严格串行。仅凭“后来 busy 变空”不可靠。
- **持久 submitting 在外部调用之前。** 宿主在此期间崩溃，恢复后可能不知道 prompt 是否已发出，必须收敛为 unknown 并对账，而非自动再发。数据库里的去重只能避免重复受理，不能独自证明远端副作用只发生一次。
- **等待绑定完整身份。** 固定 owner/project、Runner binding、Runtime id/incarnation/generation 和 ACP session；恢复后新 Runtime 是新 attempt，不能接收旧请求的回写。
- **blocked 不默认代答。** 人工操作使用明确的 permission ID；Agent 间任务内容不携带给其他 Agent 自动批准任意操作的权限。
- **完成与验收分开。** ACP turn 正常结束是一次执行完成；任务是否成功，应由结果、commit、测试或审查结论决定。

新增请求 ID/结果缓存若位于 fabricd 内存，只能在该实例存活及保留窗口内查询。跨重启持久状态仍由宿主负责，不能承诺跨故障 exactly-once。原始 ACP 透传入口保持其语义，新增 managed ACP 合同需检查对它的影响。

### 7.3 PTY 协作的定位

PTY 可以借鉴 herdr 的“选目标 → 状态检查 → 有序文本/Enter → 等待 → 读取”，但它适合辅助交互，完成保证弱于结构化 ACP。若产品提供它，应在能力上区分“终端已投递”“观察到空闲”“收到明确结果”，不能都显示成“任务成功”。

优先让常用 Agent 通过 ACP 承担自动交接；PTY 继续保留完整原生体验。确需 PTY 自动化时，对每种 Agent 分别验证 bracketed paste、前台识别、审批 UI、alternate-screen 读取和版本变化，再决定支持范围。

### 7.4 权限与内容归属

Agent A 只能取得本项目中明确授权的接收目标及操作，不能持有用户的全权限 Gateway 凭据；项目/Tenant 是服务端校验，不由 prompt 里的字段决定。跨 Runner 的产物引用还需校验接收方能否读取，不能假设相同文件路径指向相同内容。

消息应尽量是任务、必要上下文和 Artifact 引用，不广播各 Agent 的全部 token stream。请求正文和结果需要持久保存时，由个人宿主/SandDance 明确存储、访问及保留规则；Gateway/fabricd 的实时转发不变成 transcript 数据库。

Sandbox/Runner 恢复后，可以继续显示原项目与历史请求；但不能声称文件仍在，或未经核对就把 `/workspace/repo` 绑定到新机器。worktree 只提供同一仓库里的 checkout 隔离，不替代跨机器文件持久化或沙箱安全隔离。

## 8. 分阶段交付建议

| 阶段 | 交付内容 | 主要改动位置 | 可验收结果 |
| --- | --- | --- | --- |
| P0：减少启动摩擦 | 项目入口、记住默认 Agent/目录、分阶段启动、继续工作 | 个人 Web；SandDance 项目/Profiles/启动流 | 已配置项目进入时无需再填启动命令；首次 ACP 输入可完成已确认的初始化链路 |
| P1：并行工作台 | 两栏/四格、全局 Agent 列表、ACP 活动摘要、关注/未读、失效恢复 | Dune 活动摘要及宿主接口；两端 Web session/controller 和布局 | 四个 Agent 同时运行可见；需要回应能直接定位；关闭视图不杀进程；离线不显示假实时 |
| P2：隔离与定向协作 | worktree 创建/登记/清理；向一个 ACP Agent 交任务、等待、取结果 | Dune 薄 worktree 能力；宿主 Workspace/AgentRequest；优先 SandDance 队列实现 | 两个写 Agent 使用独立 checkout；请求绑定确切 Session；中断或回执丢失不重复提交 |
| P3：按需要扩展 | 有限自主委派、跨 Runner 交接、跨设备布局、精选 PTY integration | 宿主策略/持久存储与适配包 | 预算、目标权限、结果归属和恢复语义可验证；故障不扩散到无关 Agent |

建议先在 SandDance 验证并行工作台：它已经保留 `opened[]`、多个环境的 WorkspaceHost 和可调审阅区，前端起点更接近目标。底层活动摘要和执行合同在 Dune 实现，Dune 个人 Web 同期或随后接入；无需把企业生命周期逻辑搬回 Dune。[S01] [S02]

P2 可以先用独占 ACP Session 与有限串行队列完成两 Agent 的交接，不必一开始实现任意任务图。worktree 与请求持久化可以分别交付，但面向并行写入的产品入口应等文件隔离具备后再开放。

### 8.1 建议验收场景

这些是后续实现的验收要求，本次未执行：

| 场景 | 必须观察到的结果 |
| --- | --- |
| 已配置项目重新进入 | 项目、布局及仍有效会话可恢复；无重复 start/new/prompt |
| 4 个 Agent 同时运行，其中 1 个需确认 | 侧栏准确计数，一步跳到目标；另外 3 个继续运行 |
| 切换/缩放/关闭 pane | 输入只进入当前持有操作权的目标；无隐藏 pane 零尺寸 resize；移出视图不停止 Runtime |
| Agent A 在工作时收到第二条任务 | 明确 queued 或 busy；旧 turn 结束不能冒充第二条任务结束 |
| 投递后断网、宿主在 submitting 后崩溃 | 保留原 request ID，显示 unknown 或可查询事实；无自动重发 |
| Runtime/Runner 身份变化 | 旧视图和旧回执不能作用到替代实例；允许查询/导出旧现场 |
| UI 未打开时 Agent 完成 | 宿主持久任务仍正确结束；下一次进入看到结果和未读状态 |
| 两个用户看同一项目 | 已读、焦点、输入权按各自合同工作；一个人看过结果不会隐式替所有人标记 |
| 并行 worktree 清理 | 脏目录有明确处理；关闭布局不删除 checkout；移除 checkout 不顺带删除分支 |
| 多 Runner/权限撤销/慢订阅者 | 不越权聚合；撤销后停止访问；溢出后重取摘要；一个慢目标不拖住整个工作台 |

性能目标应在实施时选定环境测量：重点记录首次可输入时间、状态到侧栏的延迟、四格输入延迟、后台 Agent 增长时的订阅/内存成本。不能用 herdr 的单机 TUI 观感直接推断 Web 跨网络表现。

## 9. 采用范围与取舍

| 采用方式 | 内容 |
| --- | --- |
| 直接借鉴交互原则 | 项目与 Agent 两种导航、关注优先级、分屏焦点、关闭视图与停止执行分离、可恢复布局 |
| 按现有架构改造 | Workspace/Tab/Pane 分层、快照与摘要订阅、worktree 工作流、Agent 目标解析及等待 |
| 作为受限适配参考 | 终端 manifest、PTY prompt 投递、原生 Agent session resume、alternate-screen 读取 |
| 暂不采用 | 自研终端渲染器替代 xterm、任意深度布局作为第一版、全量 Agent 聊天网络、在 Gateway/fabricd 保存持久任务库 |

herdr 在本次提交使用 Apache-2.0。[H27] 如后续实际移植源码或规则，需要保留相应许可证、版权及适用声明，并按引入内容检查 vendor 依赖；本次仅新增调研文档，没有引入其源码或第三方资产。

## 10. 证据索引

herdr 链接均固定到本次提交；`docs/next` 是该提交随附文档，不代表独立核验了线上最新部署。Dune/SandDance 链接指向本地本次阅读的文件，基线提交见文首。

| 编号 | 证据 | 支撑范围 |
| --- | --- | --- |
| H01 | [README][H01] | 产品形态、安装、运行边界 |
| H02–03 | [autodetect][H02]、[bootstrap][H03] | 后台 server 启动、首次 cwd 与空工作区 |
| H04–05 | [Workspace][H04]、[BSP layout][H05] | 对象关系、分屏树 |
| H06–09 | [Agent 文档][H06]、[Codex manifest][H07]、[状态汇总][H08]、[Agent 面板][H09] | 状态来源、检测证据、未读与排序 |
| H10–11 | [快照结构][H10]、[恢复说明][H11] | 布局/进程/历史/原生 session 的不同边界 |
| H12 | [多机器说明][H12] | 独立重连、摘要流、显式远端路由 |
| H13–15 | [Agent API][H13]、[wait 实现][H14]、[EventHub][H15] | prompt 投递、等待门槛、有界事件 |
| H16–17 | [自动化说明][H16]、[Socket API][H17] | 操作语义、结果读取、快照与订阅 |
| H18–20 | [worktree 命令][H18]、[二进制协议][H19]、[API server][H20] | checkout 生命周期、传输与本地 socket |
| H21–27 | [截图][H21]、[依赖][H22]、[快速开始][H23]、[Pane][H24]、[onboarding][H25]、[检测防抖][H26]、[许可证][H27] | 视觉依据及补充实现 |
| H28–29 | [Ghostty 终端封装][H28]、[Tab][H29] | 终端语义实现与布局/运行对象分离 |
| D01–03 | [Web][D01]、[Runtime][D02]、[managed ACP][D03] | Dune 当前选择/展示和状态模型 |
| D04–06 | [AgentExecutor][D04]、[IM ACP Prompt][D05]、[fabricd Close][D06] | 可复用执行入口及恢复边界 |
| D07–09 | [Git][D07]、[README][D08]、[IM README][D09] | worktree 能力差距、职责、独立 IM |
| S01–03 | [工作区][S01]、[布局偏好][S02]、[ACP UI][S03] | 多会话保留、单会话展示、组件状态 |
| S04–07 | [IM Inbox][S04]、[IM 设计][S05]、[Profile 表单][S06]、[工作台说明][S07] | 企业队列、初始化与交互基础 |

[H01]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/README.md
[H02]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/server/autodetect.rs#L295
[H03]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/server/headless/bootstrap.rs#L98
[H04]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/workspace.rs#L176
[H05]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/layout.rs#L73
[H06]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/docs/next/website/src/content/docs/agents.mdx
[H07]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/detect/manifests/codex.toml
[H08]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/workspace/aggregate.rs#L50
[H09]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/client/shell/agent_sidebar.rs#L20
[H10]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/persist/snapshot.rs#L16
[H11]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/docs/next/website/src/content/docs/session-state.mdx
[H12]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/docs/next/website/src/content/docs/connecting-machines.mdx
[H13]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/app/api/agents.rs#L111
[H14]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/api/wait.rs#L177
[H15]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/api/event_hub.rs#L13
[H16]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/docs/next/website/src/content/docs/agent-automation.mdx
[H17]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/docs/next/website/src/content/docs/socket-api.mdx
[H18]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/worktree.rs#L175
[H19]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/protocol/wire.rs#L1607
[H20]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/api/server.rs#L27
[H21]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/assets/screenshot.png
[H22]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/Cargo.toml
[H23]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/docs/next/website/src/content/docs/quick-start.mdx
[H24]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/pane/state.rs#L6
[H25]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/ui/onboarding.rs
[H26]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/pane/agent_detection.rs
[H27]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/LICENSE
[H28]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/ghostty/mod.rs
[H29]: https://github.com/herdrdev/herdr/blob/e7e3dfa60e359404def46503dd105c165d02a561/src/workspace/tab.rs#L38
[D01]: /Users/bytedance/Workspace/aiomni/dune/web/src/app.tsx:211
[D02]: /Users/bytedance/Workspace/aiomni/dune/pkg/api/types.go:199
[D03]: /Users/bytedance/Workspace/aiomni/dune/pkg/fabricd/acp.go:35
[D04]: /Users/bytedance/Workspace/aiomni/dune/pkg/host/agent_executor.go:49
[D05]: /Users/bytedance/Workspace/aiomni/dune/im/duneagent/prompt.go:19
[D06]: /Users/bytedance/Workspace/aiomni/dune/pkg/fabricd/daemon.go:61
[D07]: /Users/bytedance/Workspace/aiomni/dune/pkg/fabricd/git.go
[D08]: /Users/bytedance/Workspace/aiomni/dune/README.md
[D09]: /Users/bytedance/Workspace/aiomni/dune/im/README.md
[S01]: /Users/bytedance/Workspace/aiomni/SandDance/web/src/features/workbench/page.tsx:32
[S02]: /Users/bytedance/Workspace/aiomni/SandDance/web/src/app/layout-preferences.tsx
[S03]: /Users/bytedance/Workspace/aiomni/SandDance/web/src/features/workbench/acp.tsx:162
[S04]: /Users/bytedance/Workspace/aiomni/SandDance/internal/imstore/inbox.go
[S05]: /Users/bytedance/Workspace/aiomni/SandDance/docs/im-integration.md
[S06]: /Users/bytedance/Workspace/aiomni/SandDance/web/src/features/profiles/form.tsx
[S07]: /Users/bytedance/Workspace/aiomni/SandDance/docs/web-workspace.md
