import type { Page } from "@playwright/test";
import type { AgentRuntime, ProfileRecord, Runner } from "../src/lib/api";
import { targetFor, targetKey, type Project, type SavedView } from "../src/workbench/model";
import type { AgentSession, LaunchRequest, LaunchResult } from "../src/workbench/launch";

export function workbenchState() {
  const runners: Runner[] = ["one", "two"].map((id) => ({ id, name: `Runner ${id}`, online: true, kind: "attached", binding: { runner_id: id, machine_id: `machine-${id}`, fabric_id: "attached", revision: 1 } }));
  const runtimes: Record<string, AgentRuntime[]> = Object.fromEntries(runners.map((runner, index) => [runner.id, (["pty", "acp"] as const).map((adapter) => ({ id: `${runner.id}-${adapter}`, incarnation: `boot-${runner.id}`, generation: 1, adapter, state: "running", title: `${index ? "B" : "A"}-${adapter.toUpperCase()}`, working_directory: `/repo-${index ? "b" : "a"}`, activity: { state: "idle", source: adapter, epoch: "events", sequence: 2 } }))]));
  const projects: Project[] = runners.map((runner, index) => ({ id: `project-${index}`, revision: 1, name: `Project ${index ? "B" : "A"}`, directories: [{ id: `directory-${index}`, binding: runner.binding!, path: `/repo-${index ? "b" : "a"}` }] }));
  return {
    runners, runtimes, projects, view: { id: "main", revision: 0, root: null } as SavedView,
    inputs: [] as { path: string; message: any }[], calls: [] as { runner: string; operation: string; payload: any }[],
    connections: [] as string[], viewWrites: [] as SavedView[], conflict: false, starts: 0, discoveryError: false, directoryRequests: [] as string[], directoryPageSize: 32, discoveryIssues: {} as Record<string, string>, recoveryError: false,
    resumeRequests: [] as { id: string; revision: number }[], resumeOutcome: "success" as "success" | "unknown" | "pending",
    profiles: [] as ProfileRecord[], sessions: [] as AgentSession[], launchRequests: [] as LaunchRequest[], launchFailure: "" as "" | "start" | "index",
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
    if (path === "/api/v1/profiles") return reply({ items: state.profiles });
    if (path === "/api/v1/agents") {
      state.directoryRequests.push(url.searchParams.get("cursor") ?? "");
      const start = Number(url.searchParams.get("cursor") ?? 0), end = start + state.directoryPageSize;
      const runners = state.runners.slice(start, end), issues = runners.flatMap((runner) => state.discoveryError || state.discoveryIssues[runner.id] ? [{ runner_id: runner.id, code: state.discoveryIssues[runner.id] ?? "RUNNER_UNAVAILABLE" }] : []);
      const items = runners.flatMap((runner) => !runner.binding || !runner.online || issues.some((issue) => issue.runner_id === runner.id) ? [] : (state.runtimes[runner.id] ?? []).map((runtime) => {
        const target = targetFor(runner.binding!, runtime);
        return { agent_ref: `ref-${runtime.id}`, target, runtime, recovery_error: state.recoveryError ? "RECOVERY_INDEX_UNAVAILABLE" : undefined, session: state.sessions.find((session) => session.selected && session.last_runtime && targetKey(targetFor(session.binding, session.last_runtime)) === targetKey(target)) };
      }));
      return reply({ items, runners: runners.map((runner) => ({ runner, online: runner.online, ready: runner.online })), issues, next_cursor: end < state.runners.length ? String(end) : undefined });
    }
    if (path === "/api/v1/agent-sessions") return reply({ items: state.sessions });
    const recoveryRoute = path.match(/^\/api\/v1\/agent-sessions\/([^/]+)(\/resume)?$/);
    if (recoveryRoute) {
      const session = state.sessions.find((item) => item.id === recoveryRoute[1]);
      if (!session) return reply({ code: "NOT_FOUND", error: "session missing" }, 404);
      if (method === "GET") return reply(session);
      state.resumeRequests.push({ id: session.id, revision: body.revision });
      session.revision++; session.attempt = { id: "resume-attempt", kind: "resume", state: state.resumeOutcome === "success" ? "ready" : state.resumeOutcome === "unknown" ? "unknown" : "capturing", base_revision: body.revision };
      session.status = state.resumeOutcome === "success" ? "available" : state.resumeOutcome === "unknown" ? "unknown" : "pending_capture";
      if (state.resumeOutcome === "success" || state.resumeOutcome === "pending" && session.adapter === "pty") {
        const runtime: AgentRuntime = { ...session.last_runtime!, id: `resumed-${session.adapter}`, incarnation: "resumed-boot", state: "running", title: `Restored ${session.adapter.toUpperCase()}`, working_directory: session.working_directory };
        session.last_runtime = runtime; state.runtimes[session.binding.runner_id].push(runtime);
        return reply({ session, runtime });
      }
      return state.resumeOutcome === "unknown" ? reply({ code: "RESULT_UNKNOWN", error: "load result unknown", result: { session } }, 503) : reply({ session });
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
        const binding = state.runners.find((item) => item.id === runner[1])!.binding!;
        const profile = body.custom ?? state.profiles.find((item) => item.id === body.profile?.id)?.profile;
        const runtime: AgentRuntime = { id: `new-${state.starts}-${profile.adapter}`, incarnation: "new-boot", generation: 1, adapter: profile.adapter, state: "running", title: `New ${profile.adapter}`, working_directory: body.worktree?.path ?? body.working_directory };
        const session: AgentSession = { id: `record-${state.starts}`, revision: 2, selected: state.launchFailure !== "start", binding, project_id: body.project?.id, directory_id: body.worktree ? undefined : body.directory_id, working_directory: runtime.working_directory!, adapter: runtime.adapter, agent_type: "fixture", status: "pending_capture", last_runtime: state.launchFailure === "start" ? undefined : runtime };
        const result: LaunchResult = { session, worktree: body.worktree ? { path: body.worktree.path, branch: body.worktree.branch } : undefined };
        if (state.launchFailure !== "start") { result.runtime = runtime; state.runtimes[runner[1]].push(runtime); }
        state.sessions.push(session);
        if (state.launchFailure) return reply({ code: state.launchFailure === "start" ? "START_FAILED" : "RECOVERY_INDEX_FAILED", error: "startup confirmation failed", result }, 503);
        return reply(result, 201);
      }
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
