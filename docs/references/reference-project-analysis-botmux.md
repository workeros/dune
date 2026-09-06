# Dune 参考项目分析：BotMux

> 关联规范：[设计规范](../spec.md) · [传输协议](../runner-tunnel-protocol.md) · [调研索引](README.md)

## 调研基线

| 项 | 内容 |
|---|---|
| 来源 | [deepcoldy/botmux](https://github.com/deepcoldy/botmux) |
| 固定版本 | [`b0b35c4`](https://github.com/deepcoldy/botmux/tree/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07) |
| 许可证 | MIT |
| 关注点 | 多 Agent 终端进程、持久 backend、PTY 恢复、读写权限和输入所有者 |

## 来源事实

BotMux 是本地多 Agent 终端管理器，不是 sandbox provider。`bot` 表示 Agent 配置/实例，`backend` 承载终端进程，应用侧 `runtime` 记录运行状态；持久 backend 可使用 tmux。客户端断线与进程退出分开处理。

终端控制授权使用 generation 和租约；transient snapshot/terminal seed 用于连接时恢复屏幕。各 bot 可有独立环境配置。这些名称和行为属于来源项目，不定义 Dune 的公共对象。

## 对 Dune 底层可复用

- daemon 通过 Agent Profile payload 启动多个独立进程；命令、环境、cwd 和 adapter 按实例解析，避免意外共享可变配置。
- 区分观察、输入和终止权限；同一 PTY 的输入所有权可用内存租约和 generation fencing 表达。
- 客户端断线不直接判定进程退出；状态探测可返回存活、退出或无法确认。
- PTY 能力可声明当前屏幕同步、序列边界和短时缓冲；实现必须有界，不能暗含完整历史保证。
- 已无法确认是否写入的输入不得自动重放；进程 handle 对当前有效进程实例生效，输入租约另有自己的有效期，不能以缓存 TTL 任意终止运行中的进程控制权。

## 上层参考

多 Agent 工作台、个人或团队终端列表、共享成员、协作 UI 和终端控制申请属于上层产品。Runner 及其资源保留策略也由上层管理。上层可以选择 tmux 等后端，但不能据此要求 Dune 接管任意已运行 Agent。

Profile 的保存、命名和修订由上层负责；daemon 接收完整 `kind=agent` payload，解析并执行启动配置。

## 当前 PTY 采用决策（2026-09-06）

按用户要求，PTY 现已采用 tmux，参考版本更新为 `8d2986c71574bb799c8ffa89cc7631e6d8ddeec2` 的 `tmux-backend.ts`。复用 attach viewer 与持久 session 的生命周期分离，随包提供私有 tmux；不复制 Botmux 的应用业务、多后端框架或额外 VT 状态。详见 [tmux 后端](../tmux-backend.md)。下述不要求 tmux 的约束仅为早期调研结论，已被新决定取代。

## 早期未纳入核心

不把 bot、backend 或来源 runtime 复制成上层业务资源；不要求 tmux；不保存终端转录、屏幕快照或恢复 journal。daemon 重启后的恢复由明确的外部后端/Agent 命令能力处理，不能复用已经丢失的 Dune 内存状态。本地可信用户假设不能代替 Dune 入口鉴权和 OS 执行身份边界。

## 证据链接

- backend 的能力与状态接口：[backend types](https://github.com/deepcoldy/botmux/blob/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07/src/adapters/backend/types.ts#L3-L28) 与 [lifecycle surface](https://github.com/deepcoldy/botmux/blob/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07/src/adapters/backend/types.ts#L95-L137)。
- 持久 backend 封装：[persistent-backend.ts](https://github.com/deepcoldy/botmux/blob/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07/src/core/persistent-backend.ts#L1-L91)。
- tmux 恢复探测：[tmux-recovery.ts](https://github.com/deepcoldy/botmux/blob/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07/src/core/tmux-recovery.ts#L1-L18)。
- terminal control grant 的 generation 和租约：[terminal-control-grant.ts](https://github.com/deepcoldy/botmux/blob/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07/src/core/terminal-control-grant.ts#L1-L78) 与 [续租/撤销](https://github.com/deepcoldy/botmux/blob/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07/src/core/terminal-control-grant.ts#L124-L193)。
- 写入授权检查：[terminal-write-auth.ts](https://github.com/deepcoldy/botmux/blob/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07/src/core/terminal-write-auth.ts#L107-L141)。
- 屏幕快照与 Web terminal seed：[transient-snapshot.ts](https://github.com/deepcoldy/botmux/blob/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07/src/utils/transient-snapshot.ts#L30-L99)、[web-terminal-seed.ts](https://github.com/deepcoldy/botmux/blob/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07/src/utils/web-terminal-seed.ts#L1-L52)。
- per-bot 环境隔离：[per-bot-env.ts](https://github.com/deepcoldy/botmux/blob/b0b35c4b9c642f4740512e9ca1dec4cde9cdbd07/src/core/per-bot-env.ts#L30-L118)。
