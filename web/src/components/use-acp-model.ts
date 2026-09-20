import { useEffect, useRef, useState } from "react";
import { APIError, errorText, eventPath, request, runnerPath, socketURL, type Binding, type Runtime } from "@/lib/api";
import type { BrowserEvent, State } from "./acp-state";
import { emptyModel, modelReducer, type ModelAction, type ModelCache, type ModelChange, type ModelGet, type ModelPage, integer } from "./acp-model";

export function useACPModel(binding: Binding, runtime: Runtime) {
 const [cache, setCache] = useState<ModelCache>(emptyModel), [state, setState] = useState<State>();
 const [error, setError] = useState(""), [connected, setConnected] = useState(false), [ended, setEnded] = useState(runtime.state !== "running"), [loading, setLoading] = useState(false);
 const controls = useRef({ older: async () => {}, refresh: () => {} });
 useEffect(() => {
  const controller = new AbortController();
  let disposed = false, current = emptyModel, observed: State | undefined, version = 0, pumping = false, readingState = false, discoverAgain = false, failed = false;
  let socket: WebSocket | undefined, retry: ReturnType<typeof setTimeout> | undefined, timer: ReturnType<typeof setTimeout> | undefined;
  let finished = runtime.state !== "running";
  setLoading(false); setCache(emptyModel); setState(undefined); setError(""); setConnected(false); setEnded(finished);
  const apply = (action: ModelAction) => { current = modelReducer(current, action); setCache(current); };
  const call = <T,>(operation: string, payload: unknown = {}) => request<T>(runnerPath(binding, "call"), { method: "POST", body: JSON.stringify({ operation, payload, runtime }), signal: controller.signal });
  const acceptState = (next: State) => {
   if (disposed || observed && next.revision < observed.revision) return;
   const changed = observed?.conversation?.conversation_id !== next.conversation?.conversation_id;
   if (changed) version++;
   observed = next; setState(next); apply({ type: "select", conversation: next.conversation ?? null });
   if (next.conversation && integer(next.conversation.revision) > integer(current.notifiedThrough)) {
    apply({ type: "notify", change: { conversation_id: next.conversation.conversation_id, previous_revision: current.notifiedThrough, revision: next.conversation.revision, invalidates_all: true } });
   }
   schedule();
  };
  const discover = async () => {
   if (disposed) return;
   if (readingState) { discoverAgain = true; return; }
   readingState = true;
   try { acceptState(await call<State>("acp.state")); }
   catch (cause) { if (!disposed) setError(errorText(cause)); }
   finally { readingState = false; if (discoverAgain && !disposed) { discoverAgain = false; void discover(); } }
  };
  const readFailure = (cause: unknown) => {
   if (disposed) return;
   if (cause instanceof APIError && cause.code === "CONVERSATION_CHANGED") { void discover(); return; }
   failed = true; setError(errorText(cause));
  };
  async function pump() {
   if (disposed || pumping || failed || !current.conversation) return;
   const id = current.conversation.conversation_id, issuedVersion = version;
   const latest = current.latestRevision !== undefined;
   const ids = Object.keys(current.dirty).slice(0, 200);
   if (!latest && !ids.length) return;
   pumping = true; setLoading(true);
   try {
    if (latest) {
     const page = await call<ModelPage>("acp.conversation.read", { conversation_id: id });
     if (!disposed && version === issuedVersion) apply({ type: "page", page, direction: "latest" });
    } else {
     const result = await call<ModelGet>("acp.conversation.get", { conversation_id: id, entry_ids: ids });
     if (result.entries.length + result.missing.length === 0) throw new Error("条目读取没有返回可处理结果，请重新读取会话。");
     if (!disposed && version === issuedVersion) apply({ type: "get", result });
    }
   } catch (cause) { if (version === issuedVersion) readFailure(cause); }
   finally { pumping = false; if (!disposed) { setLoading(false); schedule(); } }
  }
  function schedule() {
   clearTimeout(timer);
   if (!disposed && !failed && (current.latestRevision !== undefined || Object.keys(current.dirty).length)) timer = setTimeout(() => void pump(), 80);
  }
  controls.current = {
   older: async () => {
    if (pumping || disposed || !current.cursor || current.browserTruncated) return;
    const cursor = current.cursor, issuedVersion = version;
    pumping = true; setLoading(true); setError("");
    try {
     const page = await call<ModelPage>("acp.conversation.read", { cursor });
     if (!disposed && issuedVersion === version) apply({ type: "page", page, direction: "older" });
    } catch (cause) { if (version === issuedVersion) readFailure(cause); }
    finally { pumping = false; if (!disposed) { setLoading(false); schedule(); } }
   },
   refresh: () => {
    if (disposed) return;
    failed = false; version++; current = emptyModel; setCache(current); setError("");
    if (observed) apply({ type: "select", conversation: observed.conversation ?? null });
    void discover(); schedule();
   },
  };
  const connect = () => {
   if (disposed || finished) return;
   socket = new WebSocket(socketURL(`${eventPath(binding, runtime)}&acp_events=conversation`));
   socket.onopen = () => {
    if (disposed) return;
    setConnected(true); failed = false; version++; current = emptyModel; setCache(current); setError("");
    // The server has already acknowledged its ordinary Runtime subscription.
    // Reconnect deliberately starts a new recent window instead of claiming
    // that all previously fetched pages were synchronized.
    if (observed) apply({ type: "select", conversation: observed.conversation ?? null });
    void discover(); schedule();
   };
   socket.onmessage = (event) => {
    if (disposed) return;
    try {
     const message = JSON.parse(event.data) as BrowserEvent;
     if (message.type === "acp_state") acceptState(message.payload as State);
     if (message.type === "acp_conversation_changed") {
      const change = message.payload as ModelChange;
      if (change.conversation_id !== current.conversation?.conversation_id) void discover();
      else { apply({ type: "notify", change }); schedule(); }
     }
     if (message.type === "exit") { finished = true; setEnded(true); setConnected(false); socket?.close(); void discover(); }
     if (message.type === "error") setError(message.error ?? "会话订阅中断");
    } catch (cause) { readFailure(cause); }
   };
   socket.onclose = () => { if (!disposed) { setConnected(false); if (!finished) retry = setTimeout(connect, 1500); } };
  };
  if (finished) void discover(); else connect();
  return () => { disposed = true; version++; controller.abort(); clearTimeout(timer); clearTimeout(retry); socket?.close(); };
 }, [binding, runtime.id, runtime.incarnation, runtime.generation, runtime.state]);
 return { cache, state, error, connected, ended, loading, older: () => controls.current.older(), refresh: () => controls.current.refresh() };
}
