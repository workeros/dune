# Peer 传输接入

`pkg/transport/peer` 提供目录模式 Gateway 的双向 TLS 1.3 / WebSocket 接入，继续使用已有 Yamux 和长度前缀 protobuf。当前接口供 Go 应用装配；官方 host/CLI 的集群配置和监听入口尚待接入，不把此模块等同于已部署的 HA 服务。

## 身份与边界

每实例配置完整、直接可达的 HTTPS peer 地址、私有叶证书/密钥和独立集群 CA 信任集合。叶证书必须匹配该地址的主机名或 IP，同时允许 serverAuth 与 clientAuth；启动验证当前期限、证书链、用途和密钥匹配。拒绝 HTTP、通配监听地址、无效主机/端口、用户信息、查询参数、片段和含混路径。地址须指向实例自身，不使用负载均衡地址。

CA 认证的是可信集群成员。成员在受 TLS 保护的请求中声明入口启动身份、目标启动身份和机器目标；接收方核对本机实际启动身份，核心的 peer hello 再核对双方启动身份、执行绑定、恢复代次与 epoch。证书不直接代表用户，机器和用户凭据也不能充当 peer 身份；用户仍须通过[逐请求访问上下文](implementation.md#网络和协议)取得权限。集群 CA 不应为普通用户或机器签发 peer 证书。

peer 必须使用独立监听入口。模块要求实际 TLS 连接，忽略 `X-Forwarded-Proto`、客户端证书转发头等代理声明；允许四层 TLS 透传，不能在普通七层代理解密后以明文转发到 peer Handler。`ServerTLSConfig()` 要求客户端证书，只适用于 peer 监听器，不用于公开浏览器监听器。Handler 还独立按原集群 CA 验证客户端链，应用改变监听器的信任集合不会扩大 Handler 的授权范围。

## Go 应用装配

应用负责从私有配置读取证书和 CA，构造 `peer.Config{Address, Certificate, Roots}` 后调用 `peer.New`。私钥不进入 SQL、日志、启动信息或浏览器配置；模块复制证书字节和信任集合，应用不得修改正在使用的私钥。

应用用同一元数据后端提供的目录构造 `gateway.NewWithPeers(directory, transport.Address(), recovery, transport.Dial)`。内置授权模块按 Gateway 的新 `BootID()` 装配用户上下文；`transport.Handler(ctx, core, authorize)` 只接受与目录声明完全一致的地址。回调取得有界 context、已通过 TLS 认证的入口启动身份及目标，必须返回完全匹配的 `RolePeer` 绑定和访问处理器，不能替换为 SDK 或机器角色。

应用将 Handler 挂载在完整广告路径上，并使用 `tls.NewListener(listener, transport.ServerTLSConfig())` 启动独立 HTTP 服务。HTTP 服务应设置五秒 `ReadHeaderTimeout` 与有限的 `MaxHeaderBytes`；协议模块限制为 256 条并发 peer 连接。监听器由应用管理，取消应用 context 关闭已升级连接；关闭 HTTP server 本身不能代替取消已劫持的 WebSocket 或关闭 Gateway。

拨号不使用环境代理、不跟随重定向、不重试，只向目录中的原实例发送 peer 协商头，不带 Cookie、Origin 或 bearer。HTTP 路径、Host、协议版本、目标启动身份和每个身份头均严格检查，重复身份头拒绝。传输协商版本为 `1`，核心协议仍是 `dune-mvp/2`；不支持缺少 peer 角色或访问上下文的旧实例降级接入。证书到期会关闭空闲连接，重新连接仍需当前有效证书和用户上下文，不恢复原流或重放业务请求。

## 轮换与验证

首版通过协调重启变更配置，不提供热更新控制面。CA 轮换先将新 CA 加入所有实例的信任集合，再换用新 CA 签发的实例证书，最后移除旧 CA；每步协调重启并确认对端接入。旧实例及其连接退出后再移除旧信任，保留旧 CA 期间旧证书仍被接受。证书和 CA 撤销以部署配置与期限为准，不承诺自动查询 CRL/OCSP。

定向检查为 `go test -race ./pkg/transport/peer -count=1 -timeout=60s`，覆盖真实 TLS 握手、独立 Handler 信任检查、错误 CA/主机/启动身份、角色边界、明文及转发头、重复头、重定向、回调取消、证书到期和跨 CA 轮换。测试使用内存中的临时 CA，不写入生产信任。

启用专用 PostgreSQL 后，`go test -race ./pkg/fabricd -run TestPostgresOwnedReverseConnections -count=1 -timeout=90s` 使用三个独立 peer HTTPS 监听器、正式用户上下文和真实 tmux，验证目录定向连接、原 owner 更换后的显式重接、独立授权拒绝、空闲撤销和恢复代次隔离。SDK/反向连接仍是本地协议夹具，不将此测试称为完整 host/CLI 部署或三个独立进程的故障矩阵。
