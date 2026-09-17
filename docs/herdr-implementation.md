# herdr 借鉴方案实施记录

目标：[已确认方案](references/herdr-adoption-design.md)。逐功能实现、验证、提交，遵守 SOLID / KISS；不引入宿主队列协调系统，不保留原型旧合同的兼容层。

## 功能提交清单

- [ ] 项目工作区：Owner / Tenant 共享项目、多个 Runner 目录、默认 Profile，数据库和 HTTP 合同。
- [ ] 个人工作现场：分屏树、焦点、审阅目标、已读位置，数据库持久化和并发更新检查。
- [ ] Agent 活动：PTY / ACP 摘要、具体会话引用、活动与进程状态分离。
- [ ] Dune 并行工作台：项目和 Agent 导航、跨 Runner 分屏、焦点审阅联动、布局恢复。
- [ ] SandDance 并行工作台：接入共享合同，Tenant 内自由分屏与个人布局。
- [ ] 启动位置：已有就绪 Runner，当前目录 / worktree 选择、项目默认配置。
- [ ] 原生会话恢复：实际启动快照、可靠 ID 采集、恢复索引与并发继续去重。
- [ ] fabricd ACP 操作：Runtime 串行队列、操作引用、wait / read、有界输出与失效语义。
- [ ] fabricd PTY 投递：人工按键与文本 / Enter 统一排序、目标检查与投递状态。
- [ ] Tenant Agent 服务：发现、启动、投递、等待、读取，复用 SDK / Gateway 路由。
- [ ] MCP 接入：工具合同、会话凭据、跨 Tenant 拒绝、凭据脱敏。
- [ ] MCP 注入：managed ACP 配置、受支持 PTY 适配器与必要的 stdio bridge。
- [ ] 两产品协作交互：pending、操作进度、输出不完整与引用失效提示。
- [ ] 集成验收：双入口同队列、宿主 / fabricd 重启、A / B 输出关联、恢复旧配置及真实 Agent 互操作。

清单按产品交付顺序排列；底层依赖可先实现。一个功能涉及两仓库时各自提交，记录对应提交与验证。未通过的外部验收明确记录，不以 mock 或文档检查代替。

## 验证记录

- 2026-09-17：实施开始。Dune 初始 HEAD `40d2c42`，SandDance 初始 HEAD `e5a7db8`。工作区内已有本任务设计文档，先单独提交设计基线。尚未执行产品改动验证。
