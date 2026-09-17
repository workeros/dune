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

## 个人布局与已读

| 方法和路径 | 行为 |
| --- | --- |
| `GET {prefix}/workbench/views/{id}` | 获取当前用户的布局；未保存时返回修订 0 和空 root |
| `PUT {prefix}/workbench/views/{id}` | 保存 `revision`、`root`、`focus_pane`、`review_pane`，修订冲突返回 409 |
| `PUT {prefix}/workbench/read-markers` | 保存 `{target, epoch, sequence}`，仅向前更新，返回实际已读位置 |
| `POST {prefix}/workbench/read-markers/query` | 批量读取 `{items: [{target, epoch}]}`，最多 64 项，未读位置为 0 |

两个接口从认证上下文取得身份命名空间与用户 ID，不接受正文指定用户。同 Tenant 的其他成员拥有各自的布局和已读位置。建议工作台默认使用布局 ID `main`。

`root` 为 null 或二叉分屏树。叶子格式为 `{id, pane: {target, project_id?, directory_id?, session_record_id?}}`；分屏格式为 `{id, direction: "horizontal" | "vertical", ratio, children: [first, second]}`，ratio 为 0.1..0.9。最多 32 个 pane、16 层深度，节点 ID 唯一；焦点和固定审阅目标必须指向当前树内的 pane。`review_pane` 为空时由界面跟随焦点，不固定审阅目标。

`target` 包含完整 Runner `binding` 和 `{id, incarnation, generation, adapter}` 形式的 `runtime`。同一执行身份不能出现两次；再次打开时界面定位原 pane。保存只检查 Runner 的 Tenant 归属，允许保留已禁用或已替换 Runner 的旧引用以显示失效项；重新连接和任何输入仍必须校验实际绑定、Runtime 和权限，保存布局不会自动创建执行。

已读位置在 `(Owner, 用户命名空间, 用户, 目标执行身份, epoch)` 内单调递增。另一个用户、一个新 Runtime 或一个新事件 epoch 从自己的位置开始。布局保存冲突不自动覆盖服务器版本，也不让另一窗口的保存强制改变当前焦点。
