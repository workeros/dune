# 元数据存储与恢复

工作台支持 SQLite 和 PostgreSQL。默认 `dune web --data /absolute/private-directory` 使用目录内的 `metadata.sqlite`，目录须私有并由当前用户持有；`metadata.lock` 保证同一目录只由一个实例打开，不能通过共享盘运行多个实例。

当前处于初始开发阶段，不维护旧数据、旧 schema 或旧客户端兼容逻辑。空库在一个事务内创建完整当前结构；PostgreSQL 使用事务内锁协调并发初始化。数据库保存结构指纹，结构不匹配则拒绝打开。结构变更后选择新的开发目录或数据库；程序不会自动转换、升级或删除已有数据。

## 选择 PostgreSQL

创建权限为 0600 的私有配置文件，填写部署系统提供的连接 URL：

```yaml
postgres:
  url: "postgres://USER:PASSWORD@HOST:5432/DATABASE?sslmode=verify-full"
```

官方入口使用 `dune --config /absolute/gateway.yaml web --database-config /absolute/database.yaml --url https://example.com/tools/dune/`。`--data` 和 `--database-config` 互斥。配置也可用单独的 `sqlite_dir: /absolute/private-directory` 选择 SQLite。不要将含密码的配置提交到仓库。

Go 宿主通过 `host.Options.Database` 传入 `storage.Config`，与 `DataDir` 互斥。`storage.Postgres.BeforeConnect` 为每条新物理连接接收独立的 pgx 配置副本，可以调用私有 SDK 更新鉴权信息；回调可能并发执行，须响应 context，所有连接仍须指向同一数据库/schema。已经建立的连接继续由同一连接池管理。

## 备份与恢复

备份用于恢复同一当前结构的数据。SQLite 先停止工作台，再将整个私有目录复制到新的备份目录；恢复时复制到单独的私有目录，核对数据后显式切换 `--data`。不要复制正在写入的数据库主文件并遗漏 WAL。备份包含密码和凭据哈希，保持原私有权限。

PostgreSQL 使用发行版提供的 [pg_dump](https://www.postgresql.org/docs/17/app-pgdump.html) 和 [pg_restore](https://www.postgresql.org/docs/17/app-pgrestore.html)。停止 Dune 写入后备份到私有目录，恢复到为本次恢复准备的空数据库；使用匹配服务器版本的工具，连接和密码由私有 libpq service 配置提供。例如：

```sh
umask 077
pg_dump --dbname=service=dune_backup --format=custom --file=/private/dune.dump
pg_restore --list /private/dune.dump
pg_restore --dbname=service=dune_restore --single-transaction --exit-on-error \
  --no-owner --no-privileges /private/dune.dump
```

`dune_backup` 和 `dune_restore` 分别指向源库和新恢复库；恢复账号须具有建表权限，目标服务账号的权限由部署方配置。核对当前结构指纹、各表数据、原登录与机器身份，再切换应用。恢复不会延长已过期会话或凭据，也不能恢复备份之后的新增记录。保留原库直到验收完成，禁止用恢复备份的方式隐式丢弃已开放的新写入。

## 集群恢复代次

已有集群记录的 PostgreSQL 备份恢复后，须停止所有旧实例，核对数据库，生成新的恢复代次并更新所有实例配置，再允许机器重连。不能直接使用备份中的代次启动服务。离线工具支持先读取，再对明确的原代次进行一次条件旋转：

```sh
dune metadata cluster-recovery --database-config /absolute/private/database.yaml
dune metadata cluster-recovery --database-config /absolute/private/database.yaml \
  --rotate-from ORIGINAL_GENERATION
```

成功返回 `outcome: changed` 和新 `generation`；工具自己生成随机新代次，不接受指定历史代次。未初始化集群记录的单机数据库和 SQLite 会拒绝旋转。原机器、用户及工作内容不删除，历史 route 留作核对，但不具有新代次的路由权限。数据库事务不能停止外部旧服务；停站、配置更新和重新接入仍是恢复流程的一部分。

提交回执丢失时，命令以非零状态返回，并输出 `outcome: unknown` 与本次候选代次。此时只运行不带 `--rotate-from` 的读取命令：若数据库已是候选代次，则原操作已提交；不要盲目再旋转。若读取失败或出现第三个代次，先核对恢复操作的并发与数据库状态。该工具不恢复 Agent 或上游资源，也不替代 S3 的 owner 期限和 epoch 握手。

## 身份源与本次登录主体

启用 OIDC 后，仅接受该 issuer 签发身份对应的 Dune 会话；原本地会话不能用于企业模式，不同 issuer 的会话也不能混用。切回本地模式后，未撤销且未过期的本地会话仍可能有效；配置切换不等同于永久撤销全部旧会话。需要永久撤销时通过可信宿主管理入口停用对应用户。

首次企业登录按 issuer + subject 新建独立用户，不根据邮箱接管本地账号、机器或 Runner。关联既有 Dune 用户使用下述明确授权的身份关联流程，不通过改邮箱或直接改 SQL 绕过此边界。Dune 用户停用会同时阻止企业新登录并撤销已有会话；上游停用自动同步尚未实现，不能将回调成功或短期会话视为持续上游授权证明。

会话保存本次登录验证的 namespace 与 subject；CLI 子会话、短期连接凭据和 Attached 安装材料保留同一引用。持续访问复核原主体，不查询关联列表来替换身份。备份恢复保留原到期时间、授权版本和父子会话关系；备份之后的撤销需由部署方核对并重新执行。

## 显式关联既有账号

可信宿主管理员先认证实际操作者、授权这次身份关联，核验既有 Dune principal 与稳定外部身份的归属，再调用 `App.LinkIdentity`。应在用户首次企业登录前完成；已归属另一 principal 的外部身份会冲突，当前不提供账号合并或身份转移。`Actor` 必须来自管理员认证结果，不能直接信任普通用户传入的值。`Reason` 保存非敏感核验依据或审批单引用，禁止放入密码、令牌或工作内容。

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

## 业务状态与恢复边界

备份保存 Runner/机器绑定、操作意图与不可变创建参数、提供方动作键与已知资源引用、续期巡检计划、业务互斥、worker 租约、连接目录与准入预约。恢复不延长任何期限、不复活旧进程权限，也不触发提供方调用。外部操作可能已在备份之后发生，恢复服务后先核对原操作与资源事实；无法确认时保留 unknown，不能重放创建、续期或销毁。

Managed 模板来自启动配置，不在数据库中编辑。新建时先针对配置中的精确 Fabric、模板 ID 和版本做访问检查，再校验声明的字段类型、范围与大小；事务只保存规范化后的公开参数快照和摘要，不保存提供方 Secret 或私有 SDK 设置。已禁用版本不再用于新建，但部署方应在仍有对应资源或未完成操作时保留查询、核对和清理所需的适配配置。

配置准入和集群装配见 [peer 传输接入](peer-transport.md)，访问凭据与执行授权见 [访问检查](access-checks.md)，回归选择见 [开发流程](workflow.md)。

提供方动作在调用前写入独立稳定键，预约提交不确定时不会给予派发资格。派发结果未知的记录不能通过租约接管、恢复备份或普通重试变回首次派发。新 worker 只能核对原动作；明确完成后才保存阶段结果和资源事实。首次资源确认时间不因查询或恢复重置。提供方自身的去重、旧执行者隔离和可靠核对仍由适配器保证，SQL 租约不构成外部系统的隔离机制。

创建执行器只在首次动作预约得到明确提交且执行权复核通过后调用一次适配器 `Create`。后续进入同一动作时，包括 timeout、unknown、进程重启、租约接管和结果提交回执丢失，只能调用 `Reconcile` 查询原动作关联；已完成动作不再访问提供方。适配器返回错误时附带结果会被忽略：deadline 记为 timed_out，其他错误记为 unknown；已核验的部分引用应以 unknown 事实正常返回。查询未找到不能自行触发再次创建。

Managed 创建 worker 按数据库时钟扫描仍处于 create 阶段且执行租约已到期的 Operation；扫描结果在 Runner 与 Operation 锁内再次检查，避免把已经进入 Bootstrap 的旧快照重新领取。集群副本竞争同一 SQL 租约，调用期间定期续约，提供方 context 到期与保存结果使用不同 context，因此本次调用超时仍能持久记录 timed_out。终态动作立即交还执行租约；unknown/timed_out 保留本次租约作为最短核对间隔，租约到期后只核对原动作。交还执行租约不释放 Runner 业务互斥，也不清除 unknown。

Managed Bootstrap 的提供方动作和一次性 enrollment 哈希由一个事务提交。令牌限定原创建 Operation、已确认的 Fabric/resource_ref、Runner 与 binding revision，明文只在首次明确提交后返回；提交结果未知或重复进入只返回原动作供核对，不重新生成令牌。fabricd 消费时重新锁定并检查 principal、Runner、Operation、动作、资源引用、有效期和访问门，再把机器身份绑定到已有 Managed Runner；并发消费只有一份机器凭据能返回。普通 Attached enrollment 仍创建新 Runner，两种令牌在访问检查中使用各自的 `attached`/`managed` 子操作。

Bootstrap executor 固定规范化的公开 enrollment 地址、完整 Gateway WS(S) 地址、安装版本和令牌有效期，并用它们生成非敏感动作摘要。首次明确提交同时得到动作和明文 grant 后才调用 `Bootstrap`；此后只用原动作调用 `ReconcileBootstrap`，即使当前启动配置已变化也不改写旧动作。若原 grant 已被 fabricd 消费，已有机器与同一 Runner/Fabric/binding revision 的绑定是本地可信完成证据，可直接结束原 Bootstrap 动作而无需查询提供方。适配器错误只记录 timeout/unknown，不采信伴随错误返回的字段。

Managed worker 在同一持久循环中依次处理到期巡检、Bootstrap 与 create，每次只执行一个提供方调用。候选 SQL 只扫描本进程已配置对应能力的 Fabric，避免较早的未配置任务占满批次；Bootstrap 在锁内重新检查 create 成功、资源引用、访问门、数据库时钟有效期与原动作状态。Bootstrap 终态进入等待连接并交还执行租约，unknown/timed_out 保留租约作为最短核对间隔，接管后仍只查询原动作。

Managed enrollment 被消费及 Bootstrap 动作成功后，创建 Operation 仍停留在等待连接。机器凭据的 Gateway 握手先确认 fabricd 输入 grant；集群模式再发布当前 owner 路由，然后通过有界可信回调提交首次在线事实。元数据事务重新锁定 Runner 和原创建 Operation，核对机器绑定、binding revision、Create/Bootstrap 终态、resource_ref、访问门及数据库时钟有效期，才将创建标成 succeeded。PostgreSQL 集群还要求回调携带当前恢复代次和 epoch，并与刚发布、未过期的完整路由绑定相同；SQLite 或没有集群记录的 PostgreSQL 要求路由字段为空。回调完成前本机路由不对业务流可见，失败时握手和已发布 owner 都会撤销。提交回执未知不会在同一握手内重试；已提交的结果由后续重连幂等核对。Attached 机器不进入 Managed 生命周期事务。

资源一经确认便进入独立巡检计划，不等待首次连接。公开 `fabric.InspectProvider` 只能按固定 Runner、Fabric、resource_ref 与 binding revision 读取事实，不携带动作键，也不授予创建、续期或删除权限。适配器错误和 deadline 分别保存为 unknown/timed_out，并丢弃伴随字段；只有无错误、引用一致的 confirmed 结果可以更新到期时间或确认 Gone。确认 Gone 在同一事务关闭访问并删除 Managed enrollment 与机器身份，网络错误或本地到期时间不会触发该变化。

巡检候选、领取和续租都使用数据库时钟。每个资源的策略版本、最后事实、观测时间、下次检查时间与固定 `renew_until` 决定持久化在同一行；多副本通过独立执行修订和短租约竞争，锁内再次核对资源引用、绑定和访问状态。个人策略基于首次资源确认时间和持久 create/Bootstrap 状态计算；首次在线会重新激活因未连接而停止的计划，曾经可用后不会重套首次连接宽限期。当前 `renew_until` 只是后续续期 Operation 的冻结输入，不构成提供方调用或续期成功；续期动作执行、宿主装配和状态 API 仍须由后续检查点完成。
