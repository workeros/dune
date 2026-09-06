# Dune 参考项目分析：OpenChamber

> 关联规范：[设计规范](../spec.md) · [传输协议](../runner-tunnel-protocol.md) · [调研索引](README.md)

## 调研基线

| 项 | 内容 |
|---|---|
| 来源 | [openchamber/openchamber](https://github.com/openchamber/openchamber) |
| 固定版本 | [`feec145`](https://github.com/openchamber/openchamber/tree/feec14545f5d9ebc99881df1f20e5af158adb5fc) |
| 许可证 | MIT |
| 关注点 | desktop/web/mobile 远程终端、direct/relay 路径、设备配对、E2EE、重连与屏幕同步 |

## 来源事实

OpenChamber 不是 sandbox provider。其 `runtime/device` 是运行终端服务的桌面或开发机实例；direct connection 直接连接设备服务，relay tunnel 由设备和客户端连接 relay 转发数据。

pairing code 和 remote client credential 用于建立设备信任。terminal runtime 创建 PTY、读写数据并维护屏幕状态；snapshot/live sync 用于先同步状态再接实时事件。这些设备、路径和产品状态属于来源模型。

## 对 Dune 底层可复用

- 执行目标身份与连接路径分开；路径变化不能自动赋予权限，也不能把旧连接当作当前有效绑定。
- Gateway/daemon 分别验证相应凭证的作用域、有效期和目标；网络地址、设备 ID 和配对码不替代日常调用授权。
- Dune 使用统一的 SDK、Gateway、daemon 调用路径；Gateway 可独立或合并部署，能力调用方无需理解部署拓扑。
- PTY 能力可声明 snapshot/live barrier 和有界短时缓冲；断线不能直接推断进程已停止。
- 对结果未知的 PTY/ACP 输入不自动重放；当前连接、进程实例和输入权按各自语义 fencing。
- 跨实现的编解码、帧边界和授权行为应通过 SDK/Gateway/daemon 互操作用例验证。

## 上层参考

设备列表、配对审批、长期身份登记、凭证生命周期、家庭成员和远程开发 UI 属于上层产品。上层把既有主机或云环境映射为可调用目标，再通过 Dune 使用其中的能力。

初始化以完整 `kind=environment` Profile payload 交给 daemon 执行；是否触发及如何保存配置由上层决定。Runner、Attached/Managed 模式、保留和恢复策略也由上层封装。

## 不纳入核心

不复制设备目录、配对数据库、中心 relay 产品或终端 UI；不让 transport 断开自动销毁环境。不把来源的 direct/relay 多路径和 E2EE 自动提升为 Dune 首版保证；具体传输选择以传输协议为准。终端同步材料只在能力声明范围内短时保留于内存，不建立历史库或跨 daemon 重启恢复承诺。

## 证据链接

- direct/relay 连接选择和移动端连接管理：[mobileConnections.ts](https://github.com/openchamber/openchamber/blob/feec14545f5d9ebc99881df1f20e5af158adb5fc/packages/ui/src/apps/mobileConnections.ts#L814-L985)。
- 连接 payload 与身份字段：[connectionPayload.ts](https://github.com/openchamber/openchamber/blob/feec14545f5d9ebc99881df1f20e5af158adb5fc/packages/ui/src/lib/connectionPayload.ts#L1-L39)。
- pairing code 的生成与消费：[pairing.js](https://github.com/openchamber/openchamber/blob/feec14545f5d9ebc99881df1f20e5af158adb5fc/packages/web/server/lib/client-auth/pairing.js#L239-L288)。
- remote client credential 校验：[remote-clients.js](https://github.com/openchamber/openchamber/blob/feec14545f5d9ebc99881df1f20e5af158adb5fc/packages/web/server/lib/client-auth/remote-clients.js#L158-L229)。
- relay 的信任与数据路径说明：[relay documentation](https://github.com/openchamber/openchamber/blob/feec14545f5d9ebc99881df1f20e5af158adb5fc/packages/web/server/lib/relay/DOCUMENTATION.md#L11-L63)。
- tunnel host 与 frame 路由：[tunnel-host.js](https://github.com/openchamber/openchamber/blob/feec14545f5d9ebc99881df1f20e5af158adb5fc/packages/web/server/lib/relay/tunnel-host.js#L234-L368)。
- terminal runtime 的 PTY 生命周期：[runtime.js](https://github.com/openchamber/openchamber/blob/feec14545f5d9ebc99881df1f20e5af158adb5fc/packages/web/server/lib/terminal/runtime.js#L123-L167)。
- 跨实现 codec 测试：[cross-compat.test.js](https://github.com/openchamber/openchamber/blob/feec14545f5d9ebc99881df1f20e5af158adb5fc/packages/web/server/lib/relay/cross-compat.test.js#L24-L134)。
