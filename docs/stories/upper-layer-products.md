# 基于 Dune 的上层产品与 Runner 模型

> 关联规范：[Dune 底层协议与 SDK](../spec.md) · [企业平台集成](enterprise-platform-developer.md)

## 产品边界

Dune 是开源底层协议及 SDK 实现，包含 Gateway 和运行于 sandbox、开发机或其他执行环境内的控制 daemon。

Dune 不管理 Runner、Tenant 或 Fabric，不持久化管理 Profile，不提供 UI，不持久化业务状态。它定义标准能力与扩展能力接口，具体执行能力由插件或 adapter 提供。Profile 完整传递给 daemon 解析执行。

以下模型用于说明可以基于 Dune 构建的上层封装，不是 Dune SDK 必须实现的业务资源。

## 六类上层产品

| 上层产品 | 用户场景 | 上层承担的工作 |
|---|---|---|
| 个人使用版本 | 远程控制个人电脑 | 个人开发机接入、项目及 Agent 会话入口、远程操作界面 |
| 个人生产力版本 | 控制通过云服务购买的多个 sandbox，满足不同场景 | 多环境选择、配置、费用及生命周期管理 |
| 家庭版本 | 类似 NAS，家庭共享一个 sandbox 宿主机 | 家庭成员入口、共享宿主机资源分配及授权边界 |
| 开源项目管理 | 基于 GitHub 自定义项目管理 | GitHub 集成、项目规则、开发任务和成果交付 |
| 小企业版本 | 小团队控制云上购买的多个 sandbox，对内对外提供服务 | 团队权限、多环境管理、内部研发及外部服务产品化 |
| 大公司版本 | 完整团队、租户、员工授权，接入业务研发与 IM 综合能力 | 企业组织与身份映射、Runner/Fabric 管理、研发工具和 IM 集成 |

这些产品可以组合部署。长期远程开发是优先场景，即时任务同样由上层实现，不要求底层拥有任务调度模型。小企业版本对外提供服务所需的发布、入口和访问策略由上层负责。

## Runner 是上层顶级对象

上层 Runner 组织一个开发工作单元，可以关联多个目录和多个 Agent。用户可以在同一 Runner 内切换项目目录、继续 Agent 会话或进行人工操作。

Runner 不等于单个 Agent，也不要求每个目录都创建一个 Runner。目录共享、授权边界和并发写入策略由上层结合执行环境能力制定。

Dune 协议使用上层注入的不透明标识进行关联和授权，不在底层重建 Runner、Tenant 或项目数据库。target_id、scope_id、execution_incarnation 和 runtime_id/runtime_generation 描述路由、授权作用域及执行实例，不取代上层资源管理。

## Fabric 是上层运行后端

Fabric 表达上层如何获得执行环境：

- attached：接入已有开发机或环境。
- managed：由上层创建和管理 sandbox、容器、Pod 或云主机等环境。

Fabric 的配置、凭证和 Runner 关联由上层维护。上层创建或定位环境后，通过 Dune SDK 连接目标 daemon。

用户直接选择 Fabric 和环境初始化配置。上层可以通过二次开发提供 preset，简化常用组合的选择。

## Profile 的保存与执行

上层保存环境 Profile 和 Agent Profile，决定命名、版本、选择和复用规则。调用时以 kind=environment 或 kind=agent 的完整 payload 传递给 daemon，由 daemon 解析执行，不由上层预先展开为命令。

Profile 可以包含环境准备、Agent 安装及启动命令。Agent 通过 ACP 或 PTY adapter 启动，也可以通过配置命令使用 Agent 自身的会话恢复能力。

环境和 Agent 初始化都可以创建 OS 账户，但必须获得授权。可信上下文给出执行身份和能力上限，Profile 无权扩大该上限。

## 停止、销毁、恢复和克隆

- 停止与销毁是不同的上层操作，必须明确对进程、文件及底层环境的影响。
- 恢复沿用同一上层 Runner。
- 克隆创建新的上层 Runner。
- save、load、checkpoint 是可选 Provider 能力，不是所有执行环境共有的保证。
- 能否恢复进程、内存、文件或 Agent 会话必须分别说明，不使用一个模糊状态代替。
- 任意既有 Agent 的自动发现与收编不属于已确认的底层能力。

例如 CreateRunner、StopRunner、DestroyRunner、RestoreRunner 和 CloneRunner 都只能作为上层示意 API 名称，不是 Dune SDK 接口。

daemon 仍负责授权范围内的进程与 PTY 执行控制。Runner 生命周期上移不意味着把所有进程控制也交给云基础设施。

## 上层 UI 与插件视图

标准能力使用上层统一界面，例如文件、终端、进程和 Agent 交互面板。

扩展能力可以提供类似 MCP UI 的插件视图，由上层宿主加载并通过受控交互接口调用 Dune。插件呈现界面不自动获得额外执行权限。

Dune 不提供工作台、插件市场或 UI 宿主。上层决定插件视图的安装、加载、权限交互及产品导航。

## 数据责任

上层保存需要持久化的 Runner、Tenant、Fabric 关联、配置和业务状态。代码、文件、Agent 原生历史及恢复资料留在执行环境。

Gateway 和 daemon 可以维护有界内存中的认证连接、路由、执行对象、输入所有权和传输缓冲。它们不是业务数据库，不提供持久消息投递或完整历史重放。

## 上层实现的共同验收条件

- 底层可在没有 Runner、Tenant 和 Fabric 管理服务时独立调用。
- 所有入口都经过认证授权，daemon 只接受可信 Gateway。
- Profile 不能突破可信上下文的 OS 身份与能力上限。
- 标准能力和扩展能力按实际发现结果呈现。
- 停止、销毁、恢复、克隆和可选保存能力各有明确语义。
- 业务状态、执行环境内容和底层内存运行状态的责任明确分离。
