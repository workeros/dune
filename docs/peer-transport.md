# PostgreSQL 多 Gateway 与 peer 传输

`pkg/transport/peer` 使用双向 TLS 1.3、WebSocket、Yamux 和长度前缀 protobuf，
让用户入口 Gateway 将请求转发给当前持有 connector 的 Gateway。SQLite 不启用
该模式。

## 目录模型

共享状态只有 `dune_routes`。每台在线机器最多一个 owner 租约，记录 owner 的
boot ID、直接 peer 地址、固定执行 binding、published 标记、到期时间和单调
epoch。

1. connector 与某个 Gateway 完成 daemon 握手；
2. Gateway 在数据库中取得下一 epoch 的短租约；
3. connector 确认输入 grant，Gateway 再发布 route；
4. owner 周期续租，所有输入同时受本地单调 deadline 和 route epoch 约束；
5. owner 失联或失租后，新连接只能在旧租约到期后取得更高 epoch。

旧 owner 的 Publish、Renew、Release 不能修改新 epoch。数据库响应迟到也不能
延长 Gateway 已获得的本地权限。

自动接管不迁移已有 TCP/Yamux/WebSocket 流。浏览器会短暂断开，fabricd 自动
重连，用户显式重新连接并 attach 仍存活的 tmux Runtime。进行中的写入或 Agent
请求若结果未知，不会由入口或 connector 自动重放。

## peer 身份与逐请求授权

每个实例配置独立、直接可达的 HTTPS peer 地址、包含 serverAuth/clientAuth 的
叶证书和集群 CA。peer listener 与公开浏览器 listener 分开，不经七层代理终止
TLS；公开 Nginx 不暴露 peer Handler。

mTLS 只证明调用方是集群成员，不授予用户权限。入口在进程内生成短期 peer
上下文，绑定 source/owner boot、target、请求摘要、企业或本地 Session、Runner
binding 与到期时间。owner：

- 核对 TLS 来源、boot ID、target 和当前 route epoch；
- 对 nonce 做进程内单次消费；
- 再次调用配置的 `identity.Service` 验证当前 Session；
- 再查当前 Runner enabled/binding；
- 再执行访问策略。

因此 Dune 不需要 `access_tickets` 或 `peer_access` 表。一个 peer 连接只允许单跳，
不会转发到第三个 Gateway，也不会在失败后自动换 owner 重试。

## 宿主装配

Go 宿主通过 `host.Options.Database` 选择 PostgreSQL，并设置：

```go
Cluster: &host.ClusterOptions{Peer: peer.Config{
    Address:     "https://10.0.0.12:9443/private/peer",
    Certificate: certificate,
    Roots:       clusterRoots,
}}
```

公开 listener 调用 `App.Serve`；另建 TCP listener 交给
`App.ServePeer(listener)`。官方命令对应 `--database-config` 与
`--cluster-config`。cluster YAML 只包含 peer listen/address/certificate/key/ca，
没有共享配置指纹、实例准入或恢复代次。

部署系统负责保证副本使用兼容的身份、策略和协议配置。Dune 不在数据库中维护
实例列表，也不做配置一致性投票。

## 撤销与故障

本机撤销会立即调用 Gateway disconnect。远端 owner 最迟在周期有效性检查或
route 租约失效时关闭连接；不建立逐实例关闭待办或 ACK。若 PostgreSQL 无法
续租，owner 停止接收输入并断开 connector，由 connector 重试健康入口。

相关测试：`internal/metadata` 覆盖 route 竞争、epoch、数据库时钟与未知提交；
`pkg/gateway` 覆盖握手、期限和 peer 单跳；`pkg/fabricd` 覆盖接管后重新 attach
原 PTY；`tests` 的 PostgreSQL 用例覆盖正式多进程链路（需 `DUNE_TEST_POSTGRES`）。
