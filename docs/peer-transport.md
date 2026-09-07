# Peer 传输接入

`pkg/transport/peer` 提供目录模式 Gateway 的双向 TLS 1.3 / WebSocket 接入，继续使用已有 Yamux 和长度前缀 protobuf。`host.Options.Cluster` 已提供同一 PostgreSQL 后端的宿主装配和独立 peer 监听入口；配置一致性准入、readiness/排空和多进程故障验收仍在实施，不把当前接口等同于已部署的 HA 服务。

## 身份与边界

每实例配置完整、直接可达的 HTTPS peer 地址、私有叶证书/密钥和独立集群 CA 信任集合。叶证书必须匹配该地址的主机名或 IP，同时允许 serverAuth 与 clientAuth；启动验证当前期限、证书链、用途和密钥匹配。拒绝 HTTP、通配监听地址、无效主机/端口、用户信息、查询参数、片段和含混路径。地址须指向实例自身，不使用负载均衡地址。

CA 认证的是可信集群成员。成员在受 TLS 保护的请求中声明入口启动身份、目标启动身份和机器目标；接收方核对本机实际启动身份，核心的 peer hello 再核对双方启动身份、执行绑定、恢复代次与 epoch。证书不直接代表用户，机器和用户凭据也不能充当 peer 身份；用户仍须通过[逐请求访问上下文](implementation.md#网络和协议)取得权限。集群 CA 不应为普通用户或机器签发 peer 证书。

peer 必须使用独立监听入口。模块要求实际 TLS 连接，忽略 `X-Forwarded-Proto`、客户端证书转发头等代理声明；允许四层 TLS 透传，不能在普通七层代理解密后以明文转发到 peer Handler。`ServerTLSConfig()` 要求客户端证书，只适用于 peer 监听器，不用于公开浏览器监听器。Handler 还独立按原集群 CA 验证客户端链，应用改变监听器的信任集合不会扩大 Handler 的授权范围。

## 工作台宿主

通过 `host.Options.Database` 选择 PostgreSQL，设置 `Cluster: &host.ClusterOptions{RecoveryGeneration: recovery, Peer: peer.Config{Address: address, Certificate: certificate, Roots: roots}}`。`host.Open` 从同一存储池创建目录、用户授权和 peer 上下文；每次启动获得新的 Gateway boot 身份。恢复代次是所有副本共同配置的 32 位小写十六进制值，已有值不在启动时替换，离线恢复使用[代次工具](metadata-migration.md)。证书、私钥和 CA 的读取由宿主管理。

公开 HTTP 服务仍调用 `App.Serve` 或挂载 `App`；另创建普通 TCP listener，交给 `App.ServePeer(listener)`，由 App 加上自己的双向 TLS 策略并接管监听器。完整广告地址必须直接到达该实例的这个端口，peer 不挂载到公开 HTTP 路由。父 context 取消或 `App.Close` 一起关闭这些监听器、升级连接和用户请求；监听器启动失败应由宿主取消整个应用。关闭后仍保留远端 tmux。

机器可以连到不同于用户入口的副本，工作台和人类 CLI 请求按原目录 owner 转发一次。工作台的机器列表、Runner 列表/详情及 CLI 机器列表仅针对用户已获准查看的绑定批量读取在线事实，一页最多一百个 ID、查询最多一秒；未确认、过期和旧恢复代次路由不作为在线连接，存储故障返回服务错误。在线是展示信息，执行仍独立校验固定绑定和归属。

SQLite 不能启用 Cluster；已经初始化集群目录的 PostgreSQL 不能缺少 Cluster 配置启动单机宿主。这个启动检查**尚未构成持续的配置一致性租约**，也不能阻止并发启动的混合模式；当前切换必须先停止整个部署，并确保所有副本的身份、授权和公开入口配置一致。跨副本配置指纹、运行期准入及 readiness/排空将在后续功能点完成。

## 官方 CLI 配置

`dune web --database-config /private/database.yaml --cluster-config /private/cluster.yaml --url https://dune.example.com/dune/` 启用集群装配。继续通过全局 `--config` 指定本机的公开监听地址与 WS(S) 参数；该参数必须放在 `web` 之前。集群配置必须和 PostgreSQL 配置一起使用。

```yaml
recovery_generation: "0123456789abcdef0123456789abcdef"
peer:
  listen: "10.0.0.11:9443"
  address: "https://dune-a.internal:9443/private/peer"
  certificate: "peer-cert.pem"
  key: "peer-key.pem"
  ca: "cluster-ca.pem"
```

示例代次须在首次部署时替换为一次生成、所有副本共同保存的值（例如 `openssl rand -hex 16`）；已有数据库使用当前代次，备份恢复按离线工具旋转。每实例替换 listen、address 与自己的叶证书，公开用户入口可统一经过负载均衡，peer 地址直接定位实例。

配置和私钥文件属于当前用户，权限为 `0600`；证书与 CA 可只读共享。路径相对集群配置文件所在目录解析，也接受绝对路径。所有 PEM 文件须为不超过 64 KiB 的普通文件，不接受符号链接；目录中的秘密不会写入 SQL 或启动输出。叶证书仍须满足前述双用途、主机匹配和集群 CA 约束。

CLI 在启动服务前取得所有公开、额外 Web 和 peer 监听器；任一绑定失败则关闭本次打开的监听器与 App。运行期任一监听服务失败或收到退出信号也会关闭整个 App。当前这是直接关闭生命周期，后续的有界排空尚未实现。

## 底层 Go 应用装配

应用负责从私有配置读取证书和 CA，构造 `peer.Config{Address, Certificate, Roots}` 后调用 `peer.New`。私钥不进入 SQL、日志、启动信息或浏览器配置；模块复制证书字节和信任集合，应用不得修改正在使用的私钥。

应用用同一元数据后端提供的目录构造 `gateway.NewWithPeers(directory, transport.Address(), recovery, transport.Dial)`。内置授权模块按 Gateway 的新 `BootID()` 装配用户上下文；`transport.Handler(ctx, core, authorize)` 只接受与目录声明完全一致的地址。回调取得有界 context、已通过 TLS 认证的入口启动身份及目标，必须返回完全匹配的 `RolePeer` 绑定和访问处理器，不能替换为 SDK 或机器角色。

应用将 Handler 挂载在完整广告路径上，并使用 `tls.NewListener(listener, transport.ServerTLSConfig())` 启动独立 HTTP 服务。HTTP 服务应设置五秒 `ReadHeaderTimeout` 与有限的 `MaxHeaderBytes`；协议模块限制为 256 条并发 peer 连接。监听器由应用管理，取消应用 context 关闭已升级连接；关闭 HTTP server 本身不能代替取消已劫持的 WebSocket 或关闭 Gateway。

拨号不使用环境代理、不跟随重定向、不重试，只向目录中的原实例发送 peer 协商头，不带 Cookie、Origin 或 bearer。HTTP 路径、Host、协议版本、目标启动身份和每个身份头均严格检查，重复身份头拒绝。传输协商版本为 `1`，核心协议仍是 `dune-mvp/2`；不支持缺少 peer 角色或访问上下文的旧实例降级接入。证书到期会关闭空闲连接，重新连接仍需当前有效证书和用户上下文，不恢复原流或重放业务请求。

## 轮换与验证

首版通过协调重启变更配置，不提供热更新控制面。CA 轮换先将新 CA 加入所有实例的信任集合，再换用新 CA 签发的实例证书，最后移除旧 CA；每步协调重启并确认对端接入。旧实例及其连接退出后再移除旧信任，保留旧 CA 期间旧证书仍被接受。证书和 CA 撤销以部署配置与期限为准，不承诺自动查询 CRL/OCSP。

定向检查为 `go test -race ./pkg/transport/peer -count=1 -timeout=60s`，覆盖真实 TLS 握手、独立 Handler 信任检查、错误 CA/主机/启动身份、角色边界、明文及转发头、重复头、重定向、回调取消、证书到期和跨 CA 轮换。测试使用内存中的临时 CA，不写入生产信任。

启用专用 PostgreSQL 后，`go test -race ./pkg/fabricd -run TestPostgresOwnedReverseConnections -count=1 -timeout=90s` 使用三个独立 peer HTTPS 监听器、正式用户上下文和真实 tmux，验证目录定向连接、原 owner 更换后的显式重接、独立授权拒绝、空闲撤销和恢复代次隔离。SDK/反向连接仍是本地协议夹具，不将此测试称为完整 host/CLI 部署或三个独立进程的故障矩阵。

`go test -race ./tests -run 'TestPostgresClusterWorkbench|TestPostgresHostClusterConfiguration' -count=1 -timeout=180s` 使用两个正式 `host.App` 和独立 mTLS peer 监听器，机器由真实 fabricd 进程连接 B，工作台与人类 CLI 固定进入 A。覆盖共享在线事实、Runner 快照、真实 PTY、企业共享访问/拒绝及空闲撤销。它仍是同机双宿主测试，不证明三进程网络分区、配置漂移、真实企业权限 SDK 或 Linux 集群部署。

`go test -race ./tests -run TestPostgresClusterCLIProcesses -count=1 -timeout=180s` 启动三个独立官方 CLI 服务进程和真实 fabricd，A 签发安装材料、B 消费并持有机器连接，A/C 的 Web 和人类 CLI 经 peer 访问 B，另一入口退出用户后验证既有访问失效。测试证书写入专用临时目录，结束后连同进程、schema 一起清理；该回归是三进程正常运行链路，不是网络分区、时钟扰动或 Linux 集群故障验收。
