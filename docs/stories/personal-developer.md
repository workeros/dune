# User Story：个人开发者长期远程开发

> 关联规范：[Dune 底层协议与 SDK](../spec.md) · [Gateway 与 daemon 协议](../runner-tunnel-protocol.md) · [上层产品模型](upper-layer-products.md)

## 用户目标

作为个人开发者，我希望通过自己选择的开发工作台，远程操作开发机或托管环境中的 Agent，长期维护多个项目，在 Agent 工作和人工操作之间切换。

开发工作台是基于 Dune 构建的上层产品。Dune 提供协议、SDK、Gateway 和执行环境内的 daemon，不提供工作台 UI，也不管理 Runner、Fabric 或用户项目。

## 前置条件

- 上层工作台已接入经过认证的 Gateway，并能定位目标 daemon。
- 开发机已运行 daemon；托管环境由上层通过云基础设施准备。
- 上层保存环境初始化配置和 Agent Profile。
- 环境安装了所需标准能力或扩展能力的插件、adapter。
- 上层为本次连接提供可信执行上下文，明确允许的操作、目录和 OS 身份。

## 主流程

```text
开发者打开上层工作台
  -> 选择或创建上层 Runner
  -> 直接选择 Fabric 和环境初始化配置
  -> 上层创建托管环境或连接已有开发机
  -> 通过 SDK 建立经过认证授权的 Gateway 连接
  -> 向 daemon 完整传递环境 Profile
  -> daemon 校验授权并解析执行
  -> 选择工作目录和 Agent Profile
  -> daemon 通过 ACP 或 PTY adapter 启动 Agent
  -> 开发者持续对话、查看文件、运行测试和调试服务
  -> 按需切换目录、启动其他 Agent 或人工终端
  -> 退出工作台，稍后重新连接
```

Runner 的创建、停止、恢复、克隆和销毁都是上层产品操作，不是 Dune SDK 的 Runner API。Profile 使用完整 payload，kind 为 environment 或 agent，由 daemon 解析执行，上层不将其展开为底层命令。

## 长期使用

上层 Runner 是开发者组织长期工作的顶级对象，可以关联多个工作目录和多个 Agent。上层负责保存项目、会话关联及所需业务状态；代码、Agent 原生会话文件和其他工作内容留在执行环境。

客户端断线本身不应成为停止 Agent 的业务策略。Dune 可以维护有界内存连接状态和输出缓冲，但不提供持久会话历史。重新连接时，超出缓冲窗口必须明确报告缺口。

需要恢复 Agent 会话时，上层可以通过 Profile 中的恢复命令调用 Agent 自身能力。Dune 不承诺收编用户在任意终端中自行启动的 Agent，也不把进程重新启动等同于会话已经恢复。

## 人工操作与多个 Agent

- Files、Exec、Process、PTY、Ports、Git 等能力由已安装的能力插件或 adapter 提供。
- 标准能力可以由上层工作台提供统一界面。
- 扩展能力可以由上层加载插件视图，并通过受控接口与 Dune 交互。
- Agent 进程、终端和其他受控进程分别维护执行身份和连接状态。
- 上层决定 Agent 分工、目录分配和并发写入规则；Dune 不编排 Agent。
- 单个 Agent 退出不意味着其他 Agent、目录或开发环境应当销毁。

## 停止、恢复与销毁

上层区分停止和销毁。停止保留哪些文件、会话和进程状态取决于环境与 Provider 能力；销毁是否删除工作目录必须由上层明确告知用户。

恢复沿用同一个上层 Runner，克隆创建新的上层 Runner。save、load、checkpoint 属于可选 Provider 能力，缺失时不得表现为可用。主机重启、进程丢失和内存状态恢复的保证必须按实际能力声明。

## 验收条件

- 同一上层 Runner 可以组织多个目录和多个 Agent。
- 开发者能通过经过认证的连接持续操作目标开发环境。
- daemon 解析完整 Profile，并拒绝超出可信上下文授权上限的执行身份。
- Agent 由 ACP 或 PTY adapter 启动；可配置 Agent 自身的恢复命令。
- 断线后的历史缺口、恢复限制和不支持的能力均明确呈现。
- Dune 不持久化管理 Runner、Profile、用户项目或会话业务数据。
