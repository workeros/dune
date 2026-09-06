# User Story：从飞书使用企业远程开发环境

> 关联规范：[Dune 底层协议与 SDK](../spec.md) · [Gateway 与 daemon 协议](../runner-tunnel-protocol.md) · [企业平台集成](enterprise-platform-developer.md)

## 用户目标

作为企业开发者，我希望从飞书继续操作开发环境中的 Agent，接收实时进度，在需要时跳转企业工作台处理代码和终端操作。

企业可以只有传统云基础设施。飞书入口、研发工作台和 Runner 管理由企业基于 Dune SDK 构建或接入的上层产品提供，不以已有 Agent 平台为前提。

## 前置条件

- 企业已部署 Gateway，并在开发机或托管环境中运行 daemon。
- 企业上层服务负责飞书鉴权、业务用户权限以及执行环境关联。
- 企业已定义业务用户到 OS 账号的映射及允许使用的能力范围。
- 环境初始化配置和 Agent Profile 由上层保存。
- 飞书 Bridge 能调用上层服务及 Dune SDK，并处理飞书消息和卡片。

## 主流程

```text
开发者在飞书中选择项目或继续已有开发会话
  -> Bridge 验证飞书消息与用户身份
  -> 上层定位或创建 Runner，并选择执行环境
  -> 上层产生可信执行上下文
  -> 通过认证后的 Gateway 路由到 daemon
  -> 必要时完整传递环境或 Agent Profile
  -> daemon 在授权 OS 身份下初始化并启动 Agent
  -> ACP 或 PTY 交互经实时 Stream 返回
  -> Bridge 转换为飞书消息或卡片
  -> 开发者继续对话，或跳转上层工作台人工操作
```

如果 Bridge 调用 CreateRunner、StopRunner 等接口，这些都是企业上层的示意 API，不是 Dune SDK 接口。Profile 使用 kind=environment 或 kind=agent 的完整 payload，由 daemon 解析执行。

## 责任划分

Bridge 和企业上层负责：

- 飞书事件验证、消息去重、卡片更新及外部投递重试。
- 用户、群聊、项目和上层 Runner 的对应关系。
- 业务权限、用户到 OS 账号的映射和可信执行上下文。
- 多 Agent 分工、审批交互和结果交付。
- 执行环境创建、停止、恢复、克隆及销毁。
- 飞书消息记录与必要的业务记录，遵守企业数据政策。

Dune 负责认证授权后的连接、路由、能力调用和执行控制。Dune 不订阅飞书事件，不持久化管理飞书消息、Profile、Runner 或企业租户数据。

## 权限交互

群成员身份不能直接成为执行权限。Bridge 能发送卡片，不代表它能控制任意 Agent 或执行任意命令。

root daemon 根据可信上下文在授权 OS 身份下执行。Profile 可以选择 root，但不能突破上层授予的上限，冲突必须拒绝。非 root daemon 的全部请求使用同一 OS 用户，不承担业务用户级授权；入口鉴权和可信 Gateway 限制仍然有效。

Agent 发出的审批请求由 Bridge 或上层工作台呈现。用户同意 Agent 请求不能扩大 Gateway 与 daemon 已有的权限上限。

## 断线与接续

Bridge 断线时，Dune 不建立持久消息队列。恢复连接后可以读取当前可用运行状态和后续事件；短时内存缓冲不能作为完整会话历史。

开发者可以通过上层工作台连接同一执行环境，查看文件、操作终端或重新启动 Agent。Agent 会话恢复依赖执行环境内的原生会话数据及对应 adapter 或恢复命令。

## 验收条件

- 飞书入口使用真实的业务用户权限完成调用。
- Agent 实际运行在企业指定执行环境中。
- Profile 完整传递给 daemon 执行，不要求 Dune 持久化配置。
- 未授权群成员不能取得执行输入或写操作权限。
- Bridge 断线不促使 Dune 保存消息或自动重放有副作用的输入。
- 飞书记录由外部系统负责，代码和 Agent 会话内容留在执行环境。
