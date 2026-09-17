import type { Page } from "@playwright/test";
import type { AgentRuntime, Runner } from "../src/lib/api";
import type { Project, ReadMarker, SavedView } from "../src/workbench/model";

export function workbenchState() {
  const runners: Runner[] = ["one", "two"].map((id) => ({ id, name: `Runner ${id}`, online: true, kind: "attached", binding: { runner_id: id, machine_id: `machine-${id}`, fabric_id: "attached", revision: 1 } }));
  const runtimes: Record<string, AgentRuntime[]> = Object.fromEntries(runners.map((runner, index) => [runner.id, (["pty", "acp"] as const).map((adapter) => ({ id: `${runner.id}-${adapter}`, incarnation: `boot-${runner.id}`, generation: 1, adapter, state: "running", title: `${index ? "B" : "A"}-${adapter.toUpperCase()}`, working_directory: `/repo-${index ? "b" : "a"}`, activity: { state: "idle", source: adapter, epoch: "events", sequence: 2 } }))]));
  const projects: Project[] = runners.map((runner, index) => ({ id: `project-${index}`, revision: 1, name: `Project ${index ? "B" : "A"}`, directories: [{ id: `directory-${index}`, binding: runner.binding!, path: `/repo-${index ? "b" : "a"}` }] }));
  return {
    runners, runtimes, projects, view: { id: "main", revision: 0, root: null } as SavedView,
    markers: new Map<string, number>(), inputs: [] as { path: string; message: any }[], calls: [] as { runner: string; operation: string; payload: any }[],
    connections: [] as string[], viewWrites: [] as SavedView[], conflict: false, starts: 0, discoveryError: false,
  };
}
export type WorkbenchState = ReturnType<typeof workbenchState>;

export async function mockWorkbench(page: Page, state: WorkbenchState) {
  await page.routeWebSocket(/\/api\/v1\/ws\/runners\//, (socket) => {
    const path = new URL(socket.url()).pathname;
    state.connections.push(path);
    if (path.includes("-acp/")) socket.send(JSON.stringify({ type: "acp_state", payload: { ready: true, revision: 1, busy: "", session_id: "native", cwd: "/workspace", can_list: false, can_load: true, permissions: [] } }));
    else socket.send(JSON.stringify({ type: "data", data: `ready ${path}\r\n` }));
    socket.onMessage((message) => state.inputs.push({ path, message: JSON.parse(message.toString()) }));
  });
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request(), url = new URL(request.url()), path = url.pathname, method = request.method();
    const body = method === "GET" || method === "DELETE" ? {} : request.postDataJSON();
    const reply = (value: unknown, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
    if (path.endsWith("/bootstrap")) return reply({ login_methods: [{ kind: "password", url: "/api/v1/auth/login" }], attached: true, managed: false, tenant_scoped: false, local_registration: true });
    if (path.endsWith("/me")) return reply({ id: "owner", email: "owner@test.dev" });
    if (path === "/api/v1/runners") return reply({ items: state.runners });
    if (path === "/api/v1/profiles") return reply({ items: [] });
    if (path === "/api/v1/projects") {
      if (method === "GET") return reply({ items: state.projects });
      const project = { ...body, id: `project-${state.projects.length}`, revision: 1 }; state.projects.push(project); return reply(project, 201);
    }
    if (path.startsWith("/api/v1/projects/")) {
      const id = path.split("/").at(-1)!, index = state.projects.findIndex((project) => project.id === id);
      if (index < 0) return reply({ error: "not found" }, 404);
      if (method === "PUT") { state.projects[index] = { ...state.projects[index], ...body, revision: body.revision + 1 }; return reply(state.projects[index]); }
      if (method === "DELETE") { state.projects.splice(index, 1); return route.fulfill({ status: 204 }); }
      return reply(state.projects[index]);
    }
    if (path.endsWith("/workbench/views/main")) {
      if (method === "GET") return reply(state.view);
      state.viewWrites.push(body);
      if (state.conflict || body.revision !== state.view.revision) return reply({ code: "CONFLICT", error: "metadata conflict" }, 409);
      state.view = { ...body, id: "main", revision: body.revision + 1 }; return reply(state.view);
    }
    const key = (marker: ReadMarker) => JSON.stringify([marker.target, marker.epoch]);
    if (path.endsWith("/read-markers/query")) return reply({ items: body.items.map((marker: ReadMarker) => ({ ...marker, sequence: state.markers.get(key(marker)) ?? 0 })) });
    if (path.endsWith("/read-markers")) { state.markers.set(key(body), Math.max(state.markers.get(key(body)) ?? 0, body.sequence)); return reply({ ...body, sequence: state.markers.get(key(body)) }); }
    const runner = path.match(/\/runners\/([^/]+)\/(call|sessions)$/);
    if (runner) {
      if (runner[2] === "sessions") { state.starts++; return reply({ error: "unexpected start" }, 500); }
      state.calls.push({ runner: runner[1], operation: body.operation, payload: body.payload });
      if (body.operation === "runtime.list") return state.discoveryError ? reply({ error: "discovery temporarily unavailable" }, 503) : reply(state.runtimes[runner[1]] ?? []);
      if (body.operation === "machine.info") return reply({ home: "/workspace" });
      if (body.operation === "git") return reply({ stdout: `diff from ${runner[1]} ${body.payload.directory}`, stderr: "", exit_code: 0, truncated: false });
      if (body.operation === "files") return reply({ items: [{ name: "README.md", is_dir: false }] });
      if (body.operation === "runtime.stop") { state.runtimes[runner[1]] = state.runtimes[runner[1]].filter((runtime) => runtime.id !== body.runtime.id); return reply({}); }
      if (body.operation === "acp.action") return reply({ accepted: true });
      return reply({});
    }
    return reply({ error: `unhandled ${method} ${path}` }, 404);
  });
}
