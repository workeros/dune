# User Story：通过 SDK 与协议集成 Agent

> 关联规范：[Dune 底层协议与 SDK](../spec.md) · [Gateway 与 daemon 协议](../runner-tunnel-protocol.md)
>
> 本文件保留历史路径。Dune 不提供管理 Runner、Tenant 或 Fabric 的业务 OpenAPI；这些 API 如有需要由上层产品实现。

## 用户目标

作为集成开发者，我希望使用 Dune SDK 连接已准备好的执行环境，提交完整初始化配置，启动 Agent 并完成多轮交互，同时能够独立使用文件、终端和其他执行能力。

## 前置条件

- 执行环境已由上层或运维系统准备，并运行 daemon。
- Gateway 已配置认证、授权和目标路由，不接受匿名调用。
- 上层向连接注入可信上下文和不透明关联标识。
- 必需的能力插件及 ACP 或 PTY adapter 已安装。
- 环境与 Agent Profile 由上层保存，本次请求提供完整配置。

## 主流程

```text
上层通过 SDK 认证连接 Gateway
  -> Gateway 验证权限并路由到目标 daemon
  -> 发现实际能力及其版本、限制和扩展
  -> 完整传递环境 Profile
  -> daemon 校验执行身份与授权范围，解析并执行
  -> 完整传递 Agent Profile
  -> adapter 启动 Agent，返回受控进程与连接信息
  -> 建立 ACP 或 PTY 实时交互
  -> 多轮输入、输出和必要的审批往返
  -> 按需调用其他能力
  -> 显式停止目标进程，或断开客户端连接
```

环境创建和销毁、Runner 状态变化、业务任务完成均在此底层流程之外，由上层协调。底层使用 target_id、scope_id、execution_incarnation 及 runtime_id/runtime_generation 关联目标、授权作用域和执行实例，不内建上层 Runner 模型。

## 标准与扩展能力

Dune 定义标准能力和扩展能力接口，具体行为由插件或 adapter 实现。调用方根据发现结果使用能力，不能将未知扩展当作基础能力。

不支持的能力、版本不匹配、授权失败和执行失败必须可区分。能力不可用时不得静默降级成权限更宽的 Shell 操作。

## Profile 与 OS 身份

完整 Profile 经受控连接传递给 daemon，kind 为 environment 或 agent。daemon 解析执行，不向 Dune 配置数据库登记 Profile，也不要求上层先将 Profile 展开为命令。Profile 的持久保存、命名、版本、选择和复用策略均属于上层。

环境和 Agent 初始化可以创建 OS 账户，但必须得到对应授权。root daemon 可以切换至授权 OS 身份；Profile 指定身份与可信上下文冲突时必须拒绝。

非 root daemon 的请求统一使用 daemon 自身 OS 用户。上层仍须验证业务用户权限，Gateway 仍须认证授权，daemon 仍只接受可信 Gateway。

## 多轮 Agent 交互

ACP adapter 负责 Agent 协议适配，PTY adapter 提供终端交互。Gateway 不解释 Agent 对话和业务审批含义。

ACP 调用方匹配请求与响应，识别一轮完成或错误。PTY 调用方不能把终端静默直接解释成 Agent 完成。一轮交互结束不要求停止进程。

Dune 不收编任意既有 Agent。已有会话需要通过 Agent 支持的恢复命令或 adapter 能力重新打开，是否支持必须明确声明。

## 审批往返

```text
Agent 发出审批请求
  -> adapter 与 Stream 转发给上层
  -> 上层界面或策略处理请求
  -> 当前授权输入方返回响应
  -> 响应到达同一个有效 Agent 进程实例
```

审批响应必须对应当前会话与进程实例，不能因重连误投给另一个实例。用户对 Agent 的批准不能提升可信执行上下文授予的能力或 OS 身份上限。

## 状态与断线

Gateway 和 daemon 可以维护连接、路由、进程、输入所有权、背压及短时缓冲等内存运行状态。这些状态不构成 Runner 数据库或持久投递账本。

客户端断线不等于停止进程。缓冲超窗必须报告缺口；无法确认副作用是否执行时应返回未知状态，调用方不得自动重放输入。

Agent、daemon 或主机重启后的恢复依赖实际 backend 能力。Dune 不从不存在的内存缓冲恢复完整历史。

## 内容与清理

工作内容和 Agent 原生会话数据留在执行环境。Dune 不持久化 prompt、输出、Profile 或业务历史，Gateway 只转发实时内容。

停止受控进程属于 daemon 执行控制；销毁容器、云主机、开发机或上层 Runner 属于上层生命周期管理。清理只能作用于授权且确认归属的对象。

## 验收条件

- SDK 无需创建 Dune Runner、Tenant 或 Fabric 即可调用底层能力。
- 匿名连接、越权能力和超限 OS 身份均被拒绝。
- daemon 接收完整 Profile，完成授权后的初始化和 Agent 启动。
- ACP 与 PTY 通过各自 adapter 工作，多轮交互可持续进行。
- 单个 Agent 不可用时，其他已授权能力仍可独立调用。
- 断线缺口和未知执行结果被明确报告。
- Dune 不保存业务状态，允许有界内存运行状态。
