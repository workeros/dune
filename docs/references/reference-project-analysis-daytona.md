# Dune 参考项目分析：Daytona

> 关联规范：[设计规范](../spec.md) · [传输协议](../runner-tunnel-protocol.md) · [调研索引](README.md)

## 调研基线

| 项 | 内容 |
|---|---|
| 来源 | [daytonaio/daytona](https://github.com/daytonaio/daytona) |
| 固定版本 | [`v0.190.0 / 01c502b`](https://github.com/daytonaio/daytona/tree/01c502bb1f1ff8f2885d0cd490e043736083dca8) |
| 许可证 | AGPL-3.0 |
| 关注点 | 开发环境、Toolbox、Files/Exec/Process/PTY/Git、workspace 初始化、preview、snapshot/volume |

## 来源事实

本文沿用已审阅的历史版本，不把后续产品或仓库结构作为事实来源。

Daytona 的 `Sandbox` 是开发执行环境；其 `Runner` 是创建、启动和回收 sandbox 的基础设施组件。`Toolbox` 提供文件、命令、进程、PTY 和 Git API。`Snapshot` 是创建输入，`Volume` 是独立持久存储对象，preview URL 用于访问端口。

来源 `Runner` 不等同于上层 Dune 应用可能采用的 Runner 对象，更不是 Dune 底层协议的必选资源。

## 对 Dune 底层可复用

- Toolbox 的能力拆分适合 daemon 接口参考：Files、Exec、Process、PTY、Git 和 Ports 可独立于 Agent 使用，并由实际实现/插件声明支持情况。
- 短命令的结果与长进程 handle 分开；进程接口可提供 status、wait、signal 和流式输出。
- Git 内部委托 Exec 时仍须校验本次 Git 操作范围；独立授予任意 Shell 可能获得更宽的 OS 权限，typed Git 限制不能替代进程权限边界。
- Profile 使用完整 `kind=environment|agent` payload；daemon 解析命令、步骤、cwd/env、超时及启动参数，上层负责命名和保存。
- 端口访问须验证监听/readiness 及调用权限，不能仅根据端口号拼接可访问 URL。
- 能力描述分别表达版本、状态和约束，不将一种 provider 的行为冒充普遍保证。

## 上层参考

Docker/Kubernetes 等资源适配、Runner 生命周期、Fabric 关联、镜像构建、预热池、Snapshot/Volume 和公网 preview 属于上层平台。上层决定环境分配后何时执行 Profile，以及初始化失败时保留还是回收资源。

停止、销毁、恢复和克隆由上层结合 Provider 能力实现；恢复可保留上层 Runner 身份，克隆创建新身份。Dune 底层只管理当前有效连接和由自身启动的进程控制状态。

## 不纳入核心

不复制 Sandbox/Runner/Snapshot/Volume 业务资源或中央元数据库；不要求 Daytona 的 proxy 拓扑；不提供开发环境 UI、资源回收策略或 Profile 配置库。接口分析与实现代码复用分开，许可证记录保留来源基线。

## 证据链接

- Go SDK 的 Sandbox façade 与 preview API：[sandbox.go](https://github.com/daytonaio/daytona/blob/01c502bb1f1ff8f2885d0cd490e043736083dca8/libs/sdk-go/pkg/daytona/sandbox.go)。
- Process 的 execute、session 和 PTY surface：[process.go](https://github.com/daytonaio/daytona/blob/01c502bb1f1ff8f2885d0cd490e043736083dca8/libs/sdk-go/pkg/daytona/process.go)。
- Git clone 及结构化选项：[git.go](https://github.com/daytonaio/daytona/blob/01c502bb1f1ff8f2885d0cd490e043736083dca8/libs/sdk-go/pkg/daytona/git.go) 与 [git options](https://github.com/daytonaio/daytona/blob/01c502bb1f1ff8f2885d0cd490e043736083dca8/libs/sdk-go/pkg/options/git.go)。
- CLI MCP 暴露的 preview readiness 检查：[preview_link.go](https://github.com/daytonaio/daytona/blob/01c502bb1f1ff8f2885d0cd490e043736083dca8/apps/cli/mcp/tools/preview_link.go)。
- Toolbox API 的文件、进程、Git 等接口：[toolbox client](https://github.com/daytonaio/daytona/tree/01c502bb1f1ff8f2885d0cd490e043736083dca8/libs/toolbox-api-client/src)。
- Sandbox、Snapshot 和 Volume 是独立实体：[sandbox entities](https://github.com/daytonaio/daytona/tree/01c502bb1f1ff8f2885d0cd490e043736083dca8/apps/api/src/sandbox/entities)。
