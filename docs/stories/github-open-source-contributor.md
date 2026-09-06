# User Story：GitHub 开源贡献者

> 关联规范：[Dune 底层协议与 SDK](../spec.md) · [Gateway 与 daemon 协议](../runner-tunnel-protocol.md) · [上层产品模型](upper-layer-products.md)

## 用户目标

作为开源贡献者，我希望让 Agent 协助理解 Issue、修改代码、运行测试并准备 Pull Request，由我检查变更并决定是否提交和推送。

该流程由基于 Dune 的上层工作台或 GitHub 集成产品提供。即时任务和长期开发环境都可以使用相同底层协议。

## 前置条件

- 上层集成能够读取用户授权的 Issue、PR 或任务描述。
- 上层管理执行环境及其 Runner、Fabric 关联。
- 环境 Profile 能准备仓库、目标分支和依赖。
- Agent Profile 能通过 ACP 或 PTY 启动目标 Agent。
- 仓库凭证通过受控机制提供给执行环境。

## 主流程

```text
贡献者或 GitHub 集成获取任务
  -> 上层选择长期 Runner 或准备本次任务的执行环境
  -> 完整传递环境 Profile
  -> daemon 在授权身份与目录范围内执行初始化
  -> 完整传递 Agent Profile，启动 Agent
  -> Agent 修改代码并运行测试
  -> 实时结果返回上层界面
  -> 贡献者检查文件、测试输出和 Git diff
  -> 必要时继续对话、打开终端或启动其他 Agent
  -> 按授权执行 commit / push，并由上层创建 PR
  -> 上层决定保留、停止或销毁环境
```

上层若提供 CreateRunner、StopRunner 或 DestroyRunner，这些名称仅代表上层示意 API，不属于 Dune SDK。

## 集成边界

GitHub App、Webhook receiver 和其他上层服务负责事件订阅、身份验证、任务状态、重试、评论、Checks 及 PR 创建。Dune 不内置 GitHub 业务对象，也不持久化外部 delivery 状态。

环境 Profile 负责项目准备；Agent Profile 负责 Agent 初始化和启动。Profile 由上层保存与管理，调用时以 kind=environment 或 kind=agent 的完整 payload 传递，由 daemon 解析执行。初始化产生的文件留在执行环境。

## 权限与交付

- 标准 Git 能力应区分读取和写入授权。
- GitHub 身份不能自动扩大执行环境的权限。
- 任意 Shell 的授权范围必须按实际 OS 能力评估；仅限制 typed Git 写接口不能约束已有 Shell 权限。
- Profile 请求的 OS 身份不能超过可信上下文的授权上限。
- GitHub token、代码和 Agent 内容不得进入 Dune 的持久内容日志。
- 上层应在销毁环境前完成用户要求的成果交付，明确未提交修改的处置。

## 异常流程

初始化失败时，daemon 返回可判断的执行状态和实时输出，上层决定诊断或清理环境。已执行步骤可能留下副作用，结果未知时不得盲目重放。

测试失败或 Agent 退出后，上层可以保留环境供继续排查。Agent 退出与环境销毁不是同一操作。

网络断开后，Dune 不提供永久事件重放。上层结合当前执行状态和执行环境内的文件判断任务结果，不把 transport ACK 当作 Agent 完成任务的证明。

## 验收条件

- 任务可以在已有开发环境或临时托管环境中执行。
- 贡献者能在推送前检查代码和测试结果。
- GitHub 事件和 PR 生命周期由上层管理。
- Dune 仅执行经过认证授权的能力调用。
- 环境保留、停止和销毁由上层决定，不能由 Agent 一轮结束隐式替代。
- 代码、会话数据和恢复资料留在执行环境，Dune 不保存业务状态。
