# 企业扩展方案实施记录

实施依据：[企业扩展方案](enterprise-extensibility-plan.md)。按独立功能提交，阶段只有取得对应证据后才标记完成。当前正在实施 S0a；S0b–S3 尚未交付。

## S0a：连接与传输

### 已完成：默认拨号与协议分离

- `pkg/transport/ws` 提供默认 WS 拨号和 `net.Conn` 适配；`internal/wire` 只保留 Yamux 配置、消息编解码和握手，不再导入 HTTP、TLS 或 WebSocket 实现。
- `sdk.Connect(ctx, conn, target)` 接收已建立的连接；原 `sdk.Dial` 仍装配默认 WS 拨号。两者取得连接所有权，握手失败或取消会关闭连接；连接建立后由 `Client.Close` 管理生命周期。
- fabricd 的 `ServeConn` 承担一次反向连接的协议处理，拨号与退避仍由启动入口装配。连接取消不会销毁 tmux 会话，也不重放业务请求。
- 验证：`DUNE_REAL_AGENT= DUNE_REMOTE_CONFIG= make test` 全部本地回归通过，包括默认 WS 分片、PTY、ACP mock、文件、Git、并发和重连的进程测试；`make check-go` 通过；Gateway、daemon、SDK 的 race 检查通过。SDK 额外覆盖等待握手响应时取消及建立连接后的 context 所有权。

### 已完成：Gateway 接入与请求处理链分离

- `pkg/gateway` 仅依赖字节连接、协议与执行类型。`ServeConn` 必须显式提供可信协议绑定及连接处理器；`pkg/access` 保存每连接的授权关联，`pkg/transport/tunnel` 完成认证与 WS Upgrade。`internal/gateway` 仅保留官方独立入口的配置和监听装配。
- 处理器通过 `Open` 处理首条请求，通过受控 `Stream.Forward` 选择转发，或通过 `Send` 在模块中处理。后续消息按方向有序交给 `Message` 检查；模块不能读取裸流。关闭通知恰好一次，写入有界且串行，钩子期限为 1 秒；受信任的宿主回调必须响应 context 取消。
- 现有 Web owner/会话检查通过独立连接处理器保留，空闲连接也每秒检查本地授权有效性。这只是现有个人访问检查的迁移，还不是 S1 的企业操作级 AccessChecker。
- `wire.Stream.Close` 明确取消双向 I/O；WS 适配在关闭时解除 fasthttp 默认 hijack 所有权造成的退出等待。修复由空闲流取消测试和 Gateway 进程退出回归发现。
- `samples/gateway` 仅通过公开包嵌入标准 HTTP 服务，无需产品数据库。标准 HTTP 与 fasthttp 默认入口都接入同一 core。
- 验证：全量本地 `make test`、相关 race 和 `make check-go` 通过；补充覆盖缺失处理器、首请求拒绝、后续输入拒绝、空闲撤销、钩子超时、模块内处理、关闭通知和 fasthttp 退出。`TestTmuxSurvivesFabricdAndGateway` 验证 Gateway 重启及 fabricd 强制/正常退出保留 PTY。检查 `pkg/gateway`、`internal/wire` 导入，不含 HTTP/WS、SQL、用户或 Runner 模块。

S0a 后续还需收敛 fabricd 的公开执行入口与应用装配边界。宿主 SDK、真实身份源、Managed 提供方及集群验收仍待实施和实际环境验证。
