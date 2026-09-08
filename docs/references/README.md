# Dune 参考项目调研索引

> 关联规范：[设计规范](../spec.md) · [传输协议](../runner-tunnel-protocol.md)

本目录记录源码调研及其对 Dune 的启发，不是第二份规范。各文档按自己的调研日期、固定版本、许可证和证据链接阅读；修订 Dune 职责映射不代表重新验证来源项目的新版本或许可证。

## 阅读边界

以下职责划分主要描述底层目标设计。Dune 当前还包含个人工作台、身份/SQL 元数据模块及开发机状态持久化；具体已实现行为以代码为准，不用底层边界否定上层功能。新增调研应分别说明目标设计、当前实现和待验证建议。

Dune 交付底层协议、SDK、Gateway 和 daemon。统一调用路径为 `SDK -> Gateway -> daemon -> 能力实现/插件`；Gateway 可独立部署，也可与其他组件合并部署。转发文件、终端或 ACP 数据不意味着保存这些内容。

- 来源项目的 `Sandbox`、`Session`、`Runner`、`Device` 等沿用来源含义，不强行映射为 Dune 资源。
- Runner、Tenant、Fabric、资源分配、停止/销毁、保存/恢复、配置存储、业务任务与 UI 属于上层封装。
- Profile 是底层可校验、解析和执行的完整 payload，`kind=environment|agent`；Profile 的保存、命名、修订和复用由上层负责。
- 标准能力接口与扩展能力接口由协议约定，实际能力由 daemon 承载的实现或插件声明。插件 UI 属于上层产品。
- daemon 可使用 root 多用户或 non-root 单用户部署；两种入口均须鉴权。业务身份及 OS 身份/ACL 映射由上层配置，daemon 执行授权边界。
- Gateway 和 daemon 仅维护运行所需的有界内存状态，不建立元数据数据库、内容库、执行 journal 或持久去重表。进程 handle 按有效进程实例存活；凭证、输入租约和短时缓存按各自规则过期，不将统一 TTL 强加给所有运行状态。
- 通过 Files 写入用户目录及 Agent 自身保存内容不等于 Dune 建立存储服务。
- 同一存活进程可在断线后重连；daemon 重启、环境恢复和新进程不能靠旧内存句柄假定连续。Agent 可由启动命令调用其原生恢复能力，不接管任意既有进程。
- 上层恢复可保留 Runner 身份，克隆产生新 Runner；Dune 负责当前执行实例和连接的有效性，不规定上层业务对象的终态。

## 项目索引

| 项目 | Dune 底层参考 | 上层封装参考 | 文档 |
|---|---|---|---|
| BotMux | PTY 后端、输入权、短断线恢复 | 多 Agent 终端产品 | [BotMux](reference-project-analysis-botmux.md) |
| Cloudflare Sandbox SDK | SDK/执行服务分层、能力调用、副作用重试 | 云资源生命周期、平台 tunnel | [Cloudflare Sandbox](reference-project-analysis-cloudflare-sandbox.md) |
| Daytona | Files/Exec/Process/PTY/Git 接口 | 开发环境分配、Snapshot/Volume、preview | [Daytona](reference-project-analysis-daytona.md) |
| DeepSeek Harness | ACP 互操作、能力接口、取消与结算、远程执行 provider 差距 | Agent loop、插件组合、会话事件与子 Agent | [DeepSeek Harness](reference-project-analysis-deepseek-harness.md) |
| E2B | 来宾 daemon、能力 readiness、代理路由 | 微虚机调度、模板、pause/resume | [E2B](reference-project-analysis-e2b.md) |
| Modal Sandboxes | 进程 handle、结构化能力约束 | 资源请求、GPU、Volume、Snapshot | [Modal](reference-project-analysis-modal-sandboxes.md) |
| OpenChamber | 远程连接、凭证分层、PTY 同步 | 设备配对与远程开发 UI | [OpenChamber](reference-project-analysis-openchamber.md) |
| OpenClaw | 启动配置、能力授权、doctor | 身份接入、Agent 产品、channel | [OpenClaw](reference-project-analysis-openclaw-2.md) |
| Vercel Sandbox | 进程 handle、文件接口、重试边界 | 持久 Sandbox、运行实例、恢复/克隆 | [Vercel Sandbox](reference-project-analysis-vercel-sandbox.md) |

## 维护要求

1. 保留来源自己的对象名、固定版本、许可证记录及证据；架构调整不能改写来源事实。
2. 分开说明“来源事实”“对 Dune 底层可复用”“上层参考”“不纳入核心”。映射是设计判断，不是源码事实。
3. 不将来源项目的完整平台、持久化或 UI 隐含引入 Dune；也不禁止上层按需实现这些功能。
4. 重试、去重、重连和恢复建议必须说明内存状态有效范围，不承诺跨重启的 exactly-once 或完整回放。
5. 新增协议保证需要同步修改设计规范、传输协议和互操作验证，不仅修改调研文档。
