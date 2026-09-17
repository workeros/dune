import { useCallback, useEffect, useRef, useState } from "react";
import { bindingKey, call, errorText, listAll, type AgentRuntime, type Runner } from "../lib/api";
import { targetFor, targetKey, type Agent } from "./model";
import type { AgentSession } from "./launch";

type Discovery = { agents: Agent[]; errors: Record<string, string>; checked: Set<string> };
export function useAgents(runners: Runner[]) {
  const [discovery, setDiscovery] = useState<Discovery>({ agents: [], errors: {}, checked: new Set() });
  const current = useRef(runners); current.current = runners;
  const epoch = useRef(0);
  const signature = JSON.stringify(runners.map((runner) => [bindingKey(runner.binding), runner.online]));
  const refresh = useCallback(async () => {
    const generation = ++epoch.current, queue = [...current.current], agents: Agent[] = [], errors: Record<string, string> = {}, checked = new Set<string>();
    const sessions = new Map<string, AgentSession>();
    try {
      for (const session of await listAll<AgentSession>("/api/v1/agent-sessions")) {
        if (session.selected && session.last_runtime) sessions.set(targetKey(targetFor(session.binding, session.last_runtime)), session);
      }
    } catch (cause) { errors.sessions = `会话索引暂不可用：${errorText(cause)}`; }
    await Promise.all(Array.from({ length: Math.min(4, queue.length) }, async () => {
      for (let runner = queue.shift(); runner; runner = queue.shift()) {
        if (!runner.binding || !runner.online) continue;
        const key = bindingKey(runner.binding);
        try {
          const runtimes = await call<AgentRuntime[]>(runner.binding, "runtime.list");
          checked.add(key);
          agents.push(...runtimes.map((runtime) => { const target = targetFor(runner!.binding!, runtime); return { runner: runner!, runtime, target, session: sessions.get(targetKey(target)) }; }));
        } catch (cause) { errors[key] = errorText(cause); }
      }
    }));
    if (generation === epoch.current) setDiscovery((old) => ({ agents: [...agents, ...old.agents.filter((agent) => errors[bindingKey(agent.target.binding)])], errors, checked }));
  }, []);
  useEffect(() => {
    let disposed = false, active = false;
    const tick = async () => { if (active || disposed) return; active = true; try { await refresh(); } finally { active = false; } };
    void tick();
    const timer = setInterval(() => { if (!document.hidden) void tick(); }, 4000);
    return () => { disposed = true; epoch.current++; clearInterval(timer); };
  }, [signature, refresh]);
  const add = (runner: Runner, runtime: AgentRuntime, session?: AgentSession) => {
    epoch.current++;
    if (!runner.binding) return;
    const agent = { runner, runtime, target: targetFor(runner.binding, runtime), session };
    setDiscovery((old) => ({ ...old, agents: [...old.agents.filter((item) => item.runtime.id !== runtime.id || bindingKey(item.target.binding) !== bindingKey(runner.binding)), agent], checked: new Set([...old.checked, bindingKey(runner.binding)]) }));
    return agent;
  };
  return { ...discovery, refresh, add };
}
