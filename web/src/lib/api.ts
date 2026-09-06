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
  if (!response.ok) { const error = body as { error?: string; code?: string }; throw new APIError(error.error ?? "请求失败", response.status, error.code ?? "FAILED"); }
  return body as T;
}
export function post<T>(path: string, body: unknown) { return request<T>(path, { method: "POST", body: JSON.stringify(body) }); }
export function call<T>(machine: string, operation: string, payload: unknown = {}, runtime?: Runtime) {
  return post<T>(`/api/machines/${encodeURIComponent(machine)}/call`, { operation, payload, runtime });
}
export const errorText = (error: unknown) => error instanceof Error ? error.message : String(error);
export type User = { id: string; email: string };
export type StartupInfo = { login_methods: { kind: string; url: string }[]; local_registration: boolean; attached: boolean; managed: boolean; public_url: string; gateway_url: string };
export type Machine = { id: string; name: string; os: string; arch: string; online: boolean };
export type Runtime = { id: string; incarnation: string; generation: number; adapter: "pty" | "acp"; state: string; exit_code?: number; title?: string; working_directory?: string };
export type AgentConfig = { id: string; name: string; command: string; args: string[]; env: Record<string, string>; adapter: "pty" | "acp"; history_lines?: number };
