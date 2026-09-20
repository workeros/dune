import type { State } from "../src/components/acp-state";
import type { Page } from "@playwright/test";
import type { AgentRuntime, ProfileRecord, Runner } from "../src/lib/api";
import { targetFor, type Project, type SavedView } from "../src/workbench/model";
import type { LaunchRequest, LaunchResult } from "../src/workbench/launch";

export function workbenchACPState(id: string): State {
 return { ready: true, revision: 1, busy: "", session_id: "native", cwd: "/workspace", can_list: false, can_load: true, permissions: [], conversation: {
  conversation_id: `conversation-${id}`, revision: "1", phase: "ready", origin: "new", open_outcome: "succeeded", open_error: null,
  head_order: "0", retained_from_order: "1", retained_entry_count: 0, prefix_evicted: false, content_omitted: false, context_incomplete: false, native_history_coverage: "not_applicable",
 } };
}

export function workbenchState() {
  const runners: Runner[] = ["one", "two"].map((id) => ({ id, name: `Runner ${id}`, online: true, kind: "attached", binding: { runner_id: id, machine_id: `machine-${id}`, fabric_id: "attached", revision: 1 } }));
  const runtimes: Record<string, AgentRuntime[]> = Object.fromEntries(runners.map((runner, index) => [runner.id, (["pty", "acp"] as const).map((adapter) => ({ id: `${runner.id}-${adapter}`, incarnation: `boot-${runner.id}`, generation: 1, adapter, state: "running", title: `${index ? "B" : "A"}-${adapter.toUpperCase()}`, working_directory: `/repo-${index ? "b" : "a"}`, activity: { state: "idle", source: adapter, epoch: "events", sequence: 2 } }))]));
  const projects: Project[] = runners.map((runner, index) => ({ id: `project-${index}`, revision: 1, name: `Project ${index ? "B" : "A"}`, directories: [{ id: `directory-${index}`, binding: runner.binding!, path: `/repo-${index ? "b" : "a"}` }] }));
  return {
    runners, runtimes, projects, acpStates: Object.fromEntries(Object.values(runtimes).flat().filter((runtime) => runtime.adapter === "acp").map((runtime) => [runtime.id, workbenchACPState(runtime.id)])), view: { id: "main", revision: 0, root: null } as SavedView,
    inputs: [] as { path: string; message: any }[], calls: [] as { runner: string; operation: string; payload: any }[],
    connections: [] as string[], viewWrites: [] as SavedView[], conflict: false, starts: 0, discoveryError: false, directoryRequests: [] as string[], directoryPageSize: 32, discoveryIssues: {} as Record<string, string>,
    profiles: [] as ProfileRecord[], launchRequests: [] as LaunchRequest[], launchFailure: "" as "" | "start" | "mcp",
  };
}
export type WorkbenchState = ReturnType<typeof workbenchState>;

export async function mockWorkbench(page: Page, state: WorkbenchState) {
  await page.routeWebSocket(/\/api\/v1\/ws\/runners\//, (socket) => {
    const path = new URL(socket.url()).pathname;
    state.connections.push(path);
    if (path.includes("-acp/")) socket.send(JSON.stringify({ type: "acp_state", payload: state.acpStates[path.split("/").at(-2)!] ?? workbenchACPState(path.split("/").at(-2)!) }));
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
    if (path === "/api/v1/profiles") return reply({ items: state.profiles });
    if (path === "/api/v1/agents") {
      state.directoryRequests.push(url.searchParams.get("cursor") ?? "");
      const start = Number(url.searchParams.get("cursor") ?? 0), end = start + state.directoryPageSize;
      const runners = state.runners.slice(start, end), issues = runners.flatMap((runner) => state.discoveryError || state.discoveryIssues[runner.id] ? [{ runner_id: runner.id, code: state.discoveryIssues[runner.id] ?? "RUNNER_UNAVAILABLE" }] : []);
      const items = runners.flatMap((runner) => !runner.binding || !runner.online || issues.some((issue) => issue.runner_id === runner.id) ? [] : (state.runtimes[runner.id] ?? []).map((runtime) => {
        const target = targetFor(runner.binding!, runtime);
        return { agent_ref: `ref-${runtime.id}`, target, runtime };
      }));
      return reply({ items, runners: runners.map((runner) => ({ runner, online: runner.online, ready: runner.online })), issues, next_cursor: end < state.runners.length ? String(end) : undefined });
    }
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
    const runner = path.match(/\/runners\/([^/]+)\/(call|sessions)$/);
    if (runner) {
      if (runner[2] === "sessions") {
        state.starts++; state.launchRequests.push(body);
        const profile = body.custom ?? state.profiles.find((item) => item.id === body.profile?.id)?.profile;
        const runtime: AgentRuntime = { id: `new-${state.starts}-${profile.adapter}`, incarnation: "new-boot", generation: 1, adapter: profile.adapter, state: "running", project_id: body.project?.id, directory_id: body.worktree ? undefined : body.directory_id, title: `New ${profile.adapter}`, working_directory: body.worktree?.path ?? body.working_directory };
        const result: LaunchResult = { worktree: body.worktree ? { path: body.worktree.path, branch: body.worktree.branch } : undefined };
        if (state.launchFailure !== "start") { result.runtime = runtime; state.runtimes[runner[1]].push(runtime); }
        if (state.launchFailure) return reply({ code: state.launchFailure === "start" ? "START_FAILED" : "MCP_CONFIGURATION_FAILED", error: "startup confirmation failed", result }, 503);
        return reply(result, 201);
      }
      state.calls.push({ runner: runner[1], operation: body.operation, payload: body.payload });
      if (body.operation === "runtime.list") return state.discoveryError ? reply({ error: "discovery temporarily unavailable" }, 503) : reply(state.runtimes[runner[1]] ?? []);
      if (body.operation === "machine.info") return reply({ home: "/workspace" });
      if (body.operation === "git") return reply({ stdout: `diff from ${runner[1]} ${body.payload.directory}`, stderr: "", exit_code: 0, truncated: false });
      if (body.operation === "files") return reply({ items: [{ name: "README.md", is_dir: false }] });
      if (body.operation === "runtime.stop") { state.runtimes[runner[1]] = state.runtimes[runner[1]].filter((runtime) => runtime.id !== body.runtime.id); return reply({}); }
      if (body.operation === "acp.state") return reply(state.acpStates[body.runtime.id] ?? workbenchACPState(body.runtime.id));
      if (body.operation === "acp.conversation.read") return reply({ conversation: (state.acpStates[body.runtime.id] ?? workbenchACPState(body.runtime.id)).conversation, entries: [], through_order: "0", has_more: false, range_evicted: false });
      if (body.operation === "acp.conversation.get") return reply({ conversation: (state.acpStates[body.runtime.id] ?? workbenchACPState(body.runtime.id)).conversation, entries: [], missing: [], unprocessed_entry_ids: [] });
      if (body.operation === "acp.action") return reply({ accepted: true });
      return reply({});
    }
    return reply({ error: `unhandled ${method} ${path}` }, 404);
  });
}
