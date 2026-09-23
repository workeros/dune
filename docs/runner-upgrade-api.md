# Runner 在线升级 API

Dune 负责安装、执行、核验和本次失败回滚；宿主负责当前成员权限及获准发行。
升级只改变连接器及完整发行目录，不升级或重建已有 ACP 宿主、Agent 和 tmux 会话。
当前交付进度与验证限制见 [实现记录](runner-online-upgrade.md)。

## Go 接入

`app.RunnerUpgrader()` 返回 `upgrade.Service`。每次调用携带 `upgrade.Scope`，包含当前
`Principal`、`OwnerID` 和完整 `runner.Binding`，由宿主重新授权。绑定中的普通路由 epoch
不参与逻辑 binding 身份，重新 enrollment 或 binding revision 变化不能自动跟随。

| 方法 | 输入 | 返回 |
| --- | --- | --- |
| `InspectRunner` | scope | 实际运行映像、完整安装及修订、支持状态 |
| `PreviewUpgrade` | scope、`PreviewRequest` | 源/目标发行、更新和重启计划、两端预检及宿主合同证据 |
| `StartUpgrade` | scope、`Request` | 持久接纳的 `Observation`；不会等待执行完成 |
| `GetUpgrade` | scope、`Query` | 原任务执行事实及观测新鲜度 |
| `ListUpgrades` | scope、`ListRequest` | 活动任务和有界分页历史 |

相同能力提供于 `pkg/client.Client`，可由 `pkg/sdk.Dial` 获得；所有操作经过 Gateway，
没有直接调用 Runner 安装目录的外部入口。

配置 `host.Options.UpgradeSource` 为 `upgrade.Source`，按固定的 `ReleaseRef` 和平台解析
获准的完整清单。清单必须含 Dune、tmux、rg、许可证的 SHA-256、字节数及权限，归档 URL
和 SHA-256，以及当前共享状态合同。目标必须可下载；初始本地安装回执不会虚构下载地址。
使用 `Manifest.Digest()` 计算按路径排序的标准 JSON 摘要，发布者和调用方保留这个固定引用。
可用 `upgrade.ReadCatalog` / `NewCatalog` 加载打包脚本生成的目录；CLI 配置为 `web --upgrade-catalog /absolute/upgrade-catalog.json`。
未配置发行源时检查和历史仍可用，目标解析明确返回 `UPGRADE_SOURCE_UNAVAILABLE`。

`host.Options.UpgradeObservations` 可注入 `upgrade.ObservationStore`；默认使用宿主配置的
SQLite 或 PostgreSQL 元数据库。多个 host 使用同一数据库或同一注入存储。宿主先持久
保留原请求再派发一次；它不能用自己的记录代替执行端接纳，也不能因为响应丢失而重发。

## HTTP

五个入口均为 `POST /api/v1/runners/{runner}/upgrade/{action}`，其中 action 为
`inspect`、`preview`、`start`、`get`、`list`。沿用现有登录、Origin、`X-Dune-Request: 1`
及 Runner 选择参数：`machine_id`、`fabric_id`、`revision`。JSON 内的 binding 必须与
选择参数一致，不能用操作 ID 绕过权限。`inspect` body 为 `{}`。

`start` 请求示例：

```json
{
  "submission_id": "upgrade-request-001",
  "binding": {
    "runner_id": "runner-a",
    "machine_id": "machine-a",
    "fabric_id": "attached",
    "revision": 7
  },
  "installation_id": "installation-a",
  "expected_installation_revision": "18",
  "expected_running_sha256": "<actual SHA-256 from inspection>",
  "release": {
    "id": "release-2026-09-23",
    "manifest_sha256": "<Manifest.Digest() result>"
  }
}
```

正常 `start` 返回 HTTP 202 和 `Observation`，查看 `operation.admission`，不能将 HTTP 状态
或传输层 `accepted` 当成升级完成。拒绝也有持久操作记录；相同键不同请求返回冲突。
`get` 使用 binding、installation_id，以及 submission_id 或 operation_id 中的一个。
`list` 使用 binding、installation_id、可选 limit（1–100）和上页返回的 opaque cursor。
`preview` 使用 binding 和 release；预览没有锁定安装，执行时仍会复核。

## 结果语义

`admission` 区分 `accepted`、`not_accepted`、`unknown`、`expired`。只有实际执行端持久
接纳才能返回 accepted。响应中断时保留原 submission_id，只查询。宿主重启后同一已保留
请求也只查询，不重发；未能发送的请求仍可能保持 unknown，不能自行声称安全重试。

`phase` 是执行阶段，`confirmed` 是执行方已持久确认终结，`rollback` 单独说明原版本是否
恢复并核验。`succeeded` 必须具有本次尝试的目标实际映像、完整发行、Gateway 接纳和正常
路由证明。旧目标的迟到证明不能结束回滚。`recovery_blocked` 保留启动封闭和恢复材料。

`freshness` 为 `live`、`last_known` 或 `unreachable`，`observed_at` 为宿主最后观察时间。
离线结果保留原任务，不推断新状态；`observation_issue` 说明当前不可达、任务不存在或安装
发生变化等观察问题。未获授权时即使存在缓存也拒绝。任务历史与当前版本独立查询。

执行端保留最新 128 个终态和至少 24 小时详情，过期后保留原键墓碑；每安装最多 4096 个
提交键，不重用旧键容量。宿主最多保留每安装 4096 个提交/观察。分页还受响应大小预算约束。
默认升级总期限 10 分钟、下载 2 分钟、检查 20 秒、服务重启 30 秒、目标确认 90 秒、回滚
2 分钟。页面等待期限不改变执行期限，自动恢复不延长原预算。

`running_from_selected_release` 区分仍执行旧发行 inode 的连接器。即使磁盘全组件和 Dune SHA 均等于目标，这种进程仍需要重启，不能返回 `already_current`。同 SHA 更新的最终证明还必须包含本次启动尝试。

本机离线诊断用 `upgrade-status --root … [--operation …]`，读取当前活动或最近任务；修复后用 `upgrade-recover --root … --operation … --revision …` 显式给予一次新的回滚预算。两者都不接纳新的升级。
