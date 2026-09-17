# 服务端 Profile 管理

`pkg/profiles` 提供可嵌入的 Profile 配置库，供 Dune Web、SandDance 和其他宿主共用。
配置代码由 Dune 维护，数据保存在运行该模块的服务端数据库。Runner / fabricd
不保存配置库、不提供 `agent.config`，每次执行接收完整 `api.Profile`。

## 数据与授权

- `profiles.Record` 包含 `owner_id`、名称、描述、创建主体、时间和完整 Profile。
  `owner_id` 在个人 Web 中是用户 ID，在 SandDance 中是空间 ID。
- `dune_profiles` 保存身份、类型和当前修订；`dune_profile_revisions` 保存每个
  修订的完整输入。SQLite 与 PostgreSQL 使用同一语义。
- `List` 返回当前版本，`Get` 接受 `profiles.Selection{ID, Revision}`。修订 0
  用于读取最新配置供编辑；执行或 IM 绑定必须选择正整数修订。
- `Update` 和 `Delete` 要求当前修订，陈旧提交返回 `ErrConflict`。编辑生成新
  修订，旧修订保持原内容；Profile 的 environment/agent 类型不能变更。
- 删除会删除配置及其修订。已接受的环境创建由宿主保存完整输入快照；IM 等保留
  配置引用的调用方需要处理配置被删除的情况，不得静默切换到其他配置或最新版本。
- 所有查询和写入都带 owner 条件。存储包接收的是可信宿主调用；它不接收浏览器
  cookie，也不判断空间角色。宿主必须先验证主体和 owner 的访问权限，不能直接
  信任客户端提交的 `owner_id` 或创建主体。

显式写入 Profile 的环境变量随配置存入服务端数据库。数据库访问、凭据保护与
备份策略由部署方负责；接口使用认证、归属检查和 `Cache-Control: no-store`，
配置正文、argv/env 不写入运行日志。Agent 自身的登录文件继续由 Agent 管理。

## 宿主接入

在宿主的 SQL 初始化事务中调用 `profiles.CreateSchema(ctx, tx)`，之后用
`profiles.NewStore(db)` 借用现有连接池。宿主拥有连接池和事务；模块不会关闭它们。
需要把产品授权或空间生命周期与配置写入放在同一事务时，使用
`profiles.NewTransaction(tx)`。删除空间时可在该事务内调用 `profiles.DeleteOwner`。
多实例初始化须串行化；与 Dune `host.Open` 共用 PostgreSQL schema 时采用其
初始化 advisory lock `1146441285`。原型阶段只创建当前结构，不导入旧配置或迁移旧表。

SandDance 的 `internal/storage/profiles.go` 负责空间成员检查并调用这个包。
创建配置时锁定空间行，删除空间和 Profile 在同一事务内完成，避免并发创建留下
孤立配置。资源创建、环境变量合并、初始化状态与机器人权限仍由 SandDance 管理。

事务提交返回错误时用 `ErrCommitUnknown` 表示结果未知。HTTP 返回 `RESULT_UNKNOWN`；
调用方先读取状态核对，不能自动重放创建、更新或删除。

## 个人 Web

Profile 管理入口独立于开发机，未接入机器或机器离线时也可以保存。编辑器支持
完整准备步骤、启动命令、显式 Shell、工作目录、环境变量、超时和 PTY/ACP。
选择 Agent Profile 和工作目录后，Web 提交完整 Profile，经 Gateway 由 fabricd 执行。

| API | 行为 |
| --- | --- |
| `GET /api/v1/profiles` | 当前用户的配置列表 |
| `POST /api/v1/profiles` | 保存 `name`、`description`、完整 `profile` |
| `GET /api/v1/profiles/{id}?revision=N` | 读取指定修订；省略时返回最新修订 |
| `PUT /api/v1/profiles/{id}` | 带当前 `revision` 和新内容创建下一修订 |
| `DELETE /api/v1/profiles/{id}?revision=N` | 按当前修订删除 |

租户宿主不开放上述个人 API。SandDance 保留自己的空间 API 与界面，内部复用
公共存储；Go 调用方直接使用 `profiles.Record` 和 `profiles.Selection`。

## 本次验证（2026-09-17）

改动位于 Dune `main` 和 SandDance `dev` 工作区。SandDance 的主模块与 IM 模块
当前均通过相邻目录 replace 使用 Dune；未发布新的远端模块版本。

- Dune 全量 Go 包已覆盖；初次运行发现既有握手拒绝测试误把连接关闭当成接受，
  修正断言并检查请求未到达 fabricd 后，定向重复和 Gateway race 检查通过。
- 启用独立 PostgreSQL 后，`./tests` 的全量跨进程测试超过默认 180 秒总时限；
  用 `go test ./tests -count=1 -timeout=600s` 重跑，193 秒通过。
- `go test -race ./pkg/profiles ./pkg/gateway ./internal/metadata ./internal/webapp ./pkg/host`
  在 SQLite / 独立 PostgreSQL 下通过，覆盖归属隔离、不可变版本、并发修改和宿主事务。
- `make test-im-race check-go` 通过；`make build web-check web-build` 和 Web 单元测试通过。
- SandDance `GOWORK=off go test ./...`、`go vet ./...` 通过；配置独立 PostgreSQL、
  当前 Dune 二进制及 tmux 后，storage / integration 的 race 回归通过，包含真实
  Gateway、fabricd 与本地 PTY/ACP 测试进程。测试不调用真实模型服务。
- SandDance 前端类型检查、生产构建和 `profiles.spec.ts` 三个浏览器用例通过。
- Dune 本地浏览器验证了无 Runner 时创建完整 Agent Profile、回读准备步骤/参数/
  超时、编辑生成新修订、640px 窗口入口与表单，以及 Escape 关闭后焦点返回入口。

构建仍有包体积建议警告。未进行真实 Agent、飞书平台或远端部署验收。临时
PostgreSQL、预览服务和超时测试遗留的专用 tmux server 已停止。
