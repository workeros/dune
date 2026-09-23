# Runner 安装与在线升级

个人和嵌入宿主签发的接入命令使用 `/api/v1/install.sh`。命令携带预先分配的 Runner ID，
提交 enrollment 后结果未知时应查询该 Runner，不能重放 enrollment 或另发令牌掩盖未知结果。

```sh
sh dune-install.sh enroll https://dune.example.com TOKEN RUNNER_ID [CA_CERT]
# 配置已由本次 enrollment 成功保存、尚未创建安装身份时：
sh dune-install.sh install https://dune.example.com [CA_CERT]
```

本项目采用新的原型安装基线。安装器只创建新身份，不转换旧目录、配置或数据库。
已有 `installation.json` 或无法归属的 `current` 会被拒绝；现存会话与配置不能自动清空。
接入只接受不存在的配置，损坏文件和悬空链接也视为已存在。

标准安装使用私有根目录，默认为 `~/.local/share/dune`，可用 `DUNE_INSTALL_DIR` 设置：

- `installation.json`：安装身份、服务信息、完整组件观察和单调安装修订。
- `current`：指向 `releases/<id>` 的相对链接，连接器服务执行 `current/dune`。
- `recovery/`：独立恢复服务保留的发行；连接器切换不会替换它。
- `upgrades/`：任务、提交身份、冻结发行、确认与恢复证据。
- `diagnostics/`：有界连接器诊断，不记录凭据或完整配置。

配置、证书、密钥、SessionDir、宿主保留程序、tmux socket 和工作目录必须在发行目录外。
默认配置为 `~/.config/dune/config.yaml`，可用 `DUNE_CONFIG` 设置。配置固定原宿主升级控制端点。
本地初始安装通过实际文件生成完整发行回执，不虚构归档 URL；在线目标必须有宿主获准的不可变归档及摘要。

Linux 使用 systemd 用户实例，macOS 使用 launchd 用户域。attached 和新 managed bootstrap
均由标准安装器创建两个独立的用户服务：`dune` 与 `dune-upgrade`（名称可配置）。
目标环境必须提供该用户的服务管理器，预检不通过就拒绝安装；没有临时 `nohup` 降级分支。
连接器重启不停止 worker、tmux 或独立 ACP 宿主。managed bootstrap 根目录独立保存其配置和安装。

服务定义丢失或启动中断时，修复注册服务不会切换发行、重新 enrollment 或解除启动封闭：

```sh
dune repair-services --root /absolute/installation
# 手动唤醒已有活动操作，绝不接纳新升级：
dune upgrade-worker --root /absolute/installation --once
```

升级使用持久任务和完整发行修订。目标实际程序、完整文件、原 binding 的 Gateway 接纳、
正常路由只读往返均匹配本次尝试，执行方才持久提交成功并解除启动封闭。
切换后失败会进入原发行回滚；回滚也必须核验当前共享状态、原文件和平台可达性。
不能通过恢复旧 SessionDir 快照丢弃升级期间的新记录。

公开接口见 [升级 API](runner-upgrade-api.md)，当前实现与剩余验收见 [在线升级交付记录](runner-online-upgrade.md)。
已通过 macOS arm64 的独立 LaunchAgent 生命周期测试；systemd 原生运行、其他架构和完整 U01–U19
尚不能由交叉编译或模拟服务测试替代。

反向代理部署可设置 `dune web --trusted-proxies CIDR[,CIDR...]` 或 `host.Options.TrustedProxies`。
仅从可信代理接受 X-Forwarded-For，从右向左取首个不可信节点；代理必须正确追加或覆盖来源。
