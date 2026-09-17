# 工作台元数据 API

Dune 个人模式的前缀为 `/api/v1`，SandDance / Tenant 模式为 `/api/v1/tenants/{tenant}`。请求使用宿主登录身份和现有同源请求检查。Tenant 是待授权的作用域，正文不能提供 Owner 或用户身份；项目对该 Tenant 的成员共享。

## 项目

| 方法和路径 | 行为 |
| --- | --- |
| `GET {prefix}/projects?limit=32&cursor=…` | 按 ID 分页，最多 100 条，返回 `items` 和可选 `next_cursor` |
| `POST {prefix}/projects` | 创建项目，返回 201 和修订 1 |
| `GET {prefix}/projects/{id}` | 获取当前项目 |
| `PUT {prefix}/projects/{id}` | 携带当前 `revision` 更新，冲突返回 409 |
| `DELETE {prefix}/projects/{id}?revision=…` | 删除项目元数据；不停止 Runtime，不删除 checkout / worktree |

创建和更新正文：

```json
{
  "revision": 0,
  "name": "Dune",
  "directories": [
    {
      "id": "local-checkout",
      "binding": {"runner_id": "runner-id", "fabric_id": "attached", "machine_id": "machine-id", "revision": 1},
      "path": "/workspace/dune"
    }
  ],
  "default_profile": {"id": "profile-id", "revision": 3}
}
```

创建省略 `revision` 或传 0；更新必须传已读取的正数修订。默认 Profile 可省略，选择时必须属于同一 Owner / Tenant 且为 Agent Profile。目录最多 32 个，可来自不同 Runner，`id` 在项目内稳定且唯一。`repository` 可记录 worktree 所属仓库的绝对路径。保存时检查 Runner 归属和绑定，执行时仍需重新检查绑定和目录；保存目录引用并不证明路径存在。

响应包含 `id`、`owner_id`、`revision`、`created_at`、`updated_at` 及正文中的项目配置。旧绑定返回 `BINDING_CHANGED`，旧修订返回 `CONFLICT`；数据库提交结果未知返回 `RESULT_UNKNOWN`，调用方先查询，不自动重试写入。
