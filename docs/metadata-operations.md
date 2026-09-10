# 元数据结构与运维边界

Dune 支持单实例 SQLite 和 PostgreSQL。SQLite 目录必须是绝对路径、0700、由
当前用户持有；数据库文件为 0600，`metadata.lock` 阻止两个进程同时打开同一
目录。PostgreSQL 可由多个 Gateway 共享。

## 当前结构

Dune 只初始化空 schema，不维护迁移版本或旧结构兼容。建表在一个事务内完成；
PostgreSQL 使用 transaction advisory lock 串行化并发首次启动。每次打开都会校验
当前部署模式下精确的 `dune_*` 表与列集合；旧表、缺列或多列都会明确拒绝，不会
尝试就地修复。

| 逻辑事实 | 表 | 说明 |
| --- | --- | --- |
| 本地用户 | `dune_users` | 邮箱、密码哈希、enabled、认证版本 |
| 本地浏览器会话 | `dune_sessions` | token 哈希、用户、有效期、认证版本 |
| Runner 与机器绑定 | `dune_runners` | owner、类型、binding revision、机器凭据、enabled、Managed suspended |
| 一次性安装材料 | `dune_enrollments` | token 哈希、短期身份 scope、Runner/Fabric、有效期 |
| PostgreSQL owner 目录 | `dune_routes` | machine、owner boot/address、binding、epoch、租约 |

本地 SQLite 有前四张表；本地登录 PostgreSQL 有五张；使用企业身份的
PostgreSQL 省略本地用户与 Session，只保留后三张。索引不是额外逻辑表。

浏览器访问票据和 peer nonce 保存在 Gateway 内存，分页游标编码为无权限的
位置值。Managed operation、provider action、renewal、review 等状态属于外部
Managed 服务，不进入 Dune 数据库。

## PostgreSQL 配置

```yaml
postgres:
  url: "postgres://USER:PASSWORD@HOST:5432/DATABASE?sslmode=verify-full"
```

配置文件应为 0600 且不提交仓库。Go 宿主通过 `host.Options.Database` 传入
`storage.Config`。`BeforeConnect` 可为每条新物理连接更新私有认证信息，但必须
响应 context，并保持所有连接指向同一 database/schema。

## 不支持的操作

Dune 明确不实现：

- 数据库备份与还原命令；
- 历史状态回滚；
- 恢复代次或运行中副本与旧快照之间的协调；
- 自动 schema 迁移。

部署系统如需备份，应在 Dune 全部停机后使用自身数据库能力处理；恢复后的
数据库只能在确认所有旧 Gateway 已停止后作为一次新的整体部署启动。这是外部
运维责任，Dune 不验证或协调该流程，也不承诺备份时刻之后的外部副作用可回滚。

## 一致性边界

业务写入在事务内提交。若提交确认丢失，调用返回 outcome unknown，调用方只能
通过当前事实核对，不能自动重放写入。网络 EOF、peer 断开或 connector 重连同样
不代表原业务请求成功或失败。

Runner 撤销将 `enabled` 持久改为 false 并清除凭据；当前 Gateway 主动断开目标，
其他副本通过有效性轮询与 route 租约到期停止访问。没有每实例 ACK 表。

集群细节见 [peer 传输](peer-transport.md)，身份与请求授权见
[访问检查](access-checks.md)。
