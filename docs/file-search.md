# 文件搜索与编辑读取

`files/search` 使用独立请求字段 `search`，返回 `api.SearchResult`，不提供搜索游标。

```json
{"action":"search","path":"/workspace/project","search":{"mode":"content","query":"hello","case_sensitive":false,"whole_word":true,"regex":false,"include":["**/*.go"],"exclude":["vendor/**"],"context_lines":1}}
```

`mode=path` 对相对路径做字面子串匹配，只返回文件，默认不区分大小写；content 支持字面/rg 默认正则、大小写与全词。两种模式共享 include/exclude glob；include 只缩小范围，不能覆盖 ignore。默认包含隐藏文件、遵守 ignore、排除 `.git`，不跟随目录链接。`include_hidden=false`、`use_ignore_files=false` 可显式调整范围；不启用跨行、PCRE2、多根目录或未保存草稿搜索。

内容结果包含绝对 Path、从 1 开始的 Line、原始 UTF-8 Text（第一行保留 BOM）、UTF-8 字节范围 Ranges（End 不含）及 Before/After 行。客户端必须转换成编辑器列单位。rg 命中文件会再读取一次，在预算内核对完整 UTF-8 字节和每个候选命中，其他编码或扫描期间变化的文件不返回为确认结果。它不是磁盘快照。

默认 200 / 最大 1000 处命中，默认 5 / 最大 10 秒，序列化响应不超过 1 MiB，每 Engine 最多 2 个查询、每 rg 最多 2 个线程。内容单文件最多 16 MiB、单条 rg JSON 与每项上下文最多 256 KiB，最多 10 行上下文；路径模式不按内容大小过滤。结果数达到上限时保守报告不完整，即使恰好等于全集。`Complete=false` 与有界 Issues 给出 result_limit、byte_limit、timeout、content_encoding、path_encoding、file_size_limit、line_size_limit、context_size_limit、file_changed、read_error 等原因；more_issues 表示还有未列出的原因。被正常 ignore/glob 排除不属于错误。

取消通过 HTTP context / SDK stream / Gateway 到达 fabricd，只读搜索会终止并 Wait rg。写入和 Agent 请求的断线语义保持结果未知，不自动重放。服务默认运行随发行包安装的固定 rg；缺失返回 DEPENDENCY_MISSING。详见 [发行依赖](releases.md)。

SandDance 的文件页提供路径/内容模式和筛选开关、按文件分组的结果及不完整反馈。新搜索、切换目录和关闭界面取消旧查询并丢弃迟到结果。点击内容结果核对实际加载文本与上下文；dirty tab 保留草稿，clean tab 也核对磁盘变化，超出已加载范围会明确提示。工作区搜索只读取磁盘。

编辑读取首块显式请求 `with_revision=true`；进入可编辑状态前验证完整 EOF、原始字节长度与 SHA256。后续块可只读元数据；混合版本、非法编码和本地截断不能进入可编辑状态。保存或覆盖上传使用 `conditional` 与已验证 revision，冲突不自动转为 unconditional。新建使用 create，用户明确选择强制覆盖才使用 unconditional。

目录浏览单独使用 `files/list_page`：固定名称排序，默认 200 / 最大 1000 项；浏览器首屏只读一页，按需加载更多。每页仍扫描目录，不提供跨页快照；超过扫描时间预算直接报错，不伪造完整有序页。
