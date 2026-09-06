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

## SQLite 转 PostgreSQL

停止 SQLite 工作台并备份后，将现有元数据复制到空 PostgreSQL 目标：

```sh
dune metadata copy-sqlite \
  --source /absolute/current-sqlite --database-config /absolute/database.yaml
```

源库须已存在且 schema 与当前二进制匹配；源目录独占锁一直保持到复制结束，不会为缺失或不受支持的源库初始化数据。复制使用一个源快照和一个目标事务，保留全部当前业务字段，包括 Runner/Fabric 关联和绑定修订；输出各表行数。不复制网络连接，不进行双写，也不重放业务动作。

核对行数后，以目标数据库配置和原部署地址启动工作台。检查原会话、机器自动重连、Runtime 身份和 PTY 历史，再开放写入。重复复制到非空目标会失败。结果未知与开放写入后的回退限制同 JSON 导入；本命令不提供 PostgreSQL 到 SQLite 的反向转换。

## 备份与恢复

SQLite 可以在停站后通过同一工具复制到新的私有目录：

```sh
dune metadata copy-sqlite \
  --source /absolute/current-sqlite --data /absolute/backup-sqlite

# 恢复到另一个空目录，验证后显式切换 web --data。
dune metadata copy-sqlite \
  --source /absolute/backup-sqlite --data /absolute/restored-sqlite
```

复制得到当前 schema 的独立 SQLite 数据库，原密码、凭据哈希和到期时间不变。不要只复制正在使用的 `metadata.sqlite` 主文件而遗漏 WAL；本工具要求源工作台退出，通过数据库事务读取一致状态。

PostgreSQL 使用发行版提供的 [pg_dump](https://www.postgresql.org/docs/17/app-pgdump.html) 和 [pg_restore](https://www.postgresql.org/docs/17/app-pgrestore.html)。停止 Dune 写入后备份到私有目录，恢复到为本次恢复准备的空数据库；使用匹配服务器版本的工具，连接和密码由私有 libpq service 配置提供。例如：

```sh
umask 077
pg_dump --dbname=service=dune_backup --format=custom --file=/private/dune.dump
pg_restore --list /private/dune.dump
pg_restore --dbname=service=dune_restore --single-transaction --exit-on-error \
  --no-owner --no-privileges /private/dune.dump
```

`dune_backup` 和 `dune_restore` 分别指向源库和新恢复库；恢复账号须具有建表权限，目标服务账号的权限由部署方配置。核对 schema 版本、各表数据、原登录与机器身份，再切换应用。恢复不会延长已过期会话或凭据，也不能恢复备份之后的新增记录。保留原库直到验收完成，禁止用恢复备份的方式隐式丢弃已开放的新写入。

当前已验证 schema 6 的 JSON 导入、SQLite 转 PostgreSQL、SQLite 复制恢复及 PostgreSQL 17 的原生备份恢复。后续 schema/版本升级必须补充对应验证，不能据此承诺任意版本可混用或回退。

## 从 schema 1 升级到 2

schema 2 增加用户启用状态和授权版本；现有用户默认启用，原会话、密码、机器和 Runner 身份保持有效。停用后的授权版本和会话版本不匹配时拒绝访问，重新启用不能恢复旧会话或已撤销安装命令。

停止旧工作台并备份后，再启动新二进制。首次打开数据库会在迁移锁和一个事务内升级到当前 schema；中途失败会回滚全部 ALTER 和版本标记。SQLite 停站备份可复制完整私有目录，或用旧二进制的 `copy-sqlite`；新版本的 `copy-sqlite` 只接受当前 schema 的源库，不在备份过程中升级源库。PostgreSQL 须停止所有旧应用实例后再升级，此版本组合不支持混合运行。

升级后检查原账号、机器自动重连和现有 Runtime，再开放写入。旧二进制不能打开 schema 2；回退须遵守前述停写和恢复边界。已验证 schema 1 → 2 在 SQLite/PostgreSQL 上的事务回滚与身份保留，并通过旧 schema 1 工作台二进制到当前版本的真实 PTY 升级演练；没有承诺任意降级路径。

## 升级到 schema 3 与身份源切换

schema 3 增加外部身份关联、一次性登录事务和会话身份源。schema 1/2 经同一迁移事务升级；默认本地模式下原会话继续有效。停站、备份及禁止混合旧版本实例的要求同上。转库和备份包含外部身份、登录事务及会话身份源，不包含上游令牌。

启用 OIDC 后，仅接受该 issuer 签发身份对应的 Dune 会话；原本地会话不能用于企业模式，不同 issuer 的会话也不能混用。切回本地模式后，未撤销且未过期的本地会话仍可能有效；配置切换不等同于永久撤销全部旧会话。需要永久撤销时通过可信宿主管理入口停用对应用户。

首次企业登录按 issuer + subject 新建独立用户，不根据邮箱接管本地账号、机器或 Runner。迁移既有本地归属使用下述明确授权的身份关联流程，不通过改邮箱或直接改 SQL 绕过此边界。Dune 用户停用会同时阻止企业新登录并撤销已有会话；上游停用自动同步尚未实现，不能将回调成功或短期会话视为持续上游授权证明。

## schema 4：显式关联既有账号

schema 4 增加成功关联决定的持久记录，随同一事务后端保存和备份。升级仍须停站、备份并停止所有旧版本实例；默认登录、机器与 Runner 身份不因升级改变。

可信宿主管理员先认证实际操作者、授权这次迁移，核验既有 Dune principal 与稳定外部身份的归属，再调用 `App.LinkIdentity`。应在用户首次企业登录前完成；已归属另一 principal 的外部身份会冲突，当前不提供账号合并或身份转移。`Actor` 必须来自管理员认证结果，不能直接信任普通用户传入的值。`Reason` 保存非敏感核验依据或审批单引用，禁止放入密码、令牌或工作内容。

```go
// verifiedPrincipalID、verifiedIssuer 和 verifiedSubject 来自宿主核验流程。
decision := identity.LinkRequest{
    RequestID: approval.RequestID, Actor: administrator.ID,
    PrincipalID: verifiedPrincipalID,
    Namespace: verifiedIssuer, Subject: verifiedSubject,
    Reason: approval.Reference,
}
record, err := app.LinkIdentity(ctx, decision)
```

成功时身份关联、审计记录、用户授权版本递增、已有会话及待消费 enrollment 撤销一起提交。新登录获得原 principal ID，保留原机器、Runner、运行中的 Runtime 和历史；已有用户连接按撤销机制关闭。身份源和本地密码凭据本身不被删除，站点允许的登录方式仍由配置决定。

请求 ID 与所有决定字段相同的重试返回原记录，不再次撤销会话；复用请求 ID 修改决定或把外部身份关联给另一个用户均失败。提交回执丢失时，先调用 `App.IdentityLink(ctx, decision.RequestID)` 核对持久记录与完整决定；查不到或查询失败不能据此宣称先前未提交，不自动换请求 ID 重试。查询同样由宿主授权。此表只记录成功关联决定，失败的身份核验、拒绝和其他管理员活动由宿主审计，不声称覆盖所有审计事件。

## schema 5：短期连接访问凭据

schema 5 增加数据库内一次性访问凭据，替换工作台内部的进程内 ticket。只保存凭据哈希、原会话哈希引用，以及签发时的用户、身份源、机器、Runner、Fabric、绑定修订和授权版本。凭据在 30 秒内用于一次连接建立，同一会话最多 64 个待消费凭据；数据库原子消费后不允许重放。签发和消费的提交结果未知时均不自动重试。

已建立连接继续检查原会话与固定绑定，退出、停用、身份关联或机器撤销使其失效；绑定修订变化不允许旧访问自动转向新绑定。会话和机器删除同时清除未消费凭据。连接建立期限不是会话续期，删除已消费 ticket 也不代表注销已有连接。

升级沿用停站与禁止混合版本实例的要求。旧进程内 ticket 不迁移，旧连接随工作台停站结束；原账号、机器配置和远端 Runtime 保留。备份/转库包含仍存在的凭据哈希和原到期时间，不延长到期。共享 PostgreSQL 的实例可签发/消费同一凭据；目前仍须显式将连接拨到持有目标 fabricd 的 Gateway，自动目录、peer 路由和接管属于后续 S3。

## Runner 查询与既有绑定

Runner 查询复用现有 `dune_runners` 与 `dune_machines`，不增加 schema。既有 Runner ID 和机器 ID 即使相同，也保持各自语义；新 Attached 注册分别生成两个 ID，仍在消费 enrollment 的同一事务提交。JSON 导入继续沿用旧机器 ID 作为初始 Runner ID，旧机器配置与 Runtime 无需重建。

客户端通过 Runner 查询取得完整绑定快照，再以该快照交换访问凭据。签发时复核数据库当前绑定，PostgreSQL 行锁覆盖该修订；已建立访问继续检查原机器、Runner、Fabric 和修订。旧请求不能因为逻辑 Runner 相同而转向新机器。当前不提供直接编辑绑定的公共入口，Managed 绑定变更须由后续生命周期事务实现。

## schema 6：人类 CLI 登录

会话新增 `kind` 和 `parent_hash`，既有会话默认仍为浏览器会话。新增十分钟 CLI 登录事务，记录站点、身份源、客户端 proof 的哈希以及明确确认的浏览器会话引用。确认不返回 CLI 凭据；只有持有原客户端 proof 的 CLI 可以原子消费并建立子会话。确认后的身份不可替换，消费与子会话创建同事务提交，回执丢失不重放。

CLI 子会话最长八小时，且不得超过父浏览器会话期限；认证、短期凭据签发和已建立连接检查均复核父会话、身份源与授权版本。父会话删除级联撤销已确认请求、CLI 子会话和待消费访问凭据。CLI 会话使用独立类型前缀，不能作为浏览器 Cookie 或机器凭据。

升级须停站、备份并停止旧版本实例。SQL 转库按父会话先于子会话复制，完整保留登录事务、父子引用与原期限；PostgreSQL 原生恢复也验证原 CLI 会话和访问凭据。恢复不会延长有效期；备份之后的撤销不会存在于旧备份中，恢复前须按部署方要求重新执行相关撤销。CLI 本地的私有凭据文件独立保管，不在服务端数据库或备份中保存原令牌。
