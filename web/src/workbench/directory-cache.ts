import type { AgentRuntime, Runner } from "../lib/api";
import { targetKey, type AgentTarget } from "./model.ts";

export type DirectoryAgent = { agent_ref?: string; target: AgentTarget; runtime: AgentRuntime; runner?: Runner };
export type DirectoryPage = {
  items: DirectoryAgent[];
  runners: { runner: Pick<Runner, "id" | "binding">; online: boolean; ready: boolean }[];
  issues: { runner_id: string; code: string; runtime?: AgentRuntime }[];
  next_cursor?: string; complete: boolean;
};
export type DirectoryEvent = { subscription_id: string; kind: "ready" | "member" | "metadata" | "invalidated" | "heartbeat"; agent?: DirectoryAgent };
export type DiscoveryBatch = Readonly<{ subscription: string }>;

// A batch is an object capability for one subscription lifetime. Invalidating
// it fences resolved fetches and buffered callbacks, even after reconnect.
export class DirectoryCache {
  private batch?: DiscoveryBatch;
  private members = new Map<string, DirectoryAgent>();
  begin(subscription: string): DiscoveryBatch {
    this.invalidate();
    this.batch = Object.freeze({ subscription });
    return this.batch;
  }
  invalidate() { this.batch = undefined; this.members.clear(); }
  valid(batch: DiscoveryBatch) { return this.batch === batch; }
  mergePage(batch: DiscoveryBatch, page: DirectoryPage): boolean {
    if (!this.valid(batch)) return false;
    for (const agent of page.items) if (!this.merge(agent, true)) return false;
    return true;
  }
  apply(batch: DiscoveryBatch, event: DirectoryEvent): boolean {
    if (!this.valid(batch) || event.subscription_id !== batch.subscription) return false;
    if (event.kind === "invalidated") { this.invalidate(); return true; }
    if (event.kind === "heartbeat") return true;
    return !!event.agent && this.merge(event.agent, event.kind === "member");
  }
  private merge(agent: DirectoryAgent, member: boolean): boolean {
    const { runtime, target } = agent;
    if (!target?.binding?.runner_id || !runtime?.id || runtime.id !== target.runtime.id || runtime.incarnation !== target.runtime.incarnation || runtime.generation !== target.runtime.generation || runtime.adapter !== target.runtime.adapter) return false;
    const key = targetKey(target), previous = this.members.get(key);
    if (!previous && (!member || this.members.size >= 4096)) return false;
    const next = structuredClone(agent), metadata = runtime.session_metadata, old = previous?.runtime.session_metadata;
    if (metadata && !/^(0|[1-9][0-9]*)$/.test(metadata.revision)) return false;
    if (old && metadata && BigInt(metadata.revision) < BigInt(old.revision)) return true;
    if (old && !metadata) {
      next.runtime.session_metadata = structuredClone(old);
      next.agent_ref = previous?.agent_ref;
    }
    this.members.set(key, next);
    return true;
  }
  snapshot(): DirectoryAgent[] { return structuredClone([...this.members.values()]); }
}
