import { useCallback, useEffect, useRef, useState } from "react";
import { bindingKey, errorText, request, siteURL, type AgentRuntime, type Runner } from "../lib/api";
import { targetFor, targetKey, type Agent } from "./model";
import { DirectoryCache, type DirectoryEvent, type DirectoryPage, type DiscoveryBatch } from "./directory-cache";

type Discovery = { agents: Agent[]; errors: Record<string, string>; checked: Set<string> };
const emptyDiscovery = (): Discovery => ({ agents: [], errors: {}, checked: new Set() });
const issueText = (code: string) => ({ OFFLINE: "开发环境暂时离线", ACCESS_DENIED: "会话访问已失效", BINDING_CHANGED: "开发环境绑定已变化", UNSUPPORTED: "开发环境暂不支持会话发现", REGISTRATION_INVALID: "部分会话的注册信息无法核实", SESSION_UNAVAILABLE: "部分会话暂不可连接", SESSION_PROTOCOL_UNSUPPORTED: "部分会话需要支持其协议的连接服务", HOST_REGISTRATION_PENDING: "原会话正在完成宿主注册", REGISTRY_UNAVAILABLE: "暂时无法读取会话注册索引", LAUNCH_FAILED: "部分会话的启动已失败，可查询原提交" } as Record<string, string>)[code] ?? "暂时无法读取 Agent 列表";

export function useAgents(runners: Runner[], prefix = "/api/v1") {
  const [discovery, setDiscovery] = useState<Discovery>(emptyDiscovery);
  const current = useRef(runners); current.current = runners;
  const refreshAction = useRef<() => Promise<void>>(async () => {});
  const signature = JSON.stringify(runners.map((runner) => [runner.id, bindingKey(runner.binding)]));
  const refresh = useCallback(() => refreshAction.current(), []);

  useEffect(() => {
    const cache = new DirectoryCache();
    let disposed = false, source: EventSource | undefined, batch: DiscoveryBatch | undefined;
    let retry: ReturnType<typeof setTimeout> | undefined, backoff = 500, discoveryEpoch = 0;
    let controller: AbortController | undefined;
    let errors: Record<string, string> = {}, checked = new Set<string>(), observed = new Set<string>();
    const publish = () => {
      const agents: Agent[] = [];
      for (const item of cache.snapshot()) {
        const key = bindingKey(item.target.binding);
        const runner = current.current.find((value) => bindingKey(value.binding) === key);
        if (!runner || errors[key] === issueText("ACCESS_DENIED")) continue;
        const unavailable = errors[key] && !observed.has(targetKey(item.target));
        agents.push({ runner, target: item.target, runtime: unavailable ? { ...item.runtime, availability: "unavailable" } : item.runtime, ref: item.agent_ref });
      }
      setDiscovery({ agents, errors: { ...errors }, checked: new Set(checked) });
    };
    const invalidate = (cause: unknown) => {
      cache.invalidate(); batch = undefined; discoveryEpoch++;
      controller?.abort(); source?.close();
      if (disposed) return;
      errors = Object.fromEntries(current.current.filter((runner) => runner.binding).map((runner) => [bindingKey(runner.binding), errorText(cause)]));
      checked.clear(); publish();
      if (!retry) retry = setTimeout(() => { retry = undefined; connect(); }, backoff);
      backoff = Math.min(backoff * 2, 10000);
    };
    const discover = async () => {
      if (!batch || disposed) return;
      const activeBatch = batch, epoch = ++discoveryEpoch;
      controller?.abort(); controller = new AbortController();
      const signal = controller.signal;
      const nextErrors: Record<string, string> = {}, nextChecked = new Set<string>(), nextObserved = new Set<string>();
      let cursor = "";
      const seen = new Set<string>();
      try {
        do {
          const page = await request<DirectoryPage>(`${prefix}/agents?limit=32&cursor=${encodeURIComponent(cursor)}`, { signal });
          if (disposed || epoch !== discoveryEpoch || !cache.valid(activeBatch)) return;
          const issues = new Map((page.issues ?? []).map((issue) => [issue.runner_id, issue.code]));
          for (const availability of page.runners ?? []) {
            const key = bindingKey(availability.runner.binding), issue = issues.get(availability.runner.id);
            if (issue) nextErrors[key] = issueText(issue);
            else if (!availability.online) nextErrors[key] = issueText("OFFLINE");
            else nextChecked.add(key);
          }
          page.items = page.items.filter((item) => current.current.some((runner) => bindingKey(runner.binding) === bindingKey(item.target.binding)));
          for (const item of page.items) nextObserved.add(targetKey(item.target));
          if (!cache.mergePage(activeBatch, page)) throw new Error("会话列表已变化，正在重新同步…");
          cursor = page.next_cursor ?? "";
          if (cursor && seen.has(cursor)) throw new Error("Agent 列表分页未前进，请刷新重试。");
          seen.add(cursor);
        } while (cursor);
        errors = nextErrors; checked = nextChecked; observed = nextObserved;
        publish();
      } catch (cause) {
        if (!disposed && epoch === discoveryEpoch && batch === activeBatch) invalidate(cause);
      }
    };
    const connect = () => {
      if (disposed || !current.current.length) return;
      const activeSource = new EventSource(siteURL(`${prefix}/agents/events`));
      source = activeSource;
      activeSource.onmessage = (message) => {
        if (disposed || source !== activeSource) return;
        try {
          const event = JSON.parse(message.data) as DirectoryEvent;
          if (event.kind === "ready") {
            if (batch || !event.subscription_id) throw new Error("无效的目录订阅确认");
            batch = cache.begin(event.subscription_id); backoff = 500;
            errors = {}; checked.clear(); observed.clear();
            void discover(); return;
          }
          if (!batch || !cache.apply(batch, event) || event.kind === "invalidated") throw new Error("会话列表已变化，正在重新同步…");
          if (event.agent) {
            observed.add(targetKey(event.agent.target));
            const key = bindingKey(event.agent.target.binding);
            if (errors[key] === issueText("OFFLINE")) delete errors[key];
            checked.add(key); publish();
          }
        } catch (cause) { invalidate(cause); }
      };
      activeSource.onerror = () => { if (source === activeSource) invalidate(new Error("目录连接已断开，正在重新同步…")); };
    };
    setDiscovery(emptyDiscovery());
    refreshAction.current = discover;
    connect();
    return () => {
      disposed = true; cache.invalidate(); controller?.abort(); source?.close();
      if (retry) clearTimeout(retry);
      refreshAction.current = async () => {};
    };
  }, [signature, prefix]);

  const add = (runner: Runner, runtime: AgentRuntime) => {
    if (!runner.binding) return;
    void refresh();
    return { runner, runtime, target: targetFor(runner.binding, runtime) };
  };
  return { ...discovery, refresh, add };
}
