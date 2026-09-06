# 元数据存储与离线迁移

工作台支持 SQLite 和 PostgreSQL。默认 `dune web --data /absolute/private-directory` 使用目录内的 `metadata.sqlite`，目录须私有并由当前用户持有；同一目录只允许一个 Dune 实例打开，不能通过共享盘运行多个实例。JSON 只用于旧版本导入，不再作为运行时后端。

## 选择 PostgreSQL

创建权限为 0600 的私有配置文件，填写部署系统提供的连接 URL：

```yaml
postgres:
  url: "postgres://USER:PASSWORD@HOST:5432/DATABASE?sslmode=verify-full"
```

官方入口使用 `dune --config /absolute/gateway.yaml web --database-config /absolute/database.yaml --url https://example.com/tools/dune/`。`--data` 和 `--database-config` 互斥。配置也可用单独的 `sqlite_dir: /absolute/private-directory` 选择 SQLite。不要将含密码的配置提交到仓库。

Go 宿主通过 `host.Options.Database` 传入 `storage.Config`，与 `DataDir` 互斥。`storage.Postgres.BeforeConnect` 为每条新物理连接接收独立的 pgx 配置副本，可以调用私有 SDK 更新鉴权信息；回调可能并发执行，须响应 context，所有连接仍须指向同一数据库/schema。已经建立的连接继续由同一连接池管理。

## 从旧 JSON 导入

1. 停止旧工作台并排空登录、注册和绑定等写入。保留 fabricd 运行；工作台断线不销毁 tmux。记录当前启动参数、公开地址和机器入口。
2. 将整个旧元数据目录复制到新的私有备份目录，例如 `cp -a /absolute/old-accounts /absolute/backup-accounts`。不要复制正在写入的目录。备份含密码和凭据哈希，应按原权限保存。
3. 使用新二进制，将源目录导入另一个空的 SQL 目标。导入命令不读取 Gateway 配置，也不启动网络服务。

```sh
# SQLite：目标须是新的私有目录，不能就地覆盖源目录。
dune metadata import-json \
  --source /absolute/old-accounts --data /absolute/new-sqlite

# 或直接导入空的 PostgreSQL 数据库/schema。
dune metadata import-json \
  --source /absolute/old-accounts --database-config /absolute/database.yaml
```

4. 核对返回的 accounts、sessions、enrollments、machines 数量。用新目录或数据库配置启动工作台，保持原公开 URL、Cookie 路径和机器 Gateway 地址；检查原账号登录、机器上线、Runtime 和 PTY 历史后开放写入。

导入始终持有源目录的旧 `accounts.lock`，源工作台未退出会拒绝导入。导入校验 JSON 版本、唯一键、账号/凭据唯一性及关联；目标业务表必须全部为空。账号 ID、原密码哈希、机器 ID、机器凭据哈希、会话及 enrollment 到期时间保持不变，过期状态不会续期。初始 Attached Runner ID 沿用 machine ID，不要求重新安装机器或重新签发身份。连接路由由机器重连建立，不导入 TCP 连接。

所有业务记录在一个 SQL 事务中导入。失败不会修改源文件；非空目标会拒绝重复导入。提交确认丢失会报告结果未知，此时先检查目标状态，不自动重放或清空目标。数据库 schema 初始化与业务导入分开，失败后可能存在没有业务记录的已初始化目标。

新后端尚未开放写入时，可使用旧二进制和原目录/备份回退。新后端已有写入后，必须停写并显式转换或恢复，不能直接切回旧目录丢弃新增状态。此工具不迁移开发机上的文件、tmux 内容或 Agent 配置；fabricd 重启与资源销毁仍遵守各自生命周期规则。

SQLite 转 PostgreSQL及 SQL 备份恢复工具仍待后续检查点交付，不能将 JSON 导入工具当作通用 SQL 导出工具。
