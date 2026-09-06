# Dune 参考项目分析：E2B

> 关联规范：[设计规范](../spec.md) · [传输协议](../runner-tunnel-protocol.md) · [调研索引](README.md)

## 调研基线

| 项 | 内容 |
|---|---|
| 来源 | [e2b-dev/infra](https://github.com/e2b-dev/infra) |
| 固定版本 | [`9c21387`](https://github.com/e2b-dev/infra/tree/9c213875bc6ecb7f2f01d494aa837bb738fce807) |
| 许可证 | Apache-2.0 |
| 关注点 | Firecracker sandbox 分配、template build、envd、control/data plane、readiness、pause/resume |

## 来源事实

E2B 将版本化 `Template`、Firecracker `Sandbox`、调度生命周期的 `Orchestrator`、来宾内 `envd` 和外部 `Client Proxy` 组合成 Managed sandbox 平台。envd 提供命令、文件和进程 API；snapshot/pause/resume 是 provider 的虚机连续性能力。

## 对 Dune 底层可复用

- envd 展示了执行能力驻留环境内的边界：daemon 承载接口实现/插件，Gateway 和 SDK 无需理解调度器内部对象。
- 连接建立、协议版本、daemon 版本和各能力 ready 分开表达；“环境 API 已返回”不等于目标已可执行操作。
- 管理通道与工作负载端口分开授权，公开服务端口不能同时公开 daemon 管理权限。
- 路由引用和可猜测的 sandbox ID 只用于寻址，不能作为调用凭据。
- 模板供应链与已运行环境内的步骤分开；后者可由 daemon 解析执行 `kind=environment` Profile payload。
- 握手、执行实例及重连绑定需防止旧凭据/旧 frame 被误用；具体保证只覆盖协议声明的有效状态范围。

## 上层参考

Template build、镜像、微虚机分配、placement、多区域、预热与自动扩缩容属于上层 Fabric/Provider。上层组合 allocation、daemon、Profile 和能力 readiness 后决定 Runner 是否开放使用；Dune 不维护该资源状态机。

Provider 的 pause/resume/save/load/checkpoint 可由上层 SDK 封装。保存文件系统、保存内存及恢复进程是不同能力，需如实声明；恢复保留上层 Runner 身份，克隆创建新身份。恢复产生新的底层执行实例时，旧连接凭据及进程 handle 不自动有效。

## 不纳入核心

不引入 E2B 调度集群、Redis 路由目录、模板服务、资源数据库或 Snapshot 管理器。不将云分配设为唯一接入方式。Dune 接收和执行完整 Profile payload，但不保存其配置资源、Runner 元数据或 envd 输出历史。Agent 在用户磁盘上的历史可以由上层按实际存储能力处理，底层不持久化不意味着禁止用户环境快照保存这些文件。

## 证据链接

- E2B 组件边界和请求路径：[Architecture: overview](https://github.com/e2b-dev/infra/blob/9c213875bc6ecb7f2f01d494aa837bb738fce807/docs/ARCHITECTURE.md#L11-L28)。
- Template build 与 snapshot 供应链：[Architecture: templates](https://github.com/e2b-dev/infra/blob/9c213875bc6ecb7f2f01d494aa837bb738fce807/docs/ARCHITECTURE.md#L103-L135)。
- Orchestrator、node 与 placement：[Architecture: orchestration](https://github.com/e2b-dev/infra/blob/9c213875bc6ecb7f2f01d494aa837bb738fce807/docs/ARCHITECTURE.md#L193-L223)。
- envd 与 sandbox 内 API：[envd OpenAPI](https://github.com/e2b-dev/infra/blob/9c213875bc6ecb7f2f01d494aa837bb738fce807/packages/envd/spec/envd.yaml#L10-L15)。
- pause/resume 与 snapshot 路径：[Architecture: lifecycle](https://github.com/e2b-dev/infra/blob/9c213875bc6ecb7f2f01d494aa837bb738fce807/docs/ARCHITECTURE.md#L398-L456)。
- 网络和 client proxy 路径：[Architecture: networking](https://github.com/e2b-dev/infra/blob/9c213875bc6ecb7f2f01d494aa837bb738fce807/docs/ARCHITECTURE.md#L303-L332)。
