# Dune 参考项目分析：Modal Sandboxes

> 关联规范：[设计规范](../spec.md) · [传输协议](../runner-tunnel-protocol.md) · [调研索引](README.md)

## 调研基线

| 项 | 内容 |
|---|---|
| 来源 | [modal-labs/modal-client](https://github.com/modal-labs/modal-client) |
| 固定版本 | [`0b5b35c`](https://github.com/modal-labs/modal-client/tree/0b5b35c62a381cbef032cd757ab76f710ad6b4a7) |
| 许可证 | Apache-2.0 |
| 关注点 | sandbox 创建、资源请求/上限、readiness、GPU、volume、snapshot、tunnel |

## 来源事实

Modal client 只展示公开客户端契约，不代表服务端完整实现。本文保留能够从固定客户端代码确认的资源和生命周期语义。

`Sandbox` 按 image、command、resource、network、volume 等配置创建；`ContainerProcess` 是执行命令后的进程 handle。`Volume` 和 `CloudBucketMount` 表达持久或外部存储，`Tunnel` 提供端口入口，`Probe` 表达 TCP/exec readiness 条件。文件系统快照与内存快照属于不同层级的能力。

## 对 Dune 底层可复用

- 长进程 handle 应明确查询、输出、等待和终止语义，与一次短操作的响应分开。
- 能力描述须包含实际 readiness 和限制；探测失败与进程已退出不是同一个事实。
- 协议只声明实现能够证明的能力，不以平台产品名、GPU 支持或一个 `persistent` 布尔值推导隔离和恢复保证。
- daemon 可以执行完整 Profile payload 中的准备步骤、启动参数和探测操作；Dune 不保存可复用配置资源。
- SDK 的类型封装及网络可达性不能替代 Gateway/daemon 的鉴权和执行身份边界。

## 上层参考

CPU/memory request-limit、GPU、区域、磁盘、网络策略、placement 和环境分配由上层 Fabric/Provider 适配。上层把 provider readiness 与 daemon/能力探测结果组合后，决定环境是否可供业务使用。

Volume 一致性、commit/flush、并发挂载、文件系统与内存快照、save/load 和恢复策略均由上层 SDK 如实封装。恢复与克隆的 Runner 身份规则属于上层；Dune 不以某个 serverless 平台的最长寿命规定业务生命周期。

## 不纳入核心

不提供 GPU 调度器、Volume/Snapshot 资源库、公共 tunnel 管理器或资源回收策略。不把实验 snapshot 能力写成基础协议保证；不因进程退出或 probe 失败自动销毁宿主资源。不建立 readiness、Profile 或进程输出的持久数据库；当前运行状态只在内存维护并受实例有效性约束。

## 证据链接

- Sandbox 创建参数、资源、网络、volume、port 和 snapshot 选项：[py/modal/sandbox.py](https://github.com/modal-labs/modal-client/blob/0b5b35c62a381cbef032cd757ab76f710ad6b4a7/py/modal/sandbox.py)。
- TCP/exec readiness probe 定义：[Probe in sandbox.py](https://github.com/modal-labs/modal-client/blob/0b5b35c62a381cbef032cd757ab76f710ad6b4a7/py/modal/sandbox.py#L274-L352)。
- CPU/memory request-limit、GPU 和 ports 参数：[Sandbox create](https://github.com/modal-labs/modal-client/blob/0b5b35c62a381cbef032cd757ab76f710ad6b4a7/py/modal/sandbox.py#L569-L657)。
- Volume 客户端语义：[py/modal/volume.py](https://github.com/modal-labs/modal-client/blob/0b5b35c62a381cbef032cd757ab76f710ad6b4a7/py/modal/volume.py)。
- Snapshot 客户端对象：[py/modal/snapshot.py](https://github.com/modal-labs/modal-client/blob/0b5b35c62a381cbef032cd757ab76f710ad6b4a7/py/modal/snapshot.py)。
- wire-level 资源和 sandbox 消息：[modal_proto/api.proto](https://github.com/modal-labs/modal-client/blob/0b5b35c62a381cbef032cd757ab76f710ad6b4a7/modal_proto/api.proto)。
