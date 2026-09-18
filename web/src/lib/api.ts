// The workbench is served at the canonical deployment directory (ending /).
// Keep request and subscription paths relative to that directory.
export function siteURL(path: string): URL { return new URL(path.replace(/^\//, ""), new URL(".", window.location.href)); }
export function socketURL(path: string): URL {
 const url = siteURL(path); url.protocol = url.protocol === "https:" ? "wss:" : "ws:"; return url;
}
export class APIError extends Error { constructor(message: string, public status: number, public code: string, public result?: unknown) { super(message); } }
export async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(siteURL(path), { credentials: "same-origin", ...options, headers: { "Content-Type": "application/json", "X-Dune-Request": "1", ...options.headers } });
  if (response.status === 204 && response.ok) return undefined as T;
  let body: unknown;
  try { body = await response.json(); } catch { throw new APIError(`服务返回了无效响应（${response.status}）`, response.status, "INVALID_RESPONSE"); }
  if (!response.ok) { const error = body as { error?: string; code?: string; result?: unknown }; throw new APIError(error.code === "BINDING_CHANGED" ? "环境绑定已变化，请刷新列表并重新选择环境。" : error.code === "STALE_RUNTIME" ? "会话执行身份已变化，请重新选择会话。" : error.error ?? "请求失败", response.status, error.code ?? "FAILED", error.result); }
  return body as T;
}
export function post<T>(path: string, body: unknown) { return request<T>(path, { method: "POST", body: JSON.stringify(body) }); }
export function call<T>(binding: Binding, operation: string, payload: unknown = {}, runtime?: Runtime) {
  return post<T>(runnerPath(binding, "call"), { operation, payload, runtime });
}
export const errorText = (error: unknown) => error instanceof Error ? error.message : String(error);
export type User = { id: string; email: string };
export type StartupInfo = { login_methods: { kind: string; url: string }[]; local_registration: boolean; attached: boolean; managed: boolean; tenant_scoped: boolean; public_url: string; gateway_url: string };
export type Binding = { runner_id: string; fabric_id: string; machine_id: string; revision: number };
export type Runner = { id: string; name: string; kind: string; tenant_id?: string; binding?: Binding; os?: string; arch?: string; online: boolean };
export type BoundRunner = Runner & { binding: Binding };
export function bindingKey(binding?: Binding): string { return binding ? JSON.stringify([binding.runner_id, binding.fabric_id, binding.machine_id, binding.revision]) : ""; }
export function runnerPath(binding: Binding, suffix: string): string {
  const query = new URLSearchParams({ machine_id: binding.machine_id, fabric_id: binding.fabric_id, revision: String(binding.revision) });
  return `/api/v1/runners/${encodeURIComponent(binding.runner_id)}/${suffix}?${query}`;
}
export type Runtime = {
 project_id?: string; directory_id?: string; id: string; incarnation: string; generation: number; adapter: "pty" | "acp"; state: string; exit_code?: number; stop_reason?: string; started_at?: string; deadline_at?: string; title?: string; working_directory?: string };
export type AgentActivity = { state: "unknown" | "working" | "idle" | "blocked"; source: string; agent?: string; foreground?: string; epoch: string; sequence: number };
export type AgentRuntime = Runtime & { activity?: AgentActivity };
export function runtimeKey(runtime: Runtime): string { return JSON.stringify([runtime.id, runtime.incarnation, runtime.generation]); }
export function eventPath(binding: Binding, runtime: Runtime): string {
  const query = new URLSearchParams({
    machine_id: binding.machine_id,
    fabric_id: binding.fabric_id,
    revision: String(binding.revision),
    incarnation: runtime.incarnation,
    generation: String(runtime.generation),
  });
  return `/api/v1/ws/runners/${encodeURIComponent(binding.runner_id)}/sessions/${encodeURIComponent(runtime.id)}/events?${query}`;
}
export type ProfileCommand = { name?: string; argv?: string[]; run?: string; shell?: string; timeout_seconds?: number };
export type Profile = { version: 1; kind: "environment" | "agent"; working_directory: string; env?: Record<string, string>; setup: { steps: ProfileCommand[] | null }; start: ProfileCommand; adapter: "pty" | "acp" | ""; history_lines?: number; managed_acp?: boolean };
export type ProfileRecord = { id: string; owner_id: string; name: string; description: string; revision: number; profile: Profile; created_by: { type: string; subject: string }; created_at: string; updated_at: string };

export type ManagedField = { name: string; label: string; type: "string" | "integer" | "boolean"; required: boolean; minimum?: string; maximum?: string; max_length?: number; choices?: string[] };
export type ManagedUnavailableReason = "maintenance" | "capacity" | "configuration" | "unreachable" | "unknown";
export type ManagedTemplate = { fabric_id: string; id: string; version: string; name: string; disabled: boolean; fields: ManagedField[]; available: boolean; unavailable_reason?: ManagedUnavailableReason };
export type ManagedOperation = {
  id: string; runner_id: string; fabric_id: string; binding_revision: number; action: "create" | "renew" | "destroy" | "pause" | "resume";
  created_at: string; finished: boolean; outcome?: string; stage?: string; provider_outcome?: string; resource_ref?: string; error_code?: string;
  expires_at?: string; renewal_policy_version?: string; renewal_reason?: string; renewal_observed_at?: string; renewal_next_check_at?: string; renewal_until?: string;
  access_closed: boolean; access_suspended: boolean; resource_state?: "ready" | "paused" | "expired" | "gone" | "unknown"; credential_state?: "ready" | "degraded";
  capabilities?: { pause_resume: boolean; disk_snapshot: boolean }; access_close_outcome?: string; access_close_deadline?: string;
};
export type ManagedCreation = { runner: Runner; operation: ManagedOperation };

export type Page<T> = { items: T[]; next_cursor?: string };

export async function listAll<T>(path: string): Promise<T[]> {
  const items: T[] = [], seen = new Set<string>();
  let cursor = "";
  do {
    const page = await request<Page<T>>(`${path}?limit=100&cursor=${encodeURIComponent(cursor)}`);
    items.push(...page.items);
    cursor = page.next_cursor ?? "";
    if (cursor && seen.has(cursor)) throw new Error("列表分页未前进，请重新读取。");
    seen.add(cursor);
  } while (cursor);
  return items;
}
