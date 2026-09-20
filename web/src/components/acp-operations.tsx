import { useEffect, useRef, useState, type ReactNode } from "react";
import { APIError, errorText, request } from "@/lib/api";
import { Button } from "./ui/button";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "./ui/dialog";

export type ACPOperation = {
 operation_ref: string; state: "pending" | "running" | "completed" | "failed" | "cancelled" | "unknown";
 stop_reason?: string; error?: string; native_session?: { id: string; cwd: string };
};
type Submission = { operation: ACPOperation; label: string; queryError?: string };
type Output = ACPOperation & { position: number; next_position: number; incomplete: boolean; output: { update: unknown }[] | null };
const pending = (operation: ACPOperation) => ["pending", "running"].includes(operation.state);
const labels: Record<ACPOperation["state"], string> = { pending: "排队中", running: "执行中", completed: "已完成", failed: "失败", cancelled: "已取消", unknown: "结果未知" };
export const operationLabel = (operation: ACPOperation) => labels[operation.state];
const parseOperation = (value: unknown): ACPOperation => {
 const operation = value as ACPOperation | undefined;
 if (!operation || typeof operation.operation_ref !== "string" || !operation.operation_ref || !Object.hasOwn(labels, operation.state)) throw new Error("未收到有效操作引用，提交结果未知；请先检查会话。不会自动重发。");
 return operation;
};
const queryError = (cause: unknown) => cause instanceof APIError && ["OPERATION_EXPIRED", "STALE_RUNTIME", "STALE_BINDING", "NOT_FOUND"].includes(cause.code)
 ? "本次操作引用已失效，无法查询。不能据此判断任务没有执行。"
 : `${errorText(cause)}；查询未完成，可以重新查询。`;

export function useACPOperations(prefix: string, enabled: boolean, onNativeChange: () => void) {
 const [items, setItems] = useState<Submission[]>([]), [error, setError] = useState("");
 const sequence = useRef(0), nativeChanged = useRef(onNativeChange); nativeChanged.current = onNativeChange;
 const merge = (operation: ACPOperation) => {
  setItems((old) => old.map((item) => item.operation.operation_ref === operation.operation_ref ? { ...item, operation, queryError: undefined } : item));
  if (operation.native_session) nativeChanged.current();
 };
 const submit = async (action: "prompt" | "new" | "load", body: Record<string, unknown>) => {
  setError("");
  let operation: ACPOperation | undefined;
  const submissionID = crypto.randomUUID();
  const identified = { ...body, submission_id: submissionID };
  try {
   sessionStorage.setItem("dune.lastACPSubmission", JSON.stringify({ submission_id: submissionID, agent_ref: body.agent_ref, prefix }));
   operation = parseOperation(await request<unknown>(`${prefix}/agents/${action === "prompt" ? "prompt" : "open-session"}`, { method: "POST", body: JSON.stringify(identified) })); }
  catch (cause) {
   if (cause instanceof APIError && cause.result) { try { operation = parseOperation(cause.result); } catch { /* No usable receipt. */ } }
   setError(`${errorText(cause)}；不会自动重发。${operation ? "已保留本次操作，可以继续查询。" : "请先检查当前会话。"}`);
  }
  if (!operation) return false;
  const label = `${action === "prompt" ? "任务" : action === "new" ? "新建对话" : "加载对话"} ${++sequence.current}`;
  setItems((old) => [...old.slice(-63), { operation, label }]);
  if (operation.native_session) nativeChanged.current();
  return true;
 };
 const waiting = items.filter((item) => pending(item.operation) && !item.queryError).map((item) => item.operation.operation_ref).join("\n");
 useEffect(() => {
  if (!enabled || !waiting) return;
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const poll = async () => {
   await Promise.all(waiting.split("\n").map(async (ref) => {
    try {
     const result = await request<{ operation: ACPOperation }>(`${prefix}/agents/wait`, { method: "POST", body: JSON.stringify({ operation_ref: ref, timeout_ms: 1000 }), signal: controller.signal });
     if (!controller.signal.aborted) {
      const operation = parseOperation(result.operation);
      if (operation.operation_ref !== ref) throw new Error("操作查询返回了不同的引用");
      merge(operation);
     }
    } catch (cause) { if (!controller.signal.aborted) setItems((old) => old.map((item) => item.operation.operation_ref === ref ? { ...item, queryError: queryError(cause) } : item)); }
   }));
   if (!controller.signal.aborted) timer = setTimeout(() => void poll(), 1000);
  };
  void poll();
  return () => { controller.abort(); clearTimeout(timer); };
 }, [prefix, enabled, waiting]);
 const retry = (ref: string) => setItems((old) => old.map((item) => item.operation.operation_ref === ref ? { ...item, queryError: undefined } : item));
 return { items, error, submit, retry };
}

export function ACPOperations({ prefix, enabled, requests, renderUpdates }: { prefix: string; enabled: boolean; requests: ReturnType<typeof useACPOperations>; renderUpdates: (updates: unknown[]) => ReactNode }) {
 const [selected, setSelected] = useState<Submission>();
 if (!requests.items.length && !requests.error) return null;
 return <div className="acp-operations">
  {requests.error && <p className="error-box" role="alert">{requests.error}</p>}
  {!!requests.items.length && <details open><summary>本页提交 · {requests.items.length} 项（最多保留 64 项）{requests.items.some((item) => pending(item.operation)) ? " · 仍有任务处理中" : ""}</summary><div className="acp-operation-list">{requests.items.map((item) => <div className="acp-operation-row" key={item.operation.operation_ref} aria-label={item.label}>
   <strong>{item.label}</strong><span role="status">{operationLabel(item.operation)}{item.operation.stop_reason ? ` · ${item.operation.stop_reason}` : ""}</span>
   <Button size="sm" variant="ghost" disabled={!enabled} onClick={() => setSelected(item)}>查看本次输出</Button>
   {(item.queryError || item.operation.error) && <p role="alert">{item.queryError || item.operation.error}</p>}
   {item.operation.state === "unknown" && <p>结果未确认，未自动重发。可查看已保留的输出。</p>}
   {item.queryError && <Button size="sm" variant="outline" disabled={!enabled} onClick={() => requests.retry(item.operation.operation_ref)}>重新查询</Button>}
  </div>)}</div></details>}
  <Dialog open={!!selected} onOpenChange={(open) => { if (!open) setSelected(undefined); }}><DialogContent className="acp-operation-dialog"><DialogTitle>{selected?.label} · 本次输出</DialogTitle><DialogDescription className="muted my-2 text-sm">只读取这一操作的输出。完整性提示针对已产生的内容，完成状态不代表任务已验收。</DialogDescription>{selected && <OperationOutput key={selected.operation.operation_ref} prefix={prefix} operation={selected.operation} enabled={enabled} renderUpdates={renderUpdates} />}</DialogContent></Dialog>
 </div>;
}

function OperationOutput({ prefix, operation, enabled, renderUpdates }: { prefix: string; operation: ACPOperation; enabled: boolean; renderUpdates: (updates: unknown[]) => ReactNode }) {
 const [output, setOutput] = useState<{ operation: ACPOperation; updates: unknown[]; position: number; incomplete: boolean }>({ operation, updates: [], position: 0, incomplete: false });
 const [error, setError] = useState(""), [loading, setLoading] = useState(false);
 const current = useRef(output); current.current = output;
 const busy = useRef(false), alive = useRef(true), generation = useRef(0);
 const read = async (signal?: AbortSignal) => {
  if (busy.current || !enabled) return;
  busy.current = true; const version = ++generation.current; setLoading(true); setError("");
  const position = current.current.position;
  try {
   const result = await request<{ operation: Output }>(`${prefix}/agents/read`, { method: "POST", body: JSON.stringify({ operation_ref: operation.operation_ref, position, limit: 64 }), signal });
   const next = result.operation;
   if (!alive.current || signal?.aborted || version !== generation.current) return;
   if (parseOperation(next).operation_ref !== operation.operation_ref || !Number.isSafeInteger(next.position) || !Number.isSafeInteger(next.next_position) || next.position < position || next.next_position < next.position || next.output != null && !Array.isArray(next.output)) throw new Error("无法确认本次输出的位置");
   setOutput((old) => {
    const updates = [...old.updates, ...(next.output ?? []).map((entry) => entry.update)];
    let bytes = 0, from = updates.length;
    while (from > 0) { const size = JSON.stringify(updates[from - 1] ?? null).length; if (bytes + size > 1024 * 1024) break; bytes += size; from--; }
    return { operation: next, updates: updates.slice(from), position: next.next_position, incomplete: old.incomplete || next.incomplete || next.position > position || from > 0 };
   });
  } catch (cause) { if (alive.current && !signal?.aborted && version === generation.current) setError(queryError(cause)); }
  finally { if (version === generation.current) { busy.current = false; if (alive.current && !signal?.aborted) setLoading(false); } }
 };
 useEffect(() => { alive.current = true; const controller = new AbortController(); void read(controller.signal); return () => { alive.current = false; generation.current++; busy.current = false; controller.abort(); }; }, [prefix, operation.operation_ref, enabled]);
 return <>
  <p role="status">{operationLabel(output.operation)}{output.operation.stop_reason ? ` · ${output.operation.stop_reason}` : ""}</p>
  {output.incomplete && <p className="error-box" role="alert">本次输出不完整，部分内容已被裁剪或无法归属。</p>}
  {error && <p className="error-box" role="alert">{error}</p>}
  <div className="acp-operation-output">{renderUpdates(output.updates)}{!output.updates.length && !loading && !error && <p>本次操作尚无可读输出。</p>}</div>
  <Button size="sm" variant="outline" disabled={!enabled || loading} onClick={() => void read()}>{loading ? "读取中…" : "读取后续输出"}</Button>
 </>;
}
