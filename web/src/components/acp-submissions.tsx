import { useEffect, useState } from "react";
import { APIError, call, errorText, request, runtimeKey, bindingKey, type Binding, type Runtime } from "@/lib/api";
import { Button } from "./ui/button";

const storageKey = "dune.acpSubmissions";
const changed = "dune-submissions-changed";
const controlActions = new Set(["permission", "cancel", "stop", "forget"]);
const actions: Record<string, string> = { new: "新建对话", load: "加载对话", list: "查询 Agent 历史", prompt: "任务", permission: "权限回答", cancel: "取消任务", stop: "停止", forget: "清理" };
type Admission = "unknown" | "accepted" | "not_accepted" | "expired";
type Receipt = { submission_id: string; admission: Admission; operation_ref?: string; stage?: string; error_code?: string; target: { runner_id: string; fabric_id: string; machine_id: string; binding_revision: number; runtime_id: string; runtime_incarnation: string; runtime_generation: number } };
type Saved = { submission_id: string; agent_ref: string; prefix: string; binding: Binding; runtime: Runtime; action: string; created_at: string; admission?: Admission; stage?: string; operation_ref?: string; outcome?: string };

function readSaved(): Saved[] {
 const raw = sessionStorage.getItem(storageKey);
 if (!raw) return [];
 if (raw.length > 4 * 1024 * 1024) throw new Error("本地提交记录超过容量，无法安全读取。");
 const items: unknown = JSON.parse(raw);
 if (!Array.isArray(items) || items.length > 1088 || items.some((item) => !item || typeof item.submission_id !== "string" || typeof item.agent_ref !== "string" || !item.binding || !item.runtime || !Object.hasOwn(actions, item.action))) throw new Error("本地提交记录损坏，未覆盖原记录。");
 return items as Saved[];
}
function writeSaved(items: Saved[]) {
 const raw = JSON.stringify(items);
 if (raw.length > 4 * 1024 * 1024) throw new Error("本地提交记录超过容量，请先移除不再需要的记录。");
 sessionStorage.setItem(storageKey, raw);
 window.dispatchEvent(new Event(changed));
}

// Save only recovery selectors, never prompt/permission bodies. Ordinary work
// cannot consume the local slots reserved for necessary controls.
export function saveACPSubmission(prefix: string, binding: Binding, runtime: Runtime, agentRef: string, action: string): string {
 if (!agentRef || !Object.hasOwn(actions, action)) throw new Error("提交必须包含准确会话引用。");
 const items = readSaved();
 if ((action === "stop" || action === "forget") && items.some((item) => item.action === action && bindingKey(item.binding) === bindingKey(binding) && runtimeKey(item.runtime) === runtimeKey(runtime))) throw new Error("已有这次会话的控制提交记录，请先查询原提交。");
 const pool = (action: string) => action === "stop" || action === "forget" ? action : controlActions.has(action) ? "control" : "ordinary";
 const capacity = { stop: 256, forget: 256, control: 512, ordinary: 64 };
 if (items.filter((item) => pool(item.action) === pool(action)).length >= capacity[pool(action)]) throw new Error("本地提交记录已满，请先移除不再需要的记录。");
 const submissionID = crypto.randomUUID();
 const selector = { id: runtime.id, incarnation: runtime.incarnation, generation: runtime.generation, adapter: runtime.adapter, state: runtime.state };
 writeSaved([...items, { submission_id: submissionID, agent_ref: agentRef, prefix, binding, runtime: selector, action, created_at: new Date().toISOString() }]);
 return submissionID;
}

function matchesReceipt(saved: Saved, receipt: Receipt) {
 const target = receipt?.target;
 return receipt?.submission_id === saved.submission_id && ["unknown", "accepted", "not_accepted", "expired"].includes(receipt.admission) && target?.runner_id === saved.binding.runner_id && target.fabric_id === saved.binding.fabric_id && target.machine_id === saved.binding.machine_id && target.binding_revision === saved.binding.revision && target.runtime_id === saved.runtime.id && target.runtime_incarnation === saved.runtime.incarnation && target.runtime_generation === saved.runtime.generation;
}
export function recordACPReceipt(id: string, value: unknown) {
 const items = readSaved(), saved = items.find((item) => item.submission_id === id), receipt = value as Receipt;
 if (!saved || !matchesReceipt(saved, receipt) || receipt.admission === "accepted" && !receipt.operation_ref) throw new Error("提交回执与发送前保存的身份不一致，请查询原提交。");
 writeSaved(items.map((item) => item === saved ? { ...item, admission: receipt.admission, operation_ref: receipt.operation_ref, stage: receipt.stage } : item));
}

const admissionLabels: Record<Admission, string> = { unknown: "接纳未确认", accepted: "已接纳", not_accepted: "未接纳", expired: "回执已过期" };
const outcomeLabels: Record<string, string> = { pending: "排队中", running: "执行中", completed: "操作已完成", failed: "操作失败", cancelled: "操作已取消", unknown: "操作结果未知", expired: "操作结果已过期" };
const stageLabels: Record<string, string> = { written: "控制已送达", input_unrecoverable: "输入写入未完成", stopping: "正在停止", stopped: "已停止", cleaning: "正在清理", completed: "清理已完成" };

export function ACPSubmissionRecovery({ prefix, binding, runtime }: { prefix: string; binding?: Binding; runtime?: Runtime }) {
 const [items, setItems] = useState<Saved[]>([]), [error, setError] = useState(""), [reading, setReading] = useState<string>();
 const runtimeIdentity = runtime ? runtimeKey(runtime) : "", bindingIdentity = bindingKey(binding);
 useEffect(() => {
  const refresh = () => {
   try { setItems(readSaved().filter((item) => item.prefix === prefix && (!bindingIdentity || bindingKey(item.binding) === bindingIdentity) && (!runtimeIdentity || runtimeKey(item.runtime) === runtimeIdentity))); }
   catch (cause) { setError(errorText(cause)); }
  };
  refresh(); window.addEventListener(changed, refresh);
  return () => window.removeEventListener(changed, refresh);
 }, [prefix, bindingIdentity, runtimeIdentity]);
 const query = async (saved: Saved) => {
  setReading(saved.submission_id); setError("");
  try {
   const receipt = await request<Receipt>(`${saved.prefix}/agents/submission`, { method: "POST", body: JSON.stringify({ submission_id: saved.submission_id, agent_ref: saved.agent_ref }) });
   recordACPReceipt(saved.submission_id, receipt);
   if (receipt.admission === "accepted" && !controlActions.has(saved.action)) {
    let outcome: string;
    try {
     const result = await call<{ state: string; operation_ref: string }>(saved.binding, "agent.operation.wait", { operation_ref: receipt.operation_ref, timeout_ms: 0 }, saved.runtime);
     if (result.operation_ref !== receipt.operation_ref || !Object.hasOwn(outcomeLabels, result.state)) throw new Error("无法确认原操作结果。");
     outcome = result.state;
    } catch (cause) { if (cause instanceof APIError && cause.code === "OPERATION_EXPIRED") outcome = "expired"; else throw cause; }
    writeSaved(readSaved().map((item) => item.submission_id === saved.submission_id ? { ...item, outcome } : item));
   }
  } catch (cause) { setError(`${errorText(cause)}；保留原提交记录，可以重新查询。`); }
  finally { setReading(undefined); }
 };
 const remove = (id: string) => {
  try { writeSaved(readSaved().filter((item) => item.submission_id !== id)); } catch (cause) { setError(errorText(cause)); }
 };
 if (!items.length && !error) return null;
 return <details className="border-t px-3 py-2 text-xs" aria-label={runtimeIdentity ? "提交恢复记录" : "历史提交查询"}><summary>{runtimeIdentity ? "提交记录" : "全部提交记录"} · {items.length} · 刷新后可查询</summary>
  <p className="muted my-2">查询原提交不会重新发送任务。移除记录只清理本页保存的查询标识。</p>
  {error && <p role="alert" className="error-box">{error}</p>}
  <div className="max-h-48 overflow-auto">{items.map((item) => <div className="flex flex-wrap items-center gap-2 border-b py-2" key={item.submission_id}>
   <strong>{actions[item.action]}</strong>{!runtimeIdentity && <span>{item.binding.runner_id} · {item.runtime.id.slice(0, 12)}</span>}<time>{new Date(item.created_at).toLocaleTimeString()}</time>
   <span role="status">{item.outcome ? outcomeLabels[item.outcome] : item.stage && stageLabels[item.stage] || admissionLabels[item.admission ?? "unknown"]}</span>
   <Button size="sm" variant="ghost" disabled={!!reading} onClick={() => void query(item)}>{reading === item.submission_id ? "查询中…" : "查询原提交"}</Button>
   <Button size="sm" variant="ghost" disabled={!!reading} onClick={() => remove(item.submission_id)}>移除本地记录</Button>
  </div>)}</div>
 </details>;
}
