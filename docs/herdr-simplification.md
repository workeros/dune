# herdr 工作台存储精简

2026-09-18 用户确认的新范围，取代原方案中的退出恢复和已读位置落库要求。原实施记录保留为历史验证证据，不代表这些能力仍属于当前范围。

## 目标

herdr 新增的持久化只保留三张表：`dune_projects`（项目及目录）、`dune_views`（个人布局）、`dune_agent_credentials`（MCP 调用方凭据）。SandDance 外部身份模式加上原来的五张 Dune 表，共八张；SandDance 自身业务表不计入其中。

存活 Agent 可重新连接；进程退出后显示已结束，不自动重启，也不提供工作台“继续恢复”。当前原生会话与活动从 fabricd 读取；队列、操作状态和有界输出仍归 fabricd。保留 PTY / ACP、项目、当前目录 / worktree 启动及 Tenant MCP 通信。

MCP 凭据直接绑定已确认的 Tenant、调用方、Runner 和完整 Runtime 身份，每次调用仍经 SDK / Gateway 校验归属和存活。它不依赖恢复档案。未读提示只在浏览器工作台内存中记录，重新加载后重新计算。

不把删除的表改名或把恢复档案塞入其他表，不保留旧 API 兼容入口，不自动迁移或删除现有业务数据库。

## 实施与验证

- [x] 删除已读位置表、存储类型和 HTTP API；两产品改用浏览器内存。
- [x] MCP 凭据直接绑定调用方和执行实例，移除会话恢复依赖。
- [ ] 删除持久会话档案及 Runtime 会话索引、退出恢复服务和界面入口。
- [ ] 两产品启动及发现只使用当前 Runtime；保留项目关联、部分失败结果及未知结果不重放。
- [ ] 同步文档和 SandDance 依赖，完成 Go / Web / 多进程与浏览器验收。

已读改动验证：SQLite / PostgreSQL 的 metadata、webapp、workbench 回归与 vet 通过；两端类型检查通过；各一项 Chromium 用例验证查看、新活动、重新加载和零已读 API 请求。SandDance 共享工作台路由回归使用当前 Dune workspace 通过。完整构建与整体验收随其余裁剪完成后执行。

凭据改动验证：SQLite / PostgreSQL 的签发、轮换、撤销、跨租户隔离、绑定校验及数据库重开回归通过；身份、授权、metadata、webapp、host 全包回归通过，凭据与 MCP 定向 race 和相关 vet 通过。凭据直接保存完整调用方与目标，每个 MCP 请求再经 Gateway 校验当前执行实例。
