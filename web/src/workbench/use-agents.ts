import { useCallback, useEffect, useRef, useState } from "react";
import { APIError, bindingKey, errorText, request, type AgentRuntime, type Runner } from "../lib/api";
const isAccessError = (error: unknown) => error instanceof APIError && [401, 403].includes(error.status);

import { targetFor, targetKey, type Agent, type AgentTarget } from "./model";

type DirectoryPage = {
  items: { agent_ref: string; target: AgentTarget; runtime: AgentRuntime }[];
  runners: { runner: Pick<Runner, "id" | "binding">; online: boolean; ready: boolean }[];
  issues: { runner_id: string; code: string; runtime?: AgentRuntime }[]; next_cursor?: string; complete: boolean;
};
type Discovery = { agents: Agent[]; errors: Record<string, string>; checked: Set<string> };
const issueText = (code: string) => ({ OFFLINE: "开发环境暂时离线", ACCESS_DENIED: "会话访问已失效", BINDING_CHANGED: "开发环境绑定已变化", UNSUPPORTED: "开发环境暂不支持会话发现", REGISTRATION_INVALID: "部分会话的注册信息无法核实", SESSION_UNAVAILABLE: "部分会话暂不可连接", SESSION_PROTOCOL_UNSUPPORTED: "部分会话需要支持其协议的连接服务", HOST_REGISTRATION_PENDING: "原会话正在完成宿主注册", REGISTRY_UNAVAILABLE: "暂时无法读取会话注册索引", LAUNCH_FAILED: "部分会话的启动已失败，可查询原提交" } as Record<string, string>)[code] ?? "暂时无法读取 Agent 列表";

export function useAgents(runners: Runner[], prefix = "/api/v1") {
  const [discovery, setDiscovery] = useState<Discovery>({ agents: [], errors: {}, checked: new Set() });
  const current = useRef(runners); current.current = runners;
  const epoch = useRef(0);
  const signature = JSON.stringify(runners.map((runner) => [bindingKey(runner.binding), runner.online]));
  const refresh = useCallback(async () => {
    const generation = ++epoch.current, selected = current.current;
    const agents: Agent[] = [], errors: Record<string, string> = {}, checked = new Set<string>(), denied = new Set<string>();
    if (!selected.length) { setDiscovery({ agents: [], errors: {}, checked: new Set() }); return; }
    const seen = new Set<string>();
    let cursor = "";
    try {
      do {
        const page = await request<DirectoryPage>(`${prefix}/agents?limit=32&cursor=${encodeURIComponent(cursor)}`);
        if (generation !== epoch.current) return;
        const issues = new Map(page.issues.map((issue) => [issue.runner_id, issue.code]));
        for (const availability of page.runners) {
          const runner = selected.find((item) => item.id === availability.runner.id && bindingKey(item.binding) === bindingKey(availability.runner.binding));
          if (!runner?.binding) continue;
          const key = bindingKey(runner.binding), issue = issues.get(runner.id);
          if (issue) { errors[key] = issueText(issue); if (issue === "ACCESS_DENIED") denied.add(key); }
          else if (availability.online && (page.complete || page.issues.length > 0)) checked.add(key);
          else if (availability.online) errors[key] = "会话发现尚未完成";
          else errors[key] = issueText("OFFLINE");
        }
        for (const item of page.items) {
          const runner = selected.find((runner) => bindingKey(runner.binding) === bindingKey(item.target.binding));
          if (!runner) continue;
          agents.push({ runner, target: item.target, runtime: item.runtime, ref: item.agent_ref });
        }
        cursor = page.next_cursor ?? "";
        if (cursor && seen.has(cursor)) throw new Error("Agent 列表分页未前进，请刷新重试。");
        seen.add(cursor);
      } while (cursor);
    } catch (cause) {
      for (const runner of selected) {
        if (!runner.binding) continue;
        const key = bindingKey(runner.binding);
        if (!checked.has(key)) errors[key] = errorText(cause);
        if (isAccessError(cause)) denied.add(key);
      }
    }
    const observed = new Set(agents.map((agent) => targetKey(agent.target)));
    if (generation === epoch.current) setDiscovery((old) => ({
      agents: [...agents.filter((agent) => !denied.has(bindingKey(agent.target.binding))), ...old.agents.filter((agent) => {
        const key = bindingKey(agent.target.binding);
        return errors[key] && !checked.has(key) && !denied.has(key) && !observed.has(targetKey(agent.target));
      }).map((agent) => ({ ...agent, runtime: { ...agent.runtime, availability: "unavailable" as const } }))], errors, checked,
    }));
  }, [prefix]);
  useEffect(() => {
    let disposed = false, active = false;
    const tick = async () => { if (active || disposed) return; active = true; try { await refresh(); } finally { active = false; } };
    void tick();
    const timer = setInterval(() => { if (!document.hidden) void tick(); }, 4000);
    return () => { disposed = true; epoch.current++; clearInterval(timer); };
  }, [signature, refresh]);
  const add = (runner: Runner, runtime: AgentRuntime) => {
    epoch.current++;
    if (!runner.binding) return;
    const agent = { runner, runtime, target: targetFor(runner.binding, runtime) };
    setDiscovery((old) => ({ ...old, agents: [...old.agents.filter((item) => item.runtime.id !== runtime.id || bindingKey(item.target.binding) !== bindingKey(runner.binding)), agent], checked: new Set([...old.checked, bindingKey(runner.binding)]) }));
    return agent;
  };
  return { ...discovery, refresh, add };
}
