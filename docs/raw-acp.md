# 原始 ACP 传输合同

原始模式由客户端保留 JSON-RPC 请求关联、能力和会话状态；宿主只拥有进程、管道和
有界字节传输。客户端丢失协议内存不能靠重新 initialize 恢复原任务，任意新调用方
需要恢复完整交互的产品应使用 managed ACP。

公开 schema：`RawACPState`、`RawACPTake`、`RawACPWrite`、`RawACPRead`、
`RawACPOutput`，以及提交回执中的 `raw_input`。SDK 提供对应的状态、接管、写入和
读取方法。Runner 只有实现相应路径后才声明 `submission.raw`、`acp.raw.state` 和
`acp.raw.read` 能力。实现与进程验收进度见 [实施记录](acp-session-lifecycle-implementation.md)。

客户端首次发送前持有并保存完整 Runtime `SubmissionKey`。接管和写入分别使用
`acp.raw.take`、`acp.raw.write`，共用原 Runtime 的提交键命名空间。接管请求包含
当前 `stream_id`、`expected_epoch` 和调用方生成的 `owner_id`；只有当前 epoch
匹配才能接纳，成功后 epoch 加一。原回执保留新 epoch，响应丢失后查询原键。
只读状态不公开 owner_id，也不会接管输入。

写入必须携带原 stream_id、owner_id、input_epoch、完整消息长度、原始字节 SHA-256
小写十六进制摘要和 data。消息必须为一条完整 JSON-RPC 2.0 对象及原始 LF／CRLF，
不重编码、补换行或移除字节。底层网络分片不改变接纳单位；完整收齐前不写 stdin。
同键改消息、流或 epoch 均冲突。

完整字节和次序交给原宿主后才 accepted；串行 writer 进入 writing；全部原字节交给
同一管道才 written。written 不代表 Agent 已解析或业务成功。fabricd 断线、调用取消
及接管不取消已接纳 writer，后来的消息按接纳顺序排在后面。无法确认的部分写入使
输入永久进入 input_unrecoverable；后续排队消息记录 not_sent，新输入被拒绝，换
连接和接管都不能修复。读输出、诊断和显式 stop 仍可用。

stdout 和 stderr 各有独立的字节偏移窗口。读取返回原字节及 next、window.oldest、
window.next、window.closed。落后于 oldest 返回 STREAM_GAP 和当前窗口，不静默
跳过缺失字节。客户端若决定读取剩余诊断，应显式使用新偏移，不能宣称协议完整。

固定默认值与硬上限：单消息 1 MiB，待写总量 4 MiB／32 条（含正在写入的消息），
stdout 窗口 8 MiB，stderr 窗口 1 MiB，单次读取 256 KiB。消息与接管回执使用独立
注册索引的普通提交额度，不挤占 stop／forget 预留。机器最多 16 个活动宿主，窗口
预算因而有机器级上限；这些计费预算不等同于实际 RSS。
