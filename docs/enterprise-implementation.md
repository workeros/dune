# 企业扩展方案实施记录

实施依据：[企业扩展方案](enterprise-extensibility-plan.md)。按独立功能提交，阶段只有取得对应证据后才标记完成。当前正在实施 S0a；S0b–S3 尚未交付。

## S0a：连接与传输

### 已完成：默认拨号与协议分离

- `pkg/transport/ws` 提供默认 WS 拨号和 `net.Conn` 适配；`internal/wire` 只保留 Yamux 配置、消息编解码和握手，不再导入 HTTP、TLS 或 WebSocket 实现。
- `sdk.Connect(ctx, conn, target)` 接收已建立的连接；原 `sdk.Dial` 仍装配默认 WS 拨号。两者取得连接所有权，握手失败或取消会关闭连接；连接建立后由 `Client.Close` 管理生命周期。
- fabricd 的 `ServeConn` 承担一次反向连接的协议处理，拨号与退避仍由启动入口装配。连接取消不会销毁 tmux 会话，也不重放业务请求。
- 验证：`DUNE_REAL_AGENT= DUNE_REMOTE_CONFIG= make test` 全部本地回归通过，包括默认 WS 分片、PTY、ACP mock、文件、Git、并发和重连的进程测试；`make check-go` 通过；Gateway、daemon、SDK 的 race 检查通过。SDK 额外覆盖等待握手响应时取消及建立连接后的 context 所有权。

这一步尚未完成 Gateway core 与 HTTP/WS 接入分离，也未提供最终的连接处理器和宿主 SDK。真实身份源、Managed 提供方及集群验收仍待实施和实际环境验证。
