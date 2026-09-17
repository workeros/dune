import type { AgentRuntime, Binding, Runner, Runtime } from "../lib/api";
import type { AgentSession } from "./launch";

export type AgentTarget = { binding: Binding; runtime: Pick<Runtime, "id" | "incarnation" | "generation" | "adapter"> };
export type ProjectDirectory = { id: string; binding: Binding; path: string; repository?: string };
export type Project = { id: string; revision: number; name: string; directories: ProjectDirectory[]; default_profile?: { id: string; revision: number } };
export type Pane = { target: AgentTarget; project_id?: string; directory_id?: string; session_record_id?: string };
export type Leaf = { id: string; pane: Pane };
export type Split = { id: string; direction: "horizontal" | "vertical"; ratio: number; children: [LayoutNode, LayoutNode] };
export type LayoutNode = Leaf | Split;
export type ViewSpec = { root: LayoutNode | null; focus_pane?: string; review_pane?: string };
export type SavedView = ViewSpec & { id: string; revision: number };
export type Agent = { ref?: string; target: AgentTarget; runtime: AgentRuntime; runner: Runner; session?: AgentSession };
export type ReadMarker = { target: AgentTarget; epoch: string; sequence: number };
export const emptyView: ViewSpec = { root: null };

export function targetKey(target: AgentTarget): string {
  const b = target.binding, r = target.runtime;
  return JSON.stringify([b.runner_id, b.fabric_id, b.machine_id, b.revision, r.id, r.incarnation, r.generation]);
}
export function targetFor(binding: Binding, runtime: AgentTarget["runtime"]): AgentTarget {
  return { binding, runtime: { id: runtime.id, incarnation: runtime.incarnation, generation: runtime.generation, adapter: runtime.adapter } };
}
export function leaves(root: LayoutNode | null): Leaf[] {
  if (!root) return [];
  return "pane" in root ? [root] : root.children.flatMap(leaves);
}
export function mapNode(root: LayoutNode, id: string, change: (node: LayoutNode) => LayoutNode): LayoutNode {
  if (root.id === id) return change(root);
  if ("pane" in root) return root;
  return { ...root, children: root.children.map((child) => mapNode(child, id, change)) as Split["children"] };
}
export function addPane(view: ViewSpec, pane: Pane, direction: Split["direction"], id: () => string): ViewSpec {
  const existing = leaves(view.root).find((leaf) => targetKey(leaf.pane.target) === targetKey(pane.target));
  if (existing) return { ...view, focus_pane: existing.id };
  if (leaves(view.root).length >= 32) throw new Error("最多同时打开 32 个 pane，请先移出部分会话。");
  const next: Leaf = { id: id(), pane };
  if (!view.root) return { root: next, focus_pane: next.id };
  const anchor = leaves(view.root).find((leaf) => leaf.id === view.focus_pane)?.id ?? leaves(view.root)[0].id;
  const root = mapNode(view.root, anchor, (node) => ({ id: id(), direction, ratio: 0.5, children: [node, next] }));
  if (layoutDepth(root) > 16) throw new Error("此处分屏层数已达上限，请选择另一个 pane。");
  return { ...view, root, focus_pane: next.id };
}
function layoutDepth(node: LayoutNode): number { return "pane" in node ? 1 : 1 + Math.max(...node.children.map(layoutDepth)); }
export function removePane(view: ViewSpec, id: string): ViewSpec {
  const remove = (node: LayoutNode): LayoutNode | null => {
    if (node.id === id) return null;
    if ("pane" in node) return node;
    const children = node.children.map(remove).filter((value): value is LayoutNode => value !== null);
    return children.length === 1 ? children[0] : { ...node, children: children as Split["children"] };
  };
  const root = view.root ? remove(view.root) : null;
  return { root, focus_pane: view.focus_pane === id ? leaves(root)[0]?.id : view.focus_pane, review_pane: view.review_pane === id ? undefined : view.review_pane };
}
export type Rect = { x: number; y: number; width: number; height: number };
export function geometry(root: LayoutNode | null) {
  const panes: Array<{ leaf: Leaf; rect: Rect }> = [], dividers: Array<{ split: Split; rect: Rect }> = [];
  function visit(node: LayoutNode, rect: Rect) {
    if ("pane" in node) { panes.push({ leaf: node, rect }); return; }
    dividers.push({ split: node, rect });
    const horizontal = node.direction === "horizontal", size = horizontal ? rect.width : rect.height;
    visit(node.children[0], { ...rect, [horizontal ? "width" : "height"]: size * node.ratio });
    visit(node.children[1], { ...rect, [horizontal ? "x" : "y"]: (horizontal ? rect.x : rect.y) + size * node.ratio, [horizontal ? "width" : "height"]: size * (1 - node.ratio) });
  }
  if (root) visit(root, { x: 0, y: 0, width: 100, height: 100 });
  return { panes, dividers };
}
export function projectFor(agent: Agent, projects: Project[]) {
  if (agent.session?.project_id) {
    const project = projects.find((item) => item.id === agent.session!.project_id);
    if (project) return { project, directory: project.directories.find((item) => item.id === agent.session!.directory_id) };
  }
  return projects.flatMap((project) => project.directories.map((directory) => ({ project, directory }))).find(({ directory }) => {
    const a = agent.target.binding, b = directory.binding;
    return a.runner_id === b.runner_id && a.machine_id === b.machine_id && a.fabric_id === b.fabric_id && a.revision === b.revision && directory.path === agent.runtime.working_directory;
  });
}
export function activityLabel(runtime: AgentRuntime): string {
  if (runtime.state !== "running") return runtime.stop_reason === "timed_out" ? "已超时" : "已退出";
  return ({ working: "执行中", idle: "空闲", blocked: "等待回应", unknown: "状态未知" } as const)[runtime.activity?.state ?? "unknown"];
}

// Update only the recovery record attached to each exact Runtime. Keeping
// unchanged nodes stable preserves the terminal/ACP connection in each pane.
export function syncSessionRecords(view: ViewSpec, agents: Agent[]): ViewSpec {
  const sessions = new Map(agents.filter((agent) => agent.session?.selected).map((agent) => [targetKey(agent.target), agent.session!.id]));
  const visit = (node: LayoutNode): LayoutNode => {
    if ("pane" in node) {
      const id = sessions.get(targetKey(node.pane.target));
      return id && id !== node.pane.session_record_id ? { ...node, pane: { ...node.pane, session_record_id: id } } : node;
    }
    const children = node.children.map(visit) as Split["children"];
    return children.every((child, index) => child === node.children[index]) ? node : { ...node, children };
  };
  const root = view.root ? visit(view.root) : null;
  return root === view.root ? view : { ...view, root };
}

export function restorePane(view: ViewSpec, id: string, agent: Agent, session: AgentSession): ViewSpec {
  const original = leaves(view.root).find((leaf) => leaf.id === id);
  if (!original || original.pane.session_record_id !== session.id) return view;
  const existing = leaves(view.root).find((leaf) => leaf.id !== id && targetKey(leaf.pane.target) === targetKey(agent.target));
  if (existing) {
    const removed = removePane(view, id);
    return { ...removed, focus_pane: view.focus_pane === id ? existing.id : removed.focus_pane, review_pane: view.review_pane === id ? existing.id : removed.review_pane };
  }
  return { ...view, root: mapNode(view.root!, id, () => ({ ...original, pane: { target: agent.target, session_record_id: session.id, project_id: session.project_id, directory_id: session.directory_id } })) };
}
