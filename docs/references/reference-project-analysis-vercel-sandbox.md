# Dune 参考项目分析：Vercel Sandbox

> 关联规范：[设计规范](../spec.md) · [传输协议](../runner-tunnel-protocol.md) · [调研索引](README.md)

## 调研基线

| 项 | 内容 |
|---|---|
| 来源 | [vercel/sandbox](https://github.com/vercel/sandbox) |
| 固定版本 | [`8e471d4`](https://github.com/vercel/sandbox/tree/8e471d48548c1d8f3287bc66f650f2e87d041445) |
| 许可证 | Apache-2.0 |
| 关注点 | v2 持久 Sandbox、运行 Session、Command handle、filesystem、snapshot/resume/fork、network policy |

## 来源事实

固定版本的 Vercel v2 将持久逻辑 `Sandbox` 与每次运行的 VM `Session` 分开。Sandbox 可保存配置、从 snapshot 恢复或 fork；Session 有独立 ID、资源、状态和运行时间。`Command` 提供输出、等待和停止 handle，`FileSystem` 是文件操作 façade，`Snapshot` 是跨 Session 的持久产物，route/port 提供 provider 端口路由。

这些来源对象必须保留其原有语义，不因为 Dune 的职责调整而改写。

## 对 Dune 底层可复用

- 命令 handle 与环境生命周期分开，支持状态、等待、停止和流式输出。
- 连接恢复与副作用重试分开；不能因 transport 错误自动重放 runCommand、PTY input 或 ACP message。
- 当前运行实例和连接需要独立有效性检查；底层实例变化后，旧进程 handle/连接凭证不自动适用于新实例。
- Files 接口与 Exec/PTY 使用一致的授权和 OS 执行身份；仅指定 cwd 不能建立完整进程隔离。
- Profile 是 daemon 可解析执行的完整 `kind=environment|agent` payload。上层可为首次初始化和恢复准备不同 payload，不要求 daemon 维护配置库。
- 运行中进程 handle 按进程实例有效，不因短时缓存过期而任意失效；副作用 admission/去重证据仅在内存有效范围内可查询，丢失后允许 unknown。

## 上层参考

持久 Sandbox 与运行 Session 的区分可帮助上层设计稳定 Runner 身份：可恢复停止与最终销毁分开，恢复保留原 Runner，克隆产生新 Runner。Provider 的 save/load/checkpoint、快照来源/期限、磁盘保留与资源状态都由上层 SDK 封装。

Fabric、镜像、网络策略、源码准备、云凭据、分配幂等/single-flight、配置冲突和公共端口发布属于上层。上层可记录 provider command ID 或调用结果，Dune 不承担该持久事实源。

## 不纳入核心

不创建持久 Sandbox、Runner、Session 或 Snapshot 业务资源；不复制 provider workflow 序列化和跨进程 SDK 对象恢复。不把 URL/hostname 当授权，不增加持久 journal 或 blanket retry。来源支持 resume/fork 不意味着 Dune 自身分配资源或恢复环境，也不应被错误地解释为上层必须更换 Runner 身份。

## 证据链接

- 持久 Sandbox 的 create 参数、image、source、network、persistence 和 snapshot 配置：[sandbox.ts](https://github.com/vercel/sandbox/blob/8e471d48548c1d8f3287bc66f650f2e87d041445/packages/vercel-sandbox/src/sandbox.ts)。
- Session 明确定义为 Sandbox 内的一次 running VM：[session.ts](https://github.com/vercel/sandbox/blob/8e471d48548c1d8f3287bc66f650f2e87d041445/packages/vercel-sandbox/src/session.ts)。
- Command handle、wait/output 与序列化字段：[command.ts](https://github.com/vercel/sandbox/blob/8e471d48548c1d8f3287bc66f650f2e87d041445/packages/vercel-sandbox/src/command.ts)。
- Filesystem façade：[filesystem.ts](https://github.com/vercel/sandbox/blob/8e471d48548c1d8f3287bc66f650f2e87d041445/packages/vercel-sandbox/src/filesystem.ts)。
- 网络策略结构：[network-policy.ts](https://github.com/vercel/sandbox/blob/8e471d48548c1d8f3287bc66f650f2e87d041445/packages/vercel-sandbox/src/network-policy.ts)。
- SDK execution context 抽象：[execution-context.ts](https://github.com/vercel/sandbox/blob/8e471d48548c1d8f3287bc66f650f2e87d041445/packages/vercel-sandbox/src/execution-context.ts)。
