// The workbench is served at the canonical deployment directory (ending /).
// Keep request and subscription paths relative to that directory.
export function siteURL(path: string): URL { return new URL(path.replace(/^\//, ""), new URL(".", window.location.href)); }
export function socketURL(path: string): URL {
 const url = siteURL(path); url.protocol = url.protocol === "https:" ? "wss:" : "ws:"; return url;
}
export class APIError extends Error { constructor(message: string, public status: number, public code: string) { super(message); } }
export async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(siteURL(path), { credentials: "same-origin", ...options, headers: { "Content-Type": "application/json", "X-Dune-Request": "1", ...options.headers } });
  let body: unknown;
  try { body = await response.json(); } catch { throw new APIError(`服务返回了无效响应（${response.status}）`, response.status, "INVALID_RESPONSE"); }
  if (!response.ok) { const error = body as { error?: string; code?: string }; throw new APIError(error.code === "BINDING_CHANGED" ? "环境绑定已变化，请刷新列表并重新选择环境。" : error.code === "STALE_RUNTIME" ? "会话执行身份已变化，请重新选择会话。" : error.error ?? "请求失败", response.status, error.code ?? "FAILED"); }
  return body as T;
}
export function post<T>(path: string, body: unknown) { return request<T>(path, { method: "POST", body: JSON.stringify(body) }); }
export function call<T>(binding: Binding, operation: string, payload: unknown = {}, runtime?: Runtime) {
  return post<T>(runnerPath(binding, "call"), { operation, payload, runtime });
}
export const errorText = (error: unknown) => error instanceof Error ? error.message : String(error);
export type User = { id: string; email: string };
export type StartupInfo = { login_methods: { kind: string; url: string }[]; local_registration: boolean; attached: boolean; managed: boolean; public_url: string; gateway_url: string };
export type Binding = { runner_id: string; fabric_id: string; machine_id: string; revision: number };
export type Runner = { id: string; name: string; kind: string; binding?: Binding; os?: string; arch?: string; online: boolean };
export type BoundRunner = Runner & { binding: Binding };
export function bindingKey(binding?: Binding): string { return binding ? JSON.stringify([binding.runner_id, binding.fabric_id, binding.machine_id, binding.revision]) : ""; }
export function runnerPath(binding: Binding, suffix: string): string {
  const query = new URLSearchParams({ machine_id: binding.machine_id, fabric_id: binding.fabric_id, revision: String(binding.revision) });
  return `/api/runners/${encodeURIComponent(binding.runner_id)}/${suffix}?${query}`;
}
export type Runtime = { id: string; incarnation: string; generation: number; adapter: "pty" | "acp"; state: string; exit_code?: number; title?: string; working_directory?: string };
export function runtimeKey(runtime: Runtime): string { return JSON.stringify([runtime.id, runtime.incarnation, runtime.generation]); }
export function eventPath(binding: Binding, runtime: Runtime): string {
  return runnerPath(binding, `sessions/${encodeURIComponent(runtime.id)}/events`) + "&" + new URLSearchParams({ incarnation: runtime.incarnation, generation: String(runtime.generation) });
}
export type AgentConfig = { id: string; name: string; command: string; args: string[]; env: Record<string, string>; adapter: "pty" | "acp"; history_lines?: number };

export type Page<T> = { items: T[]; next_cursor?: string };
