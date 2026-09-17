import { lazy, Suspense, useEffect, useState } from "react";
import { Button } from "../components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "../components/ui/dialog";
import { bindingKey, call, errorText, listAll, type ProfileRecord, type Runner } from "../lib/api";
import { activityLabel, addPane, leaves, mapNode, projectFor, removePane, restorePane, syncSessionRecords, targetKey, type Agent, type Leaf, type Project, type Split } from "./model";
import { useView } from "./use-view";
import { useAgents } from "./use-agents";
import { useReadMarkers } from "./use-read-markers";
import { ProjectEditor, useProjects } from "./projects";
import { SplitCanvas } from "./splits";
import { SessionRecovery } from "./recovery";
import { StartAgent } from "./start-agent";
import { Diff } from "./diff";
import { Files } from "./files";
import "./workbench.css";

const ACPPane = lazy(() => import("../components/acp").then((module) => ({ default: module.ACPPane })));
const TerminalPane = lazy(() => import("../components/terminal").then((module) => ({ default: module.TerminalPane })));

export function ParallelWorkbench({ runners, runnersLoading, selectedRunner, onSelectRunner, profileVersion, onManageProfiles }: {
  runners: Runner[]; runnersLoading: boolean; selectedRunner?: Runner; onSelectRunner: (runner: Runner) => void; profileVersion: number; onManageProfiles: () => void;
}) {
  const prefix = "/api/v1", saved = useView(prefix), directory = useAgents(runners), projects = useProjects(prefix);
  useEffect(() => { if (saved.loaded) saved.change((view) => syncSessionRecords(view, directory.agents)); }, [saved.loaded, saved.change, directory.agents]);
  const [projectID, setProjectID] = useState(""), [editing, setEditing] = useState<Project | "new">(), [profiles, setProfiles] = useState<ProfileRecord[]>([]);
  const [direction, setDirection] = useState<Split["direction"]>("horizontal"), [error, setError] = useState(""), [review, setReview] = useState<"files" | "git" | "">("");
  const [stopping, setStopping] = useState<Agent>(), [stopBusy, setStopBusy] = useState(false);
  useEffect(() => { let alive = true; void listAll<ProfileRecord>(`${prefix}/profiles`).then((items) => { if (alive) setProfiles(items.filter((item) => item.profile.kind === "agent")); }).catch((cause) => { if (alive) setError(errorText(cause)); }); return () => { alive = false; }; }, [profileVersion]);
  const panes = leaves(saved.view.root), project = projects.projects.find((item) => item.id === projectID);
  const agentFor = (leaf: Leaf | undefined) => leaf && directory.agents.find((agent) => targetKey(agent.target) === targetKey(leaf.pane.target));
  const focused = agentFor(panes.find((pane) => pane.id === saved.view.focus_pane));
  const read = useReadMarkers(prefix, directory.agents, focused);
  const reviewLeaf = panes.find((pane) => pane.id === (saved.view.review_pane || saved.view.focus_pane)), reviewAgent = agentFor(reviewLeaf);
  const open = (agent: Agent) => {
    if (!saved.loaded) return;
    const association = projectFor(agent, projects.projects);
    try { saved.change((current) => addPane(current, { target: agent.target, project_id: association?.project.id, directory_id: association?.directory?.id, session_record_id: agent.session?.id }, direction, () => crypto.randomUUID())); setError(""); }
    catch (cause) { setError(errorText(cause)); }
  };
  const focus = (id: string) => saved.change((old) => old.focus_pane === id ? old : { ...old, focus_pane: id });
  const stop = async () => {
    if (!stopping) return;
    setStopBusy(true); setError("");
    try { await call(stopping.target.binding, stopping.runtime.state === "running" ? "runtime.stop" : "runtime.forget", {}, stopping.runtime); setStopping(undefined); await directory.refresh(); }
    catch (cause) { setError(errorText(cause)); } finally { setStopBusy(false); }
  };
  const paneBody = (leaf: Leaf) => {
    const agent = agentFor(leaf), runner = runners.find((item) => item.id === leaf.pane.target.binding.runner_id);
    const bindingValid = runner && bindingKey(runner.binding) === bindingKey(leaf.pane.target.binding);
    const projectName = projects.projects.find((item) => item.id === leaf.pane.project_id)?.name ?? "未归类";
    const key = bindingKey(leaf.pane.target.binding), discoveryError = directory.errors[key];
    const available = bindingValid && runner.online && agent;
    let unavailable = "正在核验会话…";
    if (!runnersLoading && !runner) unavailable = "原开发环境暂不可访问";
    else if (runner && !bindingValid) unavailable = "原环境绑定已失效，请从列表选择当前会话。";
    else if (runner && !runner.online) unavailable = "开发环境离线，等待重新连接。";
    else if (discoveryError) unavailable = `暂时无法核验会话：${discoveryError}`;
    else if (directory.checked.has(key) && !agent) unavailable = leaf.pane.session_record_id ? "原运行会话已结束，可以检查原生恢复记录。" : "原会话已失效，当前还没有可用的恢复索引。可以新建 Agent。";
    return <div className="agent-pane-card"><header>
      <button className="pane-title" onClick={() => focus(leaf.id)} title={`${projectName} · ${runner?.name ?? leaf.pane.target.binding.runner_id}`}><strong>{agent?.runtime.title ?? leaf.pane.target.runtime.adapter.toUpperCase()}</strong><small>{projectName} · {runner?.name ?? "原环境"}</small></button>
      <span className="pane-status">{agent ? activityLabel(agent.runtime) : "待核验"} · {leaf.pane.target.runtime.adapter.toUpperCase()}</span>
      <Button size="sm" variant="ghost" aria-label={`固定审阅 ${agent?.runtime.title ?? leaf.id}`} onClick={() => { saved.change((old) => ({ ...old, review_pane: old.review_pane === leaf.id ? undefined : leaf.id })); setReview((old) => old || "git"); }}>{saved.view.review_pane === leaf.id ? "取消固定" : "固定审阅"}</Button>
      {available && <Button size="sm" variant="ghost" aria-label={`${agent.runtime.state === "running" ? "停止" : "删除"} ${agent.runtime.title ?? leaf.id}`} onClick={() => setStopping(agent)}>{agent.runtime.state === "running" ? "停止" : "删除"}</Button>}
      <Button size="sm" variant="ghost" aria-label={`移出 ${agent?.runtime.title ?? leaf.id}`} title="移出视图，Agent 继续运行" onClick={() => saved.change((old) => removePane(old, leaf.id))}>移出</Button>
    </header>{leaf.pane.session_record_id && agent?.runtime.state !== "running" && <SessionRecovery key={leaf.pane.session_record_id} prefix={prefix} recordID={leaf.pane.session_record_id} current={directory.agents.find((item) => item.session?.id === leaf.pane.session_record_id && item.session?.selected)} enabled={true} ready={!!bindingValid && !!runner?.online && directory.checked.has(key)} onRefresh={directory.refresh} onRestored={(runtime, session) => { if (!runner) return; const restored = directory.add(runner, runtime, session); if (restored) saved.change((view) => restorePane(view, leaf.id, restored, session)); }} />}<div className="pane-body"><Suspense fallback={<p className="muted p-4" role="status">正在打开会话…</p>}>{available ? agent.runtime.adapter === "pty" ? <TerminalPane binding={leaf.pane.target.binding} runtime={agent.runtime} focused={saved.view.focus_pane === leaf.id} /> : <ACPPane binding={leaf.pane.target.binding} runtime={agent.runtime} /> : <div className="pane-unavailable" role="status">{unavailable}</div>}</Suspense></div></div>;
  };
  const visibleAgents = directory.agents.filter((agent) => !projectID || projectFor(agent, projects.projects)?.project.id === projectID);
  return <div className="parallel-workbench">
    <header className="parallel-header"><div><h1>并行工作台</h1><p className="muted" role="status">{saved.error ? "布局未保存" : !saved.loaded ? "读取个人布局…" : saved.saving ? "保存布局中…" : "布局已保存"}</p></div><div className="flex flex-wrap gap-2"><label className="split-choice">新会话打开方向<select value={direction} onChange={(event) => setDirection(event.target.value as Split["direction"])}><option value="horizontal">左右分屏</option><option value="vertical">上下分屏</option></select></label><Button size="sm" variant={review === "files" ? "outline" : "ghost"} onClick={() => setReview((old) => old === "files" ? "" : "files")}>文件</Button><Button size="sm" variant={review === "git" ? "outline" : "ghost"} onClick={() => setReview((old) => old === "git" ? "" : "git")}>Git diff</Button></div></header>
    {saved.error && <div className="error-box mx-3" role="alert">{saved.error}<Button variant="outline" size="sm" onClick={() => void saved.reload()}>加载已保存布局</Button></div>}
    {(error || projects.error || read.error) && <p className="error-box mx-3" role="alert">{error || projects.error || read.error}</p>}
    <StartAgent runners={runners} selected={selectedRunner} onSelect={onSelectRunner} profiles={profiles} project={project} onManageProfiles={onManageProfiles} onStarted={(runner, runtime, session) => { const agent = directory.add(runner, runtime, session); if (agent) open(agent); }} />
    <div className="parallel-body"><aside className="agent-navigation">
      <div className="nav-heading"><h2>项目</h2><Button size="sm" variant="ghost" onClick={() => setEditing("new")}>新建项目</Button></div>
      <nav aria-label="项目列表"><button className={!projectID ? "selected" : ""} onClick={() => setProjectID("")}>全部项目</button>{projects.projects.map((item) => <div className="project-nav-row" key={item.id}><button className={projectID === item.id ? "selected" : ""} title={item.name} onClick={() => setProjectID(item.id)}>{item.name}</button><Button size="sm" variant="ghost" aria-label={`编辑项目 ${item.name}`} onClick={() => setEditing(item)}>编辑</Button></div>)}</nav>
      <div className="nav-heading"><h2>Agent · {visibleAgents.length}</h2><Button size="sm" variant="ghost" onClick={() => { void directory.refresh(); void projects.refresh(); }}>刷新</Button></div>
      {Object.entries(directory.errors).map(([key, message]) => <p className="error-box" key={key}>{runners.find((runner) => bindingKey(runner.binding) === key)?.name}：{message}</p>)}
      <nav aria-label="Agent 列表">{visibleAgents.map((agent) => {
        const opened = panes.find((pane) => targetKey(pane.pane.target) === targetKey(agent.target)), association = projectFor(agent, projects.projects);
        return <button key={targetKey(agent.target)} className={`agent-nav-row ${opened?.id === saved.view.focus_pane ? "selected" : ""}`} disabled={!saved.loaded} onClick={() => open(agent)} aria-label={`打开 ${agent.runtime.title || agent.runtime.adapter} · ${agent.runner.name}`}>
          <strong>{agent.runtime.title || agent.runtime.adapter}{read.unread(agent) && <span className="unread-mark" aria-label="未读活动">●</span>}</strong><span>{activityLabel(agent.runtime)} · {agent.runtime.adapter.toUpperCase()}{opened ? " · 已打开" : ""}</span><small>{association?.project.name ?? "未归类"} · {agent.runner.name}</small>
        </button>;
      })}</nav>{!visibleAgents.length && <p className="muted p-2">{runnersLoading ? "正在读取环境…" : "从上方启动 Agent，或选择其他项目。"}</p>}
    </aside><div className="parallel-stage">{saved.view.root ? <SplitCanvas root={saved.view.root} focus={saved.view.focus_pane} onFocus={focus} onRatio={(id, ratio) => saved.change((old) => ({ ...old, root: old.root ? mapNode(old.root, id, (node) => "children" in node ? { ...node, ratio } : node) : null }))} render={paneBody} /> : <div className="pane-empty"><h2>把正在做的工作放在一起</h2><p>选择左侧 Agent 打开分屏。可以组合不同项目和开发环境，移出视图后 Agent 继续运行。</p></div>}</div>
    {review && <aside className="workbench-review" aria-label="文件与 Git 审阅"><header><strong>{saved.view.review_pane ? "已固定审阅" : "跟随焦点"}</strong><span>{reviewAgent?.runner.name ?? "选择一个会话"}</span><Button size="sm" variant="ghost" onClick={() => { saved.change((old) => ({ ...old, review_pane: undefined })); }}>跟随焦点</Button></header>{reviewAgent?.runtime.working_directory && reviewLeaf ? <div className="review-content" key={`${targetKey(reviewLeaf.pane.target)}:${reviewAgent.runtime.working_directory}:${review}`}>{review === "git" ? <Diff binding={reviewLeaf.pane.target.binding} cwd={reviewAgent.runtime.working_directory} /> : <Files binding={reviewLeaf.pane.target.binding} cwd={reviewAgent.runtime.working_directory} />}</div> : <p className="muted p-4">会话可访问时，显示它所在目录的内容。</p>}</aside>}
    </div>
    {editing && <ProjectEditor prefix={prefix} project={editing === "new" ? undefined : editing} runners={runners} profiles={profiles} onClose={() => setEditing(undefined)} onSaved={async (project) => { await projects.refresh(); setProjectID(project?.id ?? ""); }} />}
    <Dialog open={!!stopping} onOpenChange={(open) => { if (!open && !stopBusy) setStopping(undefined); }}><DialogContent><DialogTitle>{stopping?.runtime.state === "running" ? "停止" : "删除"} {stopping?.runtime.title || "会话"}？</DialogTitle><DialogDescription className="muted my-4">{stopping?.runtime.adapter === "pty" ? "终端会话和保留的终端历史会被清除，开发机文件会保留。" : stopping?.runtime.state === "running" ? "Agent 进程会结束，开发机文件会保留。" : "退出的会话记录会被删除，开发机文件会保留。"}</DialogDescription><Button variant="destructive" disabled={stopBusy} onClick={() => void stop()}>{stopBusy ? "提交中…" : stopping?.runtime.state === "running" ? "确认停止" : "确认删除"}</Button></DialogContent></Dialog>
  </div>;
}
