import { useEffect, useRef, useState } from "react";
import { Button } from "./ui/button";
import { Input, Textarea } from "./ui/input";
import { socketURL, call, errorText, runnerPath, type Binding, type Runtime } from "@/lib/api";

type Permission = { id: string; params: { toolCall: { title?: string; [key: string]: unknown }; options: { optionId: string; name: string; kind: string }[] } };
type State = { revision: number; ready: boolean; busy: string; session_id: string; cwd: string; can_list: boolean; can_load: boolean; error?: string; stop_reason?: string; permissions: Permission[]; list?: { sessions: { sessionId: string; cwd: string; title?: string }[]; nextCursor?: string } };
type Update = { sessionUpdate: string; messageId?: string; content?: { type: string; text?: string }; title?: string; toolCallId?: string; status?: string };
type Entry = { kind: string; text: string; id?: string };

export function ACPPane({ binding, runtime }: { binding: Binding; runtime: Runtime }) {
 const [state, setState] = useState<State>(), [entries, setEntries] = useState<Entry[]>([]), [text, setText] = useState(""), [error, setError] = useState(""), [connected, setConnected] = useState(false), [sending, setSending] = useState(false), [sessionID, setSessionID] = useState(""), [gap, setGap] = useState(true);
 const scroll = useRef<HTMLDivElement>(null);
 const [ended, setEnded] = useState(runtime.state !== "running");
 useEffect(() => {
  let disposed = false, finished = runtime.state !== "running", socket: WebSocket | undefined, retry: ReturnType<typeof setTimeout> | undefined;
  setEnded(finished);
  const connect = () => {
   if (disposed || finished) return;
   const url = socketURL(runnerPath(binding, `sessions/${encodeURIComponent(runtime.id)}/events`));
   socket = new WebSocket(url);
   socket.onopen = () => setConnected(true);
   socket.onmessage = (event) => {
    try {
     const m = JSON.parse(event.data) as { type: string; payload?: unknown; error?: string; data?: string };
     if (m.type === "exit") { finished = true; setEnded(true); setConnected(false); socket?.close(); }
     if (m.type === "acp_state") { const next = m.payload as State; setState((old) => !old || next.revision >= old.revision ? next : old); }
     if (m.type === "acp_reset") { setEntries([]); setGap(false); }
     if (m.type === "acp_update") {
      const update = (m.payload as { update: Update }).update;
      const kind = update.sessionUpdate;
      const value = update.content?.type === "text" ? update.content.text ?? "" : JSON.stringify(update, null, 2);
      setEntries((old) => {
       const last = old[old.length - 1]; let next: Entry[];
       if (kind.endsWith("_message_chunk") && last?.kind === kind && last.id === update.messageId) next = [...old.slice(0, -1), { ...last, text: last.text + value }];
       else next = [...old, { kind, text: value, id: update.messageId }];
       // This is a bounded browser display, not a persisted transcript.
       let bytes = 0; let from = next.length;
       while (from > 0 && next.length - from < 500 && bytes + next[from - 1].text.length <= 1024 * 1024) { bytes += next[--from].text.length; }
       if (from > 0) { setGap(true); next = next.slice(from); }
       return next;
      });
     }
     if (m.type === "error") setError(m.error ?? "ACP 连接中断；未确认的操作不会重发。");
     if (m.type === "stderr" && m.data) setError(m.data.replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "").slice(-4096));
    } catch { setError("无效 ACP 响应"); }
   };
   socket.onclose = () => { if (!disposed) { setConnected(false); if (!finished) { setGap(true); retry = setTimeout(connect, 1500); } } };
  };
  connect();
  return () => { disposed = true; clearTimeout(retry); socket?.close(); };
 }, [binding, runtime.id, runtime.state]);
 useEffect(() => { scroll.current?.scrollTo({ top: scroll.current.scrollHeight }); }, [entries.length, entries[entries.length - 1]?.text]);
 const act = async (action: string, extra: Record<string, string> = {}) => {
  setSending(true); setError("");
  try {
   await call(binding, "acp.action", { action, ...extra }, runtime);
   if (action === "prompt") setText("");
   // Lifecycle state and replay arrive over the already-open event stream.
  } catch (e) { setError(errorText(e)); } finally { setSending(false); }
 };
 const busy = !!state?.busy, disabled = ended || sending || !connected || !state?.ready;
 return <div className="flex min-h-0 flex-1 flex-col">
  <div className="flex flex-wrap items-center gap-2 border-b border-foreground/20 p-3 text-xs">
   <span role="status">{ended ? "已退出" : connected ? state?.busy ? `执行中 · ${state.busy}` : "已连接" : "重连中"}{!ended && state?.stop_reason ? ` · ${state.stop_reason}` : ""}</span>
   <Button size="sm" variant="outline" disabled={disabled || busy} onClick={() => void act("new")}>新建对话</Button>
   <Button size="sm" variant="ghost" disabled={disabled || busy} onClick={() => void act("list")}>Agent 历史</Button>
   {!ended && busy && <Button size="sm" variant="ghost" disabled={disabled || state?.busy !== "prompt"} onClick={() => void act("cancel")}>取消任务</Button>}
  </div>
  <div className="flex flex-wrap gap-2 border-b border-foreground/10 p-3">
   <Input className="min-w-40 flex-1 font-mono text-xs" aria-label="ACP 会话 ID" placeholder={state?.session_id || "输入 Agent 会话 ID"} value={sessionID} onChange={(e) => setSessionID(e.target.value)} />
   <Button size="sm" variant="outline" disabled={disabled || busy || !(sessionID || state?.session_id)} onClick={() => { void act("load", { session_id: sessionID || state!.session_id }); }}>从 Agent 加载历史</Button>
  </div>
  {state?.list && <div className="max-h-40 overflow-auto border-b p-3 text-xs"><p className="mb-2 font-bold">Agent 原生历史 · 当前页</p>{state.list.sessions.length === 0 && <p>Agent 返回当前页无会话。</p>}{state.list.sessions.map((s) => <button className="block w-full truncate p-2 text-left underline" disabled={disabled || busy} key={s.sessionId} onClick={() => { setSessionID(s.sessionId); void act("load", { session_id: s.sessionId, cwd: s.cwd }); }}>{s.title || s.sessionId} · {s.cwd}</button>)}{state.list.nextCursor && <Button size="sm" disabled={disabled || busy} onClick={() => void act("list", { cursor: state.list!.nextCursor! })}>下一页</Button>}</div>}
  {ended && <p className="bg-secondary px-4 py-2 text-xs leading-5">Agent 进程已退出。重新启动 Agent 后，可按其能力加载原生历史。</p>}
  {!ended && gap && <p className="bg-secondary px-4 py-2 text-xs leading-5">这里显示当前连接收到的内容。离线期间的消息需等任务结束后从 Agent 加载；不支持 load 的 Agent 无法补回历史。</p>}
  {!ended && !state?.session_id && !busy && <p className="p-4 text-sm">新建对话，或从 Agent 原生历史中加载一个会话。</p>}
  {(error || state?.error) && <div className="error-box m-3" role="alert">{error || state?.error}</div>}
  <div ref={scroll} className="min-h-0 flex-1 overflow-auto p-4" aria-label="ACP 对话">
   {entries.map((entry, i) => entry.kind.endsWith("_message_chunk") ? <article key={i} className={`mb-3 rounded-lg border border-foreground/15 p-3 ${entry.kind === "user_message_chunk" ? "bg-secondary" : "bg-card"}`}><p className="mb-2 text-xs font-bold">{entry.kind === "user_message_chunk" ? "你" : "Agent"}</p><p className="whitespace-pre-wrap break-words text-sm leading-6">{entry.text}</p></article> : <details key={i} className="mb-2 rounded border border-foreground/15 p-3 text-xs"><summary>{entry.kind}</summary><pre className="mt-2 overflow-auto whitespace-pre-wrap">{entry.text}</pre></details>)}
  </div>
  {!ended && !!state?.permissions.length && <div className="max-h-80 overflow-auto border-t bg-[#f6e5aa] p-4">{state.permissions.map((p) => <section className="mb-3" key={p.id}><h3 className="mb-2 text-sm font-bold">等待授权 · {p.params.toolCall?.title || "Agent 工具请求"}</h3><pre aria-label="授权请求详情" className="mb-3 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded border border-foreground/15 bg-card p-3 text-xs">{JSON.stringify(p.params.toolCall, null, 2)}</pre><div className="flex flex-wrap gap-2">{p.params.options.map((o) => <Button key={o.optionId} size="sm" variant="outline" disabled={disabled} onClick={() => void act("permission", { permission_id: p.id, option_id: o.optionId })}>{o.name}</Button>)}</div></section>)}</div>}
  <form className="flex gap-2 border-t border-foreground/20 p-3" onSubmit={(e) => { e.preventDefault(); void act("prompt", { text }); }}><Textarea aria-label="发送给 Agent 的任务" placeholder="描述编码任务…" value={text} onChange={(e) => setText(e.target.value)} maxLength={65536} className="min-h-16 flex-1" /><Button type="submit" disabled={disabled || busy || !state?.session_id || !text.trim()}>发送</Button></form>
 </div>;
}
