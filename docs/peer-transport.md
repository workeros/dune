# Peer 传输接入

`pkg/transport/peer` 提供目录模式 Gateway 的双向 TLS 1.3 / WebSocket 接入，继续使用已有 Yamux 和长度前缀 protobuf。`host.Options.Cluster` 已提供同一 PostgreSQL 后端的宿主装配和独立 peer 监听入口；配置一致性准入、就绪检查与限时排空已接入；已增加三进程链路停滞回归，完整故障矩阵仍在验收，不把当前接口等同于已部署的 HA 服务。

## 身份与边界

每实例配置完整、直接可达的 HTTPS peer 地址、私有叶证书/密钥和独立集群 CA 信任集合。叶证书必须匹配该地址的主机名或 IP，同时允许 serverAuth 与 clientAuth；启动验证当前期限、证书链、用途和密钥匹配。拒绝 HTTP、通配监听地址、无效主机/端口、用户信息、查询参数、片段和含混路径。地址须指向实例自身，不使用负载均衡地址。

CA 认证的是可信集群成员。成员在受 TLS 保护的请求中声明入口启动身份、目标启动身份和机器目标；接收方核对本机实际启动身份，核心的 peer hello 再核对双方启动身份、执行绑定、恢复代次与 epoch。证书不直接代表用户，机器和用户凭据也不能充当 peer 身份；用户仍须通过[逐请求访问上下文](implementation.md#网络和协议)取得权限。集群 CA 不应为普通用户或机器签发 peer 证书。

peer 必须使用独立监听入口。模块要求实际 TLS 连接，忽略 `X-Forwarded-Proto`、客户端证书转发头等代理声明；允许四层 TLS 透传，不能在普通七层代理解密后以明文转发到 peer Handler。`ServerTLSConfig()` 要求客户端证书，只适用于 peer 监听器，不用于公开浏览器监听器。Handler 还独立按原集群 CA 验证客户端链，应用改变监听器的信任集合不会扩大 Handler 的授权范围。

## 工作台宿主

通过 `host.Options.Database` 选择 PostgreSQL，设置 `Cluster: &host.ClusterOptions{RecoveryGeneration: recovery, Peer: peer.Config{Address: address, Certificate: certificate, Roots: roots}}`。`host.Open` 从同一存储池创建目录、用户授权和 peer 上下文；每次启动获得新的 Gateway boot 身份。恢复代次是所有副本共同配置的 32 位小写十六进制值，已有值不在启动时替换，离线恢复使用[代次工具](metadata-operations.md)。证书、私钥和 CA 的读取由宿主管理。

公开 HTTP 服务仍调用 `App.Serve` 或挂载 `App`；另创建普通 TCP listener，交给 `App.ServePeer(listener)`，由 App 加上自己的双向 TLS 策略并接管监听器。完整广告地址必须直接到达该实例的这个端口，peer 不挂载到公开 HTTP 路由。父 context 取消或 `App.Close` 一起关闭这些监听器、升级连接和用户请求；监听器启动失败应由宿主取消整个应用。关闭后仍保留远端 tmux。

机器可以连到不同于用户入口的副本，工作台和人类 CLI 请求按原目录 owner 转发一次。工作台的机器列表、Runner 列表/详情及 CLI 机器列表仅针对用户已获准查看的绑定批量读取在线事实，一页最多一百个 ID、查询最多一秒；未确认、过期和旧恢复代次路由不作为在线连接，存储故障返回服务错误。在线是展示信息，执行仍独立校验固定绑定和归属。

SQLite 不能启用 Cluster；已经初始化集群目录的 PostgreSQL 不能缺少 Cluster 配置启动单机宿主。所有 PostgreSQL 宿主（包括单机模式）现在共用配置准入租约，同时启动的单机/集群模式或不兼容配置不能同时注册。

## 配置准入与变更

同一 PostgreSQL schema 保存实例启动身份、非敏感配置摘要、恢复代次与十五秒期限，五秒续约。注册串行比较尚未过期的实例；同一配置可增加副本，冲突配置拒绝。已初始化集群的数据库还要求匹配恢复代次。恢复代次读取与离线旋转使用数据库行锁，不能在旧代次检查完成后越过同时发生的旋转继续提交。

摘要包含协议版本、挂载路径、身份源 namespace、会话期限、注册开关、默认/自定义访问模式、Managed 公开目录与 worker 行为（包括历史保留期），以及宿主声明的 `ConfigurationVersion`。每实例的公开 origin、peer 广告地址可以不同；它们不代表另一套业务配置。自定义身份、权限或 Managed 提供方模块在 PostgreSQL 模式必须提供 `host.Options.ConfigurationVersion`，CLI 对应 `--configuration-version oidc-policy-v1`。宿主应在 OIDC client ID、企业 SDK/策略、模板或其他不兼容的模块设置变化时更换这个非敏感版本；Dune 不解析企业实现内部的闭包和私有配置，也不把密码、client secret、证书私钥加入摘要。凭据的兼容轮换可保留版本。

宿主从数据库调用开始时刻折算保守的单调本地期限，续约不延长已经发给 fabricd 的原 grant。`gateway.AdmissionLease` 只表达额外的输入期限，core 不读取配置或 SQL；连接准入、应用 hook、发送和反向连接输入 grant 都受期限约束，定时器未调度时仍逐次检查。配置准入过期、续约失败或回执未知会关闭本次 App，不重放续约、不让旧 boot 复活，也不回滚此前已受理的工作。

`App.Close` 停止续约，但不提前删除原数据库预约：旧输入可能仍在缓冲区，冲突配置须等它的原期限自然结束。相同配置可立即重启；变更不兼容配置时，先停止旧实例并等待旧预约到期，再启动全部新实例。这里的十五秒是正常数据库时钟下的期限，不等同于排空窗口；修改数据库时钟或灾难恢复仍须按离线恢复步骤停站和隔离旧实例。

## 就绪与退出

`App.Readiness()` 提供本机状态；公开部署目录下的 `GET/HEAD health/ready` 和 `health/live` 返回同一份不缓存的 JSON，不包含身份、目标 ID、凭据或内部地址。`accepting` 表示允许新工作，`serving` 表示已有工作仍具备本机服务资格，`draining` 表示已启动排空。`requests` 统计 HTTP/管理员工作（不含健康探针和隧道连接），`gateway` 包含连接数、业务流数和本机已确认且输入资格有效的 `online_routes` 数。这些字段不扫描数据库目录，也不证明身份源、企业权限服务、所有机器或外部提供方健康。

例如公开 URL 为 `https://dune.example.com/tools/dune/` 时，就绪路径为 `/tools/dune/health/ready`。允许接新工作时返回 200；排空、关闭、准入失效或本机 peer 证书到期返回 503。`health/live` 在排空但尚可服务时仍返回 200，关闭或本机准入失效返回 503。挂载到外部服务器时，关闭后的探针仍可返回状态；App 自己拥有的监听器关闭后不可继续探测。

`App.Shutdown(ctx)` 不可逆地停止新的 HTTP/管理员工作、公开/peer 连接、已有连接上的新流和新的 Managed worker 迭代。已有流可以继续收发，并保留访问检查、输入期限、配置与连接归属续约；正常排空后才关闭连接并释放 owner。排空边界前已由 worker 领取的一轮可以完成，但完成后不会领取下一个阶段或排队操作。空闲 daemon/SDK/peer 控制连接不阻塞排空，持续终端和事件订阅属于未完成工作。已接受的 HTTP 请求可以完成当前处理；其中尚未建立的新 Gateway 工作仍受停止准入约束，不自动重试或转移到别处。

Shutdown 使用调用方的 deadline；没有 deadline 时默认五秒。期限或 context 取消后调用 Close，取消剩余 HTTP、流和 Managed 提供方调用，等待处理器、流回调与 worker 退出再释放存储，并返回 context 错误。提供方动作仍按原 action identity、unknown/timed_out 和接管规则持久恢复，不因排空而重发。可信回调和提供方适配器须遵守取消和及时返回约定，因此这是工作排空预算，不是对任意不合作代码的强制终止保证。由 App 拥有的 HTTP 服务器在正常结束时先完成响应刷新；挂载模式不关闭外部服务器，外部宿主自行完成其 HTTP 退出。不要在活跃 Dune 处理器、回调或 Managed 提供方调用内部等待 Shutdown/Close。

父 context 取消、准入故障或显式 Close 仍立即停止。自有宿主应先调用 Shutdown，再取消 Open 的生命周期 context。官方 `dune web` 的 SIGINT/SIGTERM 使用 `--drain-timeout`（默认 `5s`，零表示立即关闭，不允许负值）；启动期间仍响应取消。退出不销毁远端 tmux，未确认写入的结果仍可能未知。配置预约保留到原期限，不因排空完成提前释放。就绪状态只表示本机准入和排空状态，不检查 Managed 提供方健康。

协议宿主可独立调用 `Gateway.Drain()`；返回通道在所有已接受业务流及应用清理回调结束后关闭，随后调用 Close 释放空闲连接。core 只管理连接和流，不理解 HTTP、SQL 或业务生命周期。

上层撤销一个机器目标时调用 `Gateway.Disconnect(target)`。该调用在当前 Gateway 生命周期内永久拒绝此目标的新连接和新流，关闭直接 machine、SDK 入口及 peer 会话；返回通道仅在该目标所有已受理流及清理回调退出后关闭。Managed 销毁把当时仍获准运行的应用实例快照成共享 SQL 关闭待办，各实例只用自己的启动身份领取、完成本机 Disconnect 后确认；全部确认才提前结束等待，失联实例仍保留至固定 deadline 并形成 `timed_out`，不会被其他节点代签。

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

CLI 在启动服务前取得所有公开、额外 Web 和 peer 监听器；任一绑定失败则关闭本次打开的监听器与 App。运行期任一监听服务失败会立即关闭整个 App；收到退出信号则按上述期限先排空，再关闭 App。

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

`go test -race ./tests -run TestPostgresClusterTwoUserIsolationAndCapabilities -count=1 -timeout=120s` 在三个正式节点上把两条真实 fabricd 隧道分别固定到 B 和 C，两名用户从 A 进入。默认 owner 策略下，每人只发现并取得自己的目标；一次性票据不能在握手中换成另一目标，失败后也不能重放。随后两条 mTLS peer 路径分别执行 File、Git、PTY 和原始 ACP。该用例验证 Dune 的目标与用户上下文隔离，不提供同一 OS 用户内的 Shell 文件隔离。

配置准入运行 `go test -race ./internal/metadata ./pkg/gateway ./tests -run 'Admission|InstanceAdmission' -count=1 -timeout=180s`。它覆盖并发冲突、同配置跨池续约、锁等待后的过期复核、回执丢失、独立关闭计时器和有界输入。正式 CLI 的 PostgreSQL 暂停回归将宿主 SIGSTOP 超过十五秒、写入旧终端后恢复，检查旧宿主退出、文件未创建、新配置重启后原 PTY 可用。该回归没有操纵数据库时钟，也不替代三节点分区或 Linux 集群故障验收。

## 已验证的网络故障范围

三个正式 CLI 节点共用专用 PostgreSQL，机器固定进入 B，用户固定进入 A；C 用于新的用户入口或明确切换后的机器入口。本机 TCP 代理可以暂停双向字节转发并在恢复后释放滞留数据，TLS 与应用协议保持完整。

| 故障 | 实际核对的结果 |
| --- | --- |
| B 的 peer 链路停滞，已受理 Exec 的响应丢失 | 副作用已经发生，调用返回 `RESULT_UNKNOWN`，入口没有自行重拨；显式重接后副作用仍只有一次，原 owner 与 PTY 不变 |
| A 的 PostgreSQL 链路停滞 | A 在准入续约失败后退出，旧连接不能继续请求；用户显式进入 C 后访问同一 B 和原 PTY，A 恢复后可以作为新入口启动 |
| B 的 PostgreSQL 链路停滞 | B 退出；将新的机器连接路由到 C 后，目录发布新 owner/epoch，fabricd incarnation 保持、connection generation 推进，原 PTY 可重新使用；恢复 B 后目录仍指向 C |

目录另用 `TestPostgresDirectoryDatabaseClockDisturbance` 在隔离 PostgreSQL schema 中替换目录 SQL 读取的 `clock_timestamp()`，依次注入一分钟回拨、前跳及恢复。回拨时数据库保留原有效期，返回给 Gateway 的本地时长仍钳在十五秒且竞争 owner 被拒绝；前跳后旧续约失败，新 owner 使用更高 epoch，旧 Publish/Renew/Release 都不能改写它；恢复正常时，前跳产生的未来期限继续阻止第三个 owner，因此选择可用性降低而不是缩短已经承诺的期限。Gateway 把这些相对时长转换为 Go 单调时钟 deadline，目录响应延迟不能延长本地权限。

这些回归使用同机进程、TCP 转发代理和 schema 内的有效数据库时间，没有修改操作系统或 PostgreSQL 进程时钟，也没有注入内核丢包；不代表跨主机网络、NTP 实现、Linux 部署、原始/托管 ACP 的所有故障路径或 Managed 外部调用接管已经验收。原进程暂停与输入过期另有独立正式进程回归，不能把分开的证据视为所有组合均已验证。
