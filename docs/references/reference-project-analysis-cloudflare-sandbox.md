# Dune 参考项目分析：Cloudflare Sandbox SDK

> 关联规范：[设计规范](../spec.md) · [传输协议](../runner-tunnel-protocol.md) · [调研索引](README.md)

## 调研基线

| 项 | 内容 |
|---|---|
| 来源 | [cloudflare/sandbox-sdk](https://github.com/cloudflare/sandbox-sdk) |
| 固定版本 | [`20f9da4`](https://github.com/cloudflare/sandbox-sdk/tree/20f9da4a9eb297db64375c0a98753626e91b52ac) |
| 许可证 | Apache-2.0 |
| 关注点 | Cloudflare Containers 上的 sandbox、容器内服务、命令/文件/PTY/tunnel、生命周期与 retry |

## 来源事实

项目包含 Worker 侧 SDK 和容器内服务。`Sandbox` façade 绑定 Cloudflare Container，暴露命令、文件、进程、session 和 tunnel 操作；`session` 是保持 cwd、环境和 shell 状态的容器内 bash 作用域。后台 process 有独立输出、状态和停止 handle。平台 tunnel 提供容器端口访问。

## 对 Dune 底层可复用

- SDK 与执行端服务分层；Dune 的 Gateway 路由调用，daemon 承载标准和扩展能力的实现/插件。
- 区分短操作、后台进程与 PTY 的状态和 handle；连接重试不能隐式创建替代进程。
- 副作用结果需区分未接受、已接受与无法确认；transport 错误不能作为自动重放命令或输入的依据。
- 当前执行实例、连接绑定与进程实例分开 fencing，旧连接不得控制替代实例；细节以传输协议为准。
- 能力发现使用版本、状态、features、limits 和 constraints，cwd/env 受授权的执行上下文约束。
- 高频数据可经统一 Gateway 路径转发；队列和缓冲有界，Gateway/daemon 均不保存内容或元数据数据库。

## 上层参考

Cloudflare Containers 资源创建、检查、停止、销毁、位置和保留策略可由上层 Fabric/Provider 封装。Provider 的保存/恢复能力也由上层 SDK 接入。Runner、云凭据管理、资源预设、公共 tunnel 和自定义域名不进入 Dune 核心。

环境初始化可作为 `kind=environment` 的完整 Profile payload 交给 daemon 解析执行；保存该配置、确定何时执行及失败后是否回收资源由上层决定。

## 不纳入核心

不要求 Durable Object、Cloudflare 路由目录或平台自动恢复机制；不复制持久 shell session 为业务顶层对象。资源 ID/URL 不能替代鉴权，SDK façade 不能扩大 daemon 的 OS 权限。Dune 不维护持久 admission ledger；内存证据丢失后允许结果未知，由调用方处理。

## 证据链接

- Sandbox SDK façade 与能力入口：[sandbox.ts](https://github.com/cloudflare/sandbox-sdk/blob/20f9da4a9eb297db64375c0a98753626e91b52ac/packages/sandbox/src/sandbox.ts)。
- 容器内持久 shell session、前后台命令与 PTY：[session.ts](https://github.com/cloudflare/sandbox-sdk/blob/20f9da4a9eb297db64375c0a98753626e91b52ac/packages/sandbox-container/src/session.ts)。
- 容器 control API 对 Exec、Process、Files、Tunnel 的聚合：[control-plane/api.ts](https://github.com/cloudflare/sandbox-sdk/blob/20f9da4a9eb297db64375c0a98753626e91b52ac/packages/sandbox-container/src/control-plane/api.ts)。
- PTY WebSocket 路由：[server.ts](https://github.com/cloudflare/sandbox-sdk/blob/20f9da4a9eb297db64375c0a98753626e91b52ac/packages/sandbox-container/src/server.ts)。
- 生命周期 fencing：[sandbox-lifetime.ts](https://github.com/cloudflare/sandbox-sdk/blob/20f9da4a9eb297db64375c0a98753626e91b52ac/packages/sandbox/src/sandbox-lifetime.ts)。
- 响应重试策略：[response-retry.ts](https://github.com/cloudflare/sandbox-sdk/blob/20f9da4a9eb297db64375c0a98753626e91b52ac/packages/sandbox/src/response-retry.ts)。
- tunnel handler：[tunnels-handler.ts](https://github.com/cloudflare/sandbox-sdk/blob/20f9da4a9eb297db64375c0a98753626e91b52ac/packages/sandbox/src/tunnels/tunnels-handler.ts)。
