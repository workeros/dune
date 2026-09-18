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

两个接口从认证上下文取得身份命名空间与用户 ID，不接受正文指定用户。同 Tenant 的其他成员拥有各自的布局。建议工作台默认使用布局 ID `main`。

`root` 为 null 或二叉分屏树。叶子格式为 `{id, pane: {target, project_id?, directory_id?, session_record_id?}}`；分屏格式为 `{id, direction: "horizontal" | "vertical", ratio, children: [first, second]}`，ratio 为 0.1..0.9。最多 32 个 pane、16 层深度，节点 ID 唯一；焦点和固定审阅目标必须指向当前树内的 pane。`review_pane` 为空时由界面跟随焦点，不固定审阅目标。

`target` 包含完整 Runner `binding` 和 `{id, incarnation, generation, adapter}` 形式的 `runtime`。同一执行身份不能出现两次；再次打开时界面定位原 pane。保存只检查 Runner 的 Tenant 归属，允许保留已禁用或已替换 Runner 的旧引用以显示失效项；重新连接和任何输入仍必须校验实际绑定、Runtime 和权限，保存布局不会自动创建执行。

未读提示仅保存在当前浏览器工作台的内存中，页面重新加载后重新计算，不调用后端或跨浏览器同步。新 Runtime 或新的活动 epoch 不继承旧位置。布局保存冲突不自动覆盖服务器版本，也不让另一窗口的保存强制改变当前焦点。

## 活动摘要

现有 `runtime.list` / `runtime.get` 的 Runtime 增加 `activity`，包含 `state`（unknown、working、idle、blocked）、`source`、可选 `agent` / `foreground`、`epoch` 和 `sequence`。运行进程仍使用 Runtime 的 `state` 字段，活动状态不替代进程状态，也不证明某条 prompt 已成功。

managed ACP 由 controller 更新摘要，权限待处理为 blocked，普通调用执行中为 working，准备就绪且没有错误为 idle。PTY 当前根据 tmux 前台命令提供识别信息；未取得原生状态集成时始终返回 unknown，不通过静默时长、屏幕文本或进程存活推断空闲。原始 ACP 透传同样保留 unknown。

摘要不包含消息正文或权限参数，列表无需建立每个 Runtime 的内容订阅。活动变化增加 sequence，单纯轮询不增加。fabricd 重启后即使 tmux Runtime 继续存在，活动 epoch 仍重新生成，旧已读位置不能用于新的活动序列。

## 个人 Web 接入

Dune 工作台使用 `main` 布局；400 ms 合并连续改动，每次保存完成后才提交下一版本。保存失败或修订冲突时停止自动写入，保留本地现场并提供“加载已保存布局”。另一个浏览器登录同一账号可读取数据库中的布局；浏览器存储不作为布局真相。

项目编辑支持多个 Runner 目录和固定修订的默认 Agent 配置，可以明确更新到配置的当前修订。启动条使用已有在线 Runner，提交固定配置选择和 cwd，由共享服务解析、保存实际快照再启动；可以选择当前目录或新 worktree。启动与部分成功合同见 [Agent 启动](agent-launch.md)。原生 ID 采集与继续入口尚未完成，失效 pane 暂时只显示原因和新建入口。

工作台分页读取 Runner 后，按最多四个并发请求获取活动摘要；每四秒刷新，后台页面暂停轮询。只有打开的 pane 建立完整内容订阅。跨 Runner 分屏采用稳定叶子组件，调整树结构只移动或缩放会话；重复打开聚焦已有 pane。临时摘要读取失败保留已打开连接和错误提示，实际输入仍经服务端检查执行身份。
