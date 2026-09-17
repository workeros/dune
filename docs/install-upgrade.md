# 接入、修复和升级

个人与宿主签发的接入命令都使用 `/api/v1/install.sh`。命令生成时已分配稳定 Runner ID，接入过程中应保留这个标识，用于核对结果。

```sh
sh dune-install.sh enroll https://dune.example.com TOKEN RUNNER_ID [CA_CERT]
sh dune-install.sh repair https://dune.example.com [CA_CERT]
sh dune-install.sh upgrade https://dune.example.com [CA_CERT]
```

接入只接受不存在的配置；有效、损坏配置和悬空链接都直接报错，不消费令牌。下载/校验失败且尚未提交 enrollment，可以在令牌有效期内重试。提交后响应丢失或配置保存失败会明确报告结果未知和 Runner ID，不重放请求也不取回凭据：先在网页核对这个 Runner；未绑定可取消待接入，已绑定但无有效凭据则解绑后生成新命令。取消/解绑结果未知时先读取状态。

配置完整而程序或服务安装失败时使用 repair。upgrade 要求已有安装，始终沿用配置与绑定；已撤销的凭据不会被隐式重绑，fabricd 日志会报告 Gateway 拒绝凭据。新版本地初始化成功不等于 Gateway 在线，网络状态由工作台另行展示。

安装器下载完整四平台归档与 SHA256 清单，验证后才安装。程序放入 `$DUNE_INSTALL_DIR/releases/<id>`，默认 `~/.local/share/dune`；稳定入口为 `current/dune`，可将 `current` 加入 PATH。配置默认 `~/.config/dune/config.yaml`，可用 `DUNE_CONFIG` 指定；服务名默认 dune，可用 `DUNE_SERVICE_NAME` 指定。更改文件名不意味着支持多账号共享同一 SessionDir/服务。

切换顺序为准备新目录、停止旧 fabricd、更新用户服务路径、启动新版、检查私有启动回执、更新 current、删除安装器拥有的旧 release 目录。回执必须匹配本次 nonce、程序目录、PID/进程启动标识，且进程仍存活；仅 systemd/launchd 接受启动不算成功。超时或失败保留旧目录供检查，但不提供自动回退。服务 PATH 清除旧 release 和下载临时目录。

配置、证书、SessionDir、Runtime 小记录、tmux socket 和工作目录必须在 releases 外部；安装器检查路径别名后拒绝会被清理的持久位置，releases 本身也不能是符号链接。配置中的 certificate、key 和 session_dir 使用绝对路径，避免服务工作目录改变路径含义。tmux 与已运行的超时 helper 不随旧文件删除而终止；timeout 仍会在 fabricd 离线时执行 TERM，最多 2 秒后 KILL，并保留历史。显式 runtime stop 才销毁 session/history。

服务依赖系统 systemd 用户实例（Linux）或 launchd 用户域（macOS），启动回执检查使用系统 `/bin/ps`。真实平台服务管理器和休眠行为需要在相应目标机验收；本地替身测试仅证明服务命令、启动回执、程序清理与真实 fabricd/PTY 的组合行为。

登录入口默认只信任直接连接来源。反向代理部署可设置 `dune web --trusted-proxies CIDR[,CIDR...]`，宿主使用 `host.Options.TrustedProxies`。只从可信代理接受 X-Forwarded-For，从右向左取首个不可信节点；代理必须追加或覆盖正确来源。登录同时按来源和规范化账号限流，避免所有代理后用户共享单个额度。
