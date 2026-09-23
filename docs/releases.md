# 本地构建与发行依赖

`make build` 准备当前平台的 `bin/tmux`、`bin/rg` 并构建 `bin/dune`。
独立准备搜索依赖使用 `make rg`，准备四个平台使用
`python3 scripts/fetch-rg.py --all`。下载需要访问 GitHub；已下载的归档保存在
`.tools/`，每次提取前都会核对固定 SHA-256。

## 固定版本与目标平台

tmux 固定为 3.7c，来源与校验值见
[`third_party/tmux/manifest.json`](../third_party/tmux/manifest.json)。
ripgrep 固定为 15.1.0，来源、四份归档的 SHA-256 和许可证信息见
[`third_party/ripgrep/manifest.json`](../third_party/ripgrep/manifest.json)。

| Dune 目标 | ripgrep 上游产物 | 依赖边界 |
| --- | --- | --- |
| linux-amd64 | x86_64-unknown-linux-musl | 静态 PIE，不依赖目标机 glibc |
| linux-arm64 | aarch64-unknown-linux-gnu | 动态链接 `/lib/ld-linux-aarch64.so.1`，需要 glibc 2.18 或更新版本；不覆盖原生 musl/Alpine |
| darwin-amd64 | x86_64-apple-darwin | 使用系统 libiconv、libSystem |
| darwin-arm64 | aarch64-apple-darwin | 使用系统 libiconv、libSystem |

上表描述随包 rg 的依赖，不替代 Dune、Go runtime 与 tmux 的完整平台验收。
下载器只读取归档中明确命名的普通文件，不展开任意归档路径或符号链接。
随包许可证包括 ripgrep 的 COPYING、MIT、Unlicense，以及二进制内 PCRE2 10.45
的许可证。PCRE2 的来源与 SHA-256 同样固定；Dune 搜索 API 不因此开放 PCRE2 参数。

fabricd 默认只调用当前程序目录中的 `rg`，不自动使用 PATH 中偶然安装的版本。
缺失时返回 `DEPENDENCY_MISSING`。`DUNE_RG` 是宿主或测试显式选择程序的覆盖入口；
开发者单独运行临时 Go 测试二进制时，可将其设为仓库 `bin/rg` 的绝对路径。

## 发行包

`make release RELEASE_ID=<固定发行ID> RELEASE_BASE_URL=https://example.com/releases/<固定目录>`
构建 Web 资源和 Linux/macOS × amd64/arm64 的 Dune 程序，
准备所有固定依赖，再生成 `bin/dune-<os>-<arch>.tar.gz` 和相邻的 `.sha256` 文件。
每份归档包含：

- `dune`、`tmux`、`rg`，位于同一级程序目录。
- `licenses/`，包含上游授权说明和 tmux/ripgrep 的固定版本清单。

每个平台同时生成 `.manifest.json`、`.ref.json`，以及整个发行的 `upgrade-catalog.json`。
清单记录全部普通文件的摘要、大小及安装权限、当前共享状态合同、不可变归档 URL 和摘要；
引用摘要按 Go `Manifest.Digest` 的 JSON 规则计算。归档成员按路径排序，时间戳固定，
不包含本机用户名或符号链接。发布方将归档放在指定固定 URL 后，宿主加载获准目录：

```sh
dune web --url https://dune.example.com --upgrade-catalog /absolute/upgrade-catalog.json
```

嵌入宿主也可用 `upgrade.NewCatalog` / `ReadCatalog` 配置 `host.Options.UpgradeSource`，
或实现自身发行策略。目录在宿主启动时冻结；更新目录后重启宿主。客户端只提交 `.ref.json`
中的固定身份，不能指定下载命令。单平台已构建产物可用打包脚本的 `--platform OS-ARCH` 发布。

安装和升级必须整体安装这些内容。持久配置、证书、SessionDir、tmux socket 与
工作目录放在发行目录之外。升级保持 tmux 3.7c 不变：新的 tmux 客户端继续连接
旧进程持有的私有 server，不能为了替换文件而重启承载 PTY 的 server。

PTY 超时 helper 在创建 Runtime 时已启动，到期只使用当前进程内的计时、信号和
文件操作。删除旧目录不要求它再次执行旧路径。仍运行的 tmux/helper 可以继续使用
已映射的旧程序文件；操作系统在进程退出后回收相关磁盘空间。

## 验证范围

发行验收需要检查归档成员、可执行权限、归档 SHA-256 与四个平台实际运行行为。
交叉编译和归档检查不能替代 Linux/macOS 各目标机的安装、搜索、终端和升级测试。

本地回归分别覆盖已删除旧 helper 程序后的超时，以及删除旧 tmux 程序后新客户端
继续 attach；`pkg/fabricd` 的 PTY 回归通过 Gateway/SDK 验证关闭 fabricd 期间超时、
重开后读取原因与历史，以及 stop/forget 清理。真实休眠、真实 Agent 和服务管理器
升级仍按[开发流程](workflow.md)及目标平台条件另行验收。

`TestNativeRunnerOnlineUpgrade` 使用独立、唯一的用户服务名称运行真实安装器、worker 和
不同 Dune 发行，通过公开升级 API 及真实 Gateway 路由检查成功与平台注册拒绝后的回滚。
用 `DUNE_TEST_SERVICE_MANAGER=1` 显式启用；该测试在结束时卸载自身创建的服务。
