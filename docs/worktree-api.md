# Runner worktree 准备

worktree 操作走 SDK → Gateway → fabricd，使用现有 Runner，复用 Git common-directory 锁。宿主负责权限与项目目录登记；fabricd 不创建云环境。

| 执行 API | 输入 | 返回 |
| --- | --- | --- |
| `worktree.list` | `directory`：原 checkout 或 worktree 的绝对路径 | worktree 数组，含 path、head、branch、detached、bare、locked、prunable |
| `worktree.create` | `directory`、`path`（新目录）、`branch`（新分支）、可选 `ref`（默认 HEAD） | 经 Git 检查确认的新 worktree |

SDK 对应 `Worktrees` / `CreateWorktree`。创建会先解析 ref 到具体 commit，再创建新分支和 checkout。当前目录的未提交更改不复制、不提交；原分支和 checkout 保留。目标路径必须不存在，分支不能已存在，不使用 force/reset，不自动删除已有目录。

ref、分支和路径通过结构化 argv 交给 Git，不拼接 shell。Git 执行和最终检查失败时不自动重做；超时或创建后无法确认返回 `RESULT_UNKNOWN`，可通过 list 检查实际目录。共享仓库的两个工作区使用同一 Git 锁；外部 Git 进程仍由 Git 自己的锁约束。

API 只准备目录。统一启动服务选择该 cwd，Runtime 保留项目标签；没有进程退出后的恢复档案。
