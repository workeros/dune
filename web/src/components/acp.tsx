import { useEffect, useMemo, useReducer, useRef, useState } from "react";
import { useACPModel } from "./use-acp-model";
import { orderedEntries, type ModelEntry } from "./acp-model";
import { ACPOperations, useACPOperations } from "./acp-operations";
import { Button } from "./ui/button";
import { Input, Textarea } from "./ui/input";
import { socketURL, call, errorText, eventPath, type Binding, type Runtime } from "@/lib/api";

import { conversationReducer, initialConversation, isRecord, readString, formatValue, type Update, type MessageEntry, type ActivityEntry, type ToolEntry, type Entry, type StreamEntry, type BrowserEvent } from "./acp-state";

function streamSummary(message: unknown) {
 if (!isRecord(message)) return { title: "非 JSON 消息", detail: "" };
 const method = readString(message, "method");
 const id = typeof message.id === "string" || typeof message.id === "number" ? String(message.id) : "";
 if (method) return { title: method, detail: id ? `id · ${id}` : "notification" };
 if (message.error) return { title: "RPC error", detail: id ? `id · ${id}` : "response" };
 return { title: "RPC response", detail: id ? `id · ${id}` : "response" };
}

function ToolGroup({ tools }: { tools: ToolEntry[] }) {
 const running = tools.some((tool) => tool.status === "running"), failed = tools.some((tool) => tool.status === "error");
 return <details className="acp-tool-group">
  <summary><span className={`acp-status-mark is-${running ? "running" : failed ? "error" : "complete"}`} />{running ? "正在执行" : failed ? "工具执行结束" : "已完成工具操作"} · {tools.length}<span className="acp-chevron">⌄</span></summary>
  <div className="acp-tool-list">{tools.map((tool) => <details className={`acp-tool-item is-${tool.status}`} key={tool.key}>
   <summary><span className="acp-status-mark" /><span title={tool.input || tool.content || tool.title}>{tool.name ? `${tool.name} · ` : ""}{tool.input || tool.content || tool.title || "工具操作"}</span><span className="acp-chevron">⌄</span></summary>
   <div className="acp-tool-detail"><strong>{tool.title || tool.name || tool.kind || "Tool"}</strong><small>Input</small><pre>{tool.input || tool.content || "无调用参数"}</pre>{tool.output && <><small>Output</small><pre>{tool.output}</pre></>}<span>{tool.status === "running" ? "运行中" : tool.status === "error" ? "失败" : "✓ 成功"}</span></div>
  </details>)}</div>
 </details>;
}

function Conversation({ entries }: { entries: Entry[] }) {
 const timeline = useMemo(() => {
  const groups: (MessageEntry | ActivityEntry | { type: "tools"; key: string; tools: ToolEntry[] })[] = [];
  for (const entry of entries) {
   const last = groups.at(-1);
   if (entry.type === "tool" && last?.type === "tools") last.tools.push(entry);
   else if (entry.type === "tool") groups.push({ type: "tools", key: `tools-${entry.key}`, tools: [entry] });
   else groups.push(entry);
  }
  return groups;
 }, [entries]);
 return <>{timeline.map((entry) => entry.type === "message" ? <article className={`acp-message is-${entry.role}`} key={entry.key}><div><p>{entry.role === "user" ? "你" : "Agent"}</p><div className="whitespace-pre-wrap break-words">{entry.text}</div></div></article> : entry.type === "tools" ? <ToolGroup key={entry.key} tools={entry.tools} /> : <details className="acp-activity" key={entry.key}><summary>{entry.kind}<span className="acp-chevron">⌄</span></summary><pre>{formatValue(entry.value)}</pre></details>)}</>;
}

function ContentBlocks({ blocks }: { blocks: unknown[] }) {
 return <>{blocks.map((block, index) => isRecord(block) && block.type === "text" && typeof block.text === "string" ? <div className="whitespace-pre-wrap break-words" key={index}>{block.text}</div> : <details className="acp-activity" key={index}><summary>{isRecord(block) && typeof block.type === "string" ? block.type : "结构化内容"}</summary><pre>{formatValue(block)}</pre></details>)}</>;
}

function ModelConversation({ entries }: { entries: ModelEntry[] }) {
 return <>{entries.map((entry) => <section key={entry.entry_id} data-entry-id={entry.entry_id}>
  {entry.message ? <article className={`acp-message is-${entry.message.role}`}><div><p>{entry.message.role === "user" ? "你" : entry.message.channel === "thought" ? "Agent · 思考" : "Agent"}</p><ContentBlocks blocks={entry.message.content} />{!!entry.message.tail?.length && <><p className="muted my-2 text-xs">中间内容已省略，以下为保留尾部</p><ContentBlocks blocks={entry.message.tail} /></>}</div></article> : entry.tool ? <details className="acp-tool-item"><summary>{typeof entry.tool.fields.title === "string" ? entry.tool.fields.title : entry.tool.tool_call_id} · {entry.tool.status === "completed" ? "完成" : entry.tool.status === "failed" ? "失败" : entry.tool.status === "unknown" ? "结果未确认" : entry.tool.status === "interrupted" ? "已中断" : "执行中"}</summary><pre>{formatValue(entry.tool.fields)}</pre>{entry.tool.status_reason && <p className="text-xs">{entry.tool.status_reason === "turn_ended_without_tool_result" ? "回合结束时未收到工具结果" : entry.tool.status_reason}</p>}</details> : entry.turn ? <p className="muted px-3 py-1 text-xs">回合 · {entry.turn.state}{entry.turn.stop_reason ? ` · ${entry.turn.stop_reason}` : ""}{entry.turn.error ? ` · ${entry.turn.error.detail}` : ""}</p> : <details className="acp-activity"><summary>{entry.activity?.update_type ?? entry.type}</summary><pre>{formatValue(entry.activity?.data)}</pre></details>}
  {entry.context_incomplete && <p className="muted px-3 text-xs">此条目的部分上下文无法确认。</p>}
  {entry.content_omitted && <p className="muted px-3 text-xs">此条目包含省略内容。</p>}
 </section>)}</>;
}

function ACPStream({ entries }: { entries: StreamEntry[] }) {
 return <aside className="acp-stream" aria-label="ACP Stream"><header><strong>ACP Stream</strong><span>{entries.length} 条</span></header><div className="acp-stream-list">{entries.length === 0 && <p className="muted p-3 text-xs">等待 ACP input / output…</p>}{entries.map((entry) => {
  const summary = streamSummary(entry.message);
  return <details className="acp-stream-item" key={entry.key}><summary><span className={`acp-direction is-${entry.direction}`}>{entry.direction}</span><span className="acp-stream-summary"><strong>{summary.title}</strong><small>{summary.detail}</small></span><time>{entry.receivedAt.toLocaleTimeString("zh-CN", { hour12: false })}</time><span className="acp-chevron">⌄</span></summary><pre>{formatValue(entry.message)}</pre></details>;
 })}</div></aside>;
}

export function ACPPane({ binding, runtime, agentRef, prefix, onNativeChange }: { binding: Binding; runtime: Runtime; agentRef?: string; prefix: string; onNativeChange: () => void }) {
 const [text, setText] = useState(""), [error, setError] = useState(""), [sending, setSending] = useState(false), [sessionID, setSessionID] = useState("");
 const [diagnostics, dispatch] = useReducer(conversationReducer, initialConversation);
 const { streamEntries, streamOpen } = diagnostics;
 const model = useACPModel(binding, runtime);
 const { state, connected, ended } = model;
 const entries = useMemo(() => orderedEntries(model.cache), [model.cache.entries]);
 const scroll = useRef<HTMLDivElement>(null), followTail = useRef(true);
 const requests = useACPOperations(prefix, true, onNativeChange);
 // Raw ACP diagnostics are opt-in and never append to the model conversation.
 useEffect(() => {
  if (!streamOpen || ended) return;
  const socket = new WebSocket(socketURL(eventPath(binding, runtime)));
  socket.onmessage = (event) => {
   try {
    const message = JSON.parse(event.data) as BrowserEvent;
    if (message.type === "acp_stream" && isRecord(message.payload)) dispatch({ type: "stream", direction: message.payload.direction === "input" ? "input" : "output", message: message.payload.message });
    if (message.type === "acp_update") dispatch({ type: "stream", direction: "output", message: { jsonrpc: "2.0", method: "session/update", params: message.payload } });
   } catch { setError("无效 ACP 诊断消息"); }
  };
  return () => socket.close();
 }, [binding, runtime.id, runtime.incarnation, runtime.generation, streamOpen, ended]);
 useEffect(() => { if (followTail.current) scroll.current?.scrollTo({ top: scroll.current.scrollHeight }); }, [entries]);
 const older = async () => {
  const element = scroll.current;
  if (!element) return;
  followTail.current = false;
  const before = element.scrollHeight, top = element.scrollTop;
  await model.older();
  requestAnimationFrame(() => { element.scrollTop = top + element.scrollHeight - before; });
 };
 const act = async (action: string, extra: Record<string, string> = {}) => {
  if (sending) return;
  setSending(true); setError("");
  try {
   if (action === "prompt" || action === "new" || action === "load") {
    if (!agentRef) throw new Error("正在同步会话引用，请稍后提交。");
    const accepted = await requests.submit(action, { agent_ref: agentRef, ...(action === "prompt" ? { text: extra.text, expected_conversation_id: extra.expected_conversation_id } : { action, ...extra }), wait_ms: 0 });
    if (accepted && action === "prompt") setText((current) => current === extra.text ? "" : current);
   } else await call(binding, "acp.action", { action, ...extra }, runtime);
  }
  catch (e) { setError(errorText(e)); } finally { setSending(false); }
 };
 const busy = !!state?.busy, disabled = ended || sending || !connected || !state?.ready;
 return <div className="flex min-h-0 flex-1 flex-col">
  <div className="flex flex-wrap items-center gap-2 border-b border-foreground/20 p-3 text-xs">
   <span role="status">{ended ? "已退出" : connected ? state?.busy ? `执行中 · ${state.busy}` : "已连接" : "重连中"}{!!state?.pending && ` · 排队 ${state.pending}`} {!ended && state?.stop_reason ? ` · ${state.stop_reason}` : ""}</span>
   <Button size="sm" variant="outline" disabled={disabled || !agentRef} onClick={() => void act("new")}>新建对话</Button>
   <Button size="sm" variant="ghost" disabled={disabled || busy || !state?.can_list} onClick={() => void act("list")}>Agent 历史</Button>
   {!ended && busy && <Button size="sm" variant="ghost" disabled={disabled || state?.busy !== "prompt"} onClick={() => void act("cancel")}>取消任务</Button>}
   <Button className="ml-auto" size="sm" variant={streamOpen ? "outline" : "ghost"} aria-pressed={streamOpen} onClick={() => dispatch({ type: "toggleStream" })}>ACP Stream{streamEntries.length ? ` · ${streamEntries.length}` : ""}</Button>
  </div>
  <div className="flex flex-wrap gap-2 border-b border-foreground/10 p-3">
   <Input className="min-w-40 flex-1 font-mono text-xs" aria-label="ACP 会话 ID" placeholder={state?.session_id || "输入 Agent 会话 ID"} value={sessionID} onChange={(event) => setSessionID(event.target.value)} />
   <Button size="sm" variant="outline" disabled={disabled || !agentRef || !state?.can_load || !(sessionID || state?.session_id)} onClick={() => void act("load", { session_id: sessionID || state!.session_id })}>从 Agent 加载历史</Button>
  </div>
  {state?.list && <div className="max-h-40 overflow-auto border-b p-3 text-xs"><p className="mb-2 font-bold">Agent 原生历史 · 当前页</p>{state.list.sessions.length === 0 && <p>Agent 返回当前页无会话。</p>}{state.list.sessions.map((session) => <button className="block w-full truncate p-2 text-left underline" disabled={disabled || !agentRef} key={session.sessionId} onClick={() => { setSessionID(session.sessionId); void act("load", { session_id: session.sessionId, cwd: session.cwd }); }}>{session.title || session.sessionId} · {session.cwd}</button>)}{state.list.nextCursor && <Button size="sm" disabled={disabled || busy} onClick={() => void act("list", { cursor: state.list!.nextCursor! })}>下一页</Button>}</div>}
  {ended && <p className="bg-secondary px-4 py-2 text-xs leading-5">Agent 进程已退出，可以新建 Agent。</p>}
  {!ended && !state?.session_id && !busy && <p className="p-4 text-sm">新建对话，或从 Agent 原生历史中加载一个会话。</p>}
  {(error || model.error || state?.error) && <div className="error-box m-3" role="alert">{error || model.error || state?.error}</div>}
  <div className="flex items-center gap-2 border-b px-3 py-2 text-xs">
   <Button size="sm" variant="ghost" disabled={model.loading} onClick={() => { followTail.current = true; model.refresh(); }}>重新读取最近内容</Button>
   {model.loading && <span role="status">读取中…</span>}
   {model.cache.conversation && <span>{model.cache.conversation.phase === "loading" ? "正在从 Agent 加载" : model.cache.conversation.phase === "creating" ? "正在创建会话" : `${entries.length} 条已读取内容`}</span>}
  </div>
  {model.cache.conversation?.open_error && <p className="error-box m-3">会话打开{model.cache.conversation.open_outcome === "unknown" ? "结果未确认" : "失败"}：{model.cache.conversation.open_error.detail}</p>}
  {(model.cache.conversation?.prefix_evicted || model.cache.rangeEvicted) && <p className="bg-secondary px-4 py-2 text-xs">部分较早内容已从服务端缓存中淘汰。</p>}
  {model.cache.conversation?.content_omitted && <p className="bg-secondary px-4 py-2 text-xs">部分内容因大小限制已省略，条目内标明了保留范围。</p>}
  {model.cache.conversation?.origin === "load" && <p className="px-4 py-2 text-xs">这里显示 Agent 提供并仍被保留的内容；加载成功不代表完整原生历史。</p>}
  {model.cache.browserTruncated && <p className="px-4 py-2 text-xs">本页达到显示容量，已移除较早条目。可重新读取最近内容。</p>}
  <div className={`acp-content${streamOpen ? " has-stream" : ""}`}>
   <div ref={scroll} className="acp-conversation" aria-label="ACP 对话" onScroll={() => { const node = scroll.current; if (node) followTail.current = node.scrollHeight - node.scrollTop - node.clientHeight < 80; }}>
    {model.cache.cursor && !model.cache.browserTruncated && <Button className="mx-auto mb-3" size="sm" variant="outline" disabled={model.loading} onClick={() => void older()}>读取更早内容</Button>}
    <ModelConversation entries={entries} />
    {model.cache.latestThrough !== undefined && !entries.length && <p className="p-4 text-sm">{model.cache.conversation?.prefix_evicted ? "会话正文已全部淘汰，打开结果仍可查询。" : model.cache.conversation?.phase === "loading" ? "Agent 尚未提供回放内容。" : "会话暂无内容。"}</p>}
    {model.cache.conversation?.state && <details className="acp-activity"><summary>当前会话信息</summary><pre>{formatValue(model.cache.conversation.state)}</pre></details>}
   </div>
   {streamOpen && <ACPStream entries={streamEntries} />}
  </div>
  {!ended && !!state?.permissions.length && <div className="max-h-80 overflow-auto border-t bg-[#f6e5aa] p-4">{state.permissions.map((permission) => <section className="mb-3" key={permission.id}><h3 className="mb-2 text-sm font-bold">等待授权 · {permission.params.toolCall?.title || "Agent 工具请求"}</h3><pre aria-label="授权请求详情" className="mb-3 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded border border-foreground/15 bg-card p-3 text-xs">{JSON.stringify(permission.params.toolCall, null, 2)}</pre><div className="flex flex-wrap gap-2">{permission.params.options.map((option) => <Button key={option.optionId} size="sm" variant="outline" disabled={disabled} onClick={() => void act("permission", { permission_id: permission.id, option_id: option.optionId })}>{option.name}</Button>)}</div></section>)}</div>}
  <ACPOperations prefix={prefix} enabled={true} requests={requests} renderUpdates={(updates) => <Conversation entries={updates.reduce<typeof initialConversation>((model, update) => conversationReducer(model, isRecord(update) ? { type: "update", update: update as Update } : { type: "notice", value: "无法解析的操作输出" }), initialConversation).entries} />} />
  <form className="flex gap-2 border-t border-foreground/20 p-3" onSubmit={(event) => { event.preventDefault(); void act("prompt", { text, expected_conversation_id: state?.conversation?.conversation_id ?? "" }); }}><Textarea aria-label="发送给 Agent 的任务" placeholder="描述编码任务…" value={text} onChange={(event) => setText(event.target.value)} maxLength={65536} className="min-h-16 flex-1" /><Button type="submit" disabled={disabled || !agentRef || (busy && state?.busy !== "prompt") || !state?.session_id || !state?.conversation?.conversation_id || !text.trim()}>{busy ? "加入队列" : "发送"}</Button></form>
 </div>;
}
