import { useEffect, useMemo, useRef, useState } from "react";
import { Button } from "./ui/button";
import { Input, Textarea } from "./ui/input";
import { socketURL, call, errorText, eventPath, type Binding, type Runtime } from "@/lib/api";

type UnknownRecord = Record<string, unknown>;
type Permission = { id: string; params: { toolCall: { title?: string; [key: string]: unknown }; options: { optionId: string; name: string; kind: string }[] } };
type State = { revision: number; ready: boolean; busy: string; session_id: string; cwd: string; can_list: boolean; can_load: boolean; error?: string; stop_reason?: string; permissions: Permission[]; list?: { sessions: { sessionId: string; cwd: string; title?: string }[]; nextCursor?: string } };
type Update = { sessionUpdate?: string; session_update?: string; messageId?: string; message_id?: string; content?: unknown; title?: string; toolCallId?: string; tool_call_id?: string; status?: string; kind?: string; rawInput?: unknown; raw_input?: unknown; rawOutput?: unknown; raw_output?: unknown; _meta?: unknown; meta?: unknown };
type MessageEntry = { type: "message"; key: string; role: "user" | "agent"; text: string; messageID?: string };
type ToolEntry = { type: "tool"; key: string; id: string; title?: string; name?: string; kind?: string; status: "running" | "complete" | "error"; content?: string; input?: string; output?: string };
type ActivityEntry = { type: "activity"; key: string; kind: string; value: unknown };
type Entry = MessageEntry | ToolEntry | ActivityEntry;
type StreamEntry = { key: number; direction: "input" | "output"; message: unknown; receivedAt: Date };
type BrowserEvent = { type: string; payload?: unknown; error?: string; data?: string };

let entrySequence = 0;
const nextEntryKey = (prefix: string) => `${prefix}-${++entrySequence}`;
const maxBrowserEntries = 5000;
const maxMergedMessageChars = 512 * 1024;

const isRecord = (value: unknown): value is UnknownRecord => !!value && typeof value === "object" && !Array.isArray(value);
const readString = (value: UnknownRecord, ...keys: string[]) => {
 for (const key of keys) if (typeof value[key] === "string" && value[key]) return value[key] as string;
 return undefined;
};
const parseJSON = (value: string): unknown => { try { return JSON.parse(value) as unknown; } catch { return value; } };
const textFromContent = (value: unknown): string => {
 if (typeof value === "string") return value;
 if (Array.isArray(value)) return value.map(textFromContent).filter(Boolean).join("\n");
 if (!isRecord(value)) return "";
 if (value.type === "text" && typeof value.text === "string") return value.text;
 return "content" in value ? textFromContent(value.content) : "";
};
const formatValue = (value: unknown): string => {
 if (typeof value === "string") {
  const parsed = parseJSON(value);
  return typeof parsed === "string" ? parsed : JSON.stringify(parsed, null, 2);
 }
 if (value === undefined || value === null) return "";
 try { return JSON.stringify(value, null, 2); } catch { return String(value); }
};
const toolStatus = (value?: string): ToolEntry["status"] => {
 const status = value?.toLowerCase();
 if (["failed", "failure", "error", "cancelled", "canceled"].includes(status ?? "")) return "error";
 if (["completed", "complete", "success", "succeeded"].includes(status ?? "")) return "complete";
 return "running";
};
const updateEntries = (entries: Entry[], update: Update): Entry[] => {
 const kind = update.sessionUpdate ?? update.session_update ?? "session_update";
 const messageID = update.messageId ?? update.message_id;
 if (["user_message_chunk", "agent_message_chunk", "user_message", "agent_message"].includes(kind)) {
  const text = textFromContent(update.content);
  if (!text) return entries;
  const role = kind.startsWith("user_") ? "user" : "agent";
  const chunk = kind.endsWith("_chunk");
  const last = entries.at(-1);
  if (chunk && last?.type === "message" && last.role === role && last.messageID === messageID && last.text.length + text.length <= maxMergedMessageChars) {
   return [...entries.slice(0, -1), { ...last, text: last.text + text }];
  }
  return [...entries, { type: "message", key: nextEntryKey("message"), role, text, messageID }];
 }
 if (kind === "tool_call" || kind === "tool_call_update") {
  const id = update.toolCallId ?? update.tool_call_id;
  if (!id) return [...entries, { type: "activity", key: nextEntryKey("activity"), kind, value: update }];
  const rawMeta = update._meta ?? update.meta;
  const meta = isRecord(rawMeta) ? rawMeta : {};
  const genius = isRecord(meta.genius) ? meta.genius : {};
  const name = readString(genius, "tool_name", "toolName");
  const description = readString(genius, "tool_description", "toolDescription");
  const next: Partial<ToolEntry> = {
   title: update.title,
   name,
   kind: update.kind,
   status: toolStatus(update.status),
   content: description || textFromContent(update.content),
   input: formatValue(update.rawInput ?? update.raw_input),
   output: formatValue(update.rawOutput ?? update.raw_output),
  };
  const index = entries.findIndex((entry) => entry.type === "tool" && entry.id === id);
  if (index >= 0) return entries.map((entry, entryIndex) => entryIndex === index && entry.type === "tool" ? {
   ...entry,
   title: next.title || entry.title,
   name: next.name || entry.name,
   kind: next.kind || entry.kind,
   status: update.status ? next.status! : entry.status,
   content: next.content || entry.content,
   input: next.input || entry.input,
   output: next.output || entry.output,
  } : entry);
  return [...entries, { type: "tool", key: `tool-${id}`, id, status: next.status!, title: next.title, name: next.name, kind: next.kind, content: next.content, input: next.input, output: next.output }];
 }
 return [...entries, { type: "activity", key: nextEntryKey("activity"), kind, value: update }];
};

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

function ACPStream({ entries }: { entries: StreamEntry[] }) {
 return <aside className="acp-stream" aria-label="ACP Stream"><header><strong>ACP Stream</strong><span>{entries.length} 条</span></header><div className="acp-stream-list">{entries.length === 0 && <p className="muted p-3 text-xs">等待 ACP input / output…</p>}{entries.map((entry) => {
  const summary = streamSummary(entry.message);
  return <details className="acp-stream-item" key={entry.key}><summary><span className={`acp-direction is-${entry.direction}`}>{entry.direction}</span><span className="acp-stream-summary"><strong>{summary.title}</strong><small>{summary.detail}</small></span><time>{entry.receivedAt.toLocaleTimeString("zh-CN", { hour12: false })}</time><span className="acp-chevron">⌄</span></summary><pre>{formatValue(entry.message)}</pre></details>;
 })}</div></aside>;
}

export function ACPPane({ binding, runtime }: { binding: Binding; runtime: Runtime }) {
 const [state, setState] = useState<State>(), [entries, setEntries] = useState<Entry[]>([]), [streamEntries, setStreamEntries] = useState<StreamEntry[]>([]), [streamOpen, setStreamOpen] = useState(false), [text, setText] = useState(""), [error, setError] = useState(""), [connected, setConnected] = useState(false), [sending, setSending] = useState(false), [sessionID, setSessionID] = useState(""), [gap, setGap] = useState(true);
 const scroll = useRef<HTMLDivElement>(null), streamSequence = useRef(0);
 const [ended, setEnded] = useState(runtime.state !== "running");
 useEffect(() => {
  let disposed = false, finished = runtime.state !== "running", socket: WebSocket | undefined, retry: ReturnType<typeof setTimeout> | undefined;
  setEnded(finished);
  const appendStream = (direction: "input" | "output", message: unknown) => setStreamEntries((old) => {
   let next = [...old, { key: ++streamSequence.current, direction, message, receivedAt: new Date() }];
   let bytes = 0, from = next.length;
   while (from > 0 && next.length - from < maxBrowserEntries) { const size = formatValue(next[from - 1].message).length; if (bytes + size > 1024 * 1024) break; bytes += size; from--; }
   return from > 0 ? next.slice(from) : next;
  });
  const connect = () => {
   if (disposed || finished) return;
   const url = socketURL(eventPath(binding, runtime));
   socket = new WebSocket(url);
   socket.onopen = () => setConnected(true);
   socket.onmessage = (event) => {
    try {
     const m = JSON.parse(event.data) as BrowserEvent;
     if (m.type === "exit") { finished = true; setEnded(true); setConnected(false); socket?.close(); }
     if (m.type === "acp_state") { const next = m.payload as State; setState((old) => !old || next.revision >= old.revision ? next : old); }
     if (m.type === "acp_reset") { setEntries([]); setGap(false); }
     if (m.type === "acp_notice") {
      setEntries((old) => [...old, { type: "activity", key: nextEntryKey("notice"), kind: "Dune · 大型 ACP 内容已省略", value: m.payload }]);
      setGap(true);
     }
     if (m.type === "acp_stream" && isRecord(m.payload)) {
      const direction = m.payload.direction === "input" ? "input" : "output";
      appendStream(direction, m.payload.message);
     }
     if (m.type === "acp_update") {
      const payload = m.payload as { update: Update };
      setEntries((old) => {
       let next = updateEntries(old, payload.update);
       let bytes = 0, from = next.length;
       while (from > 0 && next.length - from < maxBrowserEntries) { const size = formatValue(next[from - 1]).length; if (bytes + size > 1024 * 1024) break; bytes += size; from--; }
       if (from > 0) { setGap(true); next = next.slice(from); }
       return next;
      });
      appendStream("output", m.data ? parseJSON(m.data) : { jsonrpc: "2.0", method: "session/update", params: m.payload });
     }
     if (m.type === "error") setError(m.error ?? "ACP 连接中断；未确认的操作不会重发。");
     if (m.type === "stderr" && m.data) setError(m.data.replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "").slice(-4096));
    } catch { setError("无效 ACP 响应"); }
   };
   socket.onclose = () => { if (!disposed) { setConnected(false); if (!finished) { setGap(true); retry = setTimeout(connect, 1500); } } };
  };
  connect();
  return () => { disposed = true; clearTimeout(retry); socket?.close(); };
 }, [binding, runtime.id, runtime.incarnation, runtime.generation, runtime.state]);
 useEffect(() => { scroll.current?.scrollTo({ top: scroll.current.scrollHeight }); }, [entries]);
 const act = async (action: string, extra: Record<string, string> = {}) => {
  setSending(true); setError("");
  try { await call(binding, "acp.action", { action, ...extra }, runtime); if (action === "prompt") setText(""); }
  catch (e) { setError(errorText(e)); } finally { setSending(false); }
 };
 const busy = !!state?.busy, disabled = ended || sending || !connected || !state?.ready;
 return <div className="flex min-h-0 flex-1 flex-col">
  <div className="flex flex-wrap items-center gap-2 border-b border-foreground/20 p-3 text-xs">
   <span role="status">{ended ? "已退出" : connected ? state?.busy ? `执行中 · ${state.busy}` : "已连接" : "重连中"}{!ended && state?.stop_reason ? ` · ${state.stop_reason}` : ""}</span>
   <Button size="sm" variant="outline" disabled={disabled || busy} onClick={() => void act("new")}>新建对话</Button>
   <Button size="sm" variant="ghost" disabled={disabled || busy || !state?.can_list} onClick={() => void act("list")}>Agent 历史</Button>
   {!ended && busy && <Button size="sm" variant="ghost" disabled={disabled || state?.busy !== "prompt"} onClick={() => void act("cancel")}>取消任务</Button>}
   <Button className="ml-auto" size="sm" variant={streamOpen ? "outline" : "ghost"} aria-pressed={streamOpen} onClick={() => setStreamOpen((open) => !open)}>ACP Stream{streamEntries.length ? ` · ${streamEntries.length}` : ""}</Button>
  </div>
  <div className="flex flex-wrap gap-2 border-b border-foreground/10 p-3">
   <Input className="min-w-40 flex-1 font-mono text-xs" aria-label="ACP 会话 ID" placeholder={state?.session_id || "输入 Agent 会话 ID"} value={sessionID} onChange={(event) => setSessionID(event.target.value)} />
   <Button size="sm" variant="outline" disabled={disabled || busy || !state?.can_load || !(sessionID || state?.session_id)} onClick={() => void act("load", { session_id: sessionID || state!.session_id })}>从 Agent 加载历史</Button>
  </div>
  {state?.list && <div className="max-h-40 overflow-auto border-b p-3 text-xs"><p className="mb-2 font-bold">Agent 原生历史 · 当前页</p>{state.list.sessions.length === 0 && <p>Agent 返回当前页无会话。</p>}{state.list.sessions.map((session) => <button className="block w-full truncate p-2 text-left underline" disabled={disabled || busy} key={session.sessionId} onClick={() => { setSessionID(session.sessionId); void act("load", { session_id: session.sessionId, cwd: session.cwd }); }}>{session.title || session.sessionId} · {session.cwd}</button>)}{state.list.nextCursor && <Button size="sm" disabled={disabled || busy} onClick={() => void act("list", { cursor: state.list!.nextCursor! })}>下一页</Button>}</div>}
  {ended && <p className="bg-secondary px-4 py-2 text-xs leading-5">Agent 进程已退出。重新启动 Agent 后，可按其能力加载原生历史。</p>}
  {!ended && gap && <p className="bg-secondary px-4 py-2 text-xs leading-5">这里显示当前连接收到的内容。离线期间的消息需等任务结束后从 Agent 加载；不支持 load 的 Agent 无法补回历史。</p>}
  {!ended && !state?.session_id && !busy && <p className="p-4 text-sm">新建对话，或从 Agent 原生历史中加载一个会话。</p>}
  {(error || state?.error) && <div className="error-box m-3" role="alert">{error || state?.error}</div>}
  <div className={`acp-content${streamOpen ? " has-stream" : ""}`}>
   <div ref={scroll} className="acp-conversation" aria-label="ACP 对话"><Conversation entries={entries} /></div>
   {streamOpen && <ACPStream entries={streamEntries} />}
  </div>
  {!ended && !!state?.permissions.length && <div className="max-h-80 overflow-auto border-t bg-[#f6e5aa] p-4">{state.permissions.map((permission) => <section className="mb-3" key={permission.id}><h3 className="mb-2 text-sm font-bold">等待授权 · {permission.params.toolCall?.title || "Agent 工具请求"}</h3><pre aria-label="授权请求详情" className="mb-3 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded border border-foreground/15 bg-card p-3 text-xs">{JSON.stringify(permission.params.toolCall, null, 2)}</pre><div className="flex flex-wrap gap-2">{permission.params.options.map((option) => <Button key={option.optionId} size="sm" variant="outline" disabled={disabled} onClick={() => void act("permission", { permission_id: permission.id, option_id: option.optionId })}>{option.name}</Button>)}</div></section>)}</div>}
  <form className="flex gap-2 border-t border-foreground/20 p-3" onSubmit={(event) => { event.preventDefault(); void act("prompt", { text }); }}><Textarea aria-label="发送给 Agent 的任务" placeholder="描述编码任务…" value={text} onChange={(event) => setText(event.target.value)} maxLength={65536} className="min-h-16 flex-1" /><Button type="submit" disabled={disabled || busy || !state?.session_id || !text.trim()}>发送</Button></form>
 </div>;
}
