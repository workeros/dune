import { useEffect, useState } from "react";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { APIError, bindingKey, call, errorText, post, runnerPath, type AgentRuntime, type ProfileRecord, type Runner } from "../lib/api";
import { DirectoryPicker } from "./directory-picker";
import type { Project } from "./model";
import type { LaunchRequest, LaunchResult } from "./launch";

export function StartAgent({ runners, selected, onSelect, profiles, project, onStarted, onManageProfiles }: {
  runners: Runner[]; selected?: Runner; onSelect: (runner: Runner) => void; profiles: ProfileRecord[]; project?: Project;
  onStarted: (runner: Runner, runtime: AgentRuntime) => void; onManageProfiles: () => void;
}) {
  const [profileID, setProfileID] = useState(""), [cwd, setCwd] = useState(""), [directoryID, setDirectoryID] = useState("");
  const [busy, setBusy] = useState(false), [error, setError] = useState(""), [browsing, setBrowsing] = useState(false);
  const [location, setLocation] = useState("current"), [worktreePath, setWorktreePath] = useState(""), [branch, setBranch] = useState(""), [ref, setRef] = useState("");
  const runner = selected ?? runners.find((item) => item.binding && item.online);
  const current = runners.find((item) => item.id === runner?.id);
  const directory = project?.directories.find((item) => item.id === directoryID);
  const directoryMatches = !directory || bindingKey(directory.binding) === bindingKey(runner?.binding);
  const valid = !!runner?.binding && !!current?.online && bindingKey(current.binding) === bindingKey(runner.binding) && directoryMatches;
  useEffect(() => {
    const directory = project?.directories[0];
    setDirectoryID(directory?.id ?? "");
    setProfileID(project?.default_profile?.id ?? "");
    if (directory) {
      setCwd(directory.path);
      const matching = runners.find((item) => bindingKey(item.binding) === bindingKey(directory.binding));
      if (matching) onSelect(matching);
    }
  }, [project?.id]);
  useEffect(() => {
    if (!runner?.binding || !valid || cwd) return;
    let alive = true;
    void call<{ home: string }>(runner.binding, "machine.info").then((info) => { if (alive) setCwd((old) => old || info.home); }).catch((cause) => { if (alive) setError(errorText(cause)); });
    return () => { alive = false; };
  }, [bindingKey(runner?.binding), valid, cwd]);
  const selectDirectory = (id: string) => {
    const directory = project?.directories.find((item) => item.id === id);
    setDirectoryID(id);
    if (!directory) return;
    setCwd(directory.path);
    const matching = runners.find((item) => bindingKey(item.binding) === bindingKey(directory.binding));
    if (matching) { onSelect(matching); setError(""); }
    else setError("这个项目目录的原环境绑定已失效，请编辑项目并重新选择目录。");
  };
  const start = async (terminal: boolean) => {
    if (!runner?.binding || !valid || busy) return;
    setBusy(true); setError("");
    const accept = (result: LaunchResult | undefined) => {
      if (result?.worktree) { setCwd(result.worktree.path); setDirectoryID(""); setLocation("current"); }
      if (result?.runtime) onStarted(runner, result.runtime);
    };
    try {
      const body: LaunchRequest = {
        working_directory: cwd, project: project ? { id: project.id, revision: project.revision } : undefined,
        directory_id: directoryID || undefined,
        worktree: location === "worktree" ? { path: worktreePath, branch, ref: ref || undefined } : undefined,
      };
      if (terminal) body.custom = { version: 1, kind: "agent", working_directory: cwd, adapter: "pty", setup: { steps: [] }, start: { argv: ["/bin/sh"] } };
      else {
        const configured = profiles.find((item) => item.id === profileID);
        const selection = project?.default_profile?.id === profileID ? project.default_profile : configured ? { id: configured.id, revision: configured.revision } : undefined;
        if (!selection) throw new Error("请选择 Agent 配置。");
        body.profile = selection;
      }
      accept(await post<LaunchResult>(runnerPath(runner.binding, "sessions"), body));
    } catch (cause) {
      const partial = cause instanceof APIError ? cause.result as LaunchResult | undefined : undefined;
      accept(partial);
      setError(`${partial?.worktree ? `已创建 worktree：${partial.worktree.path}。` : ""}${partial?.runtime ? "Agent 已启动，请继续使用已打开的会话。" : ""}${errorText(cause)}`);
    } finally { setBusy(false); }
  };
  const locationReady = location === "current" || !!worktreePath && !!branch;
  return <div className="start-agent">
    <div className="start-agent-fields">
      <label>启动环境<select value={runner?.id ?? ""} onChange={(event) => { const selected = runners.find((item) => item.id === event.target.value); if (selected) { onSelect(selected); setCwd(""); setDirectoryID(""); setError(""); } }}><option value="" disabled>选择开发环境</option>{runners.map((item) => <option key={item.id} value={item.id} disabled={!item.binding || !item.online}>{item.name}{item.online ? "" : " · 离线"}</option>)}</select></label>
      {!!project?.directories.length && <label>项目目录<select value={directoryID} onChange={(event) => selectDirectory(event.target.value)}><option value="">自定义目录</option>{project.directories.map((directory) => <option key={directory.id} value={directory.id}>{runners.find((runner) => runner.id === directory.binding.runner_id)?.name ?? "原环境"} · {directory.path}</option>)}</select></label>}
      <label className="start-directory">工作目录<div className="flex gap-1"><Input value={cwd} onChange={(event) => { setCwd(event.target.value); setDirectoryID(""); }} placeholder="开发机上的绝对路径" /><Button variant="outline" aria-label="浏览工作目录" disabled={!valid} onClick={() => setBrowsing(true)}>浏览</Button></div></label>
      <label>Agent 配置<select value={profileID} onChange={(event) => { setProfileID(event.target.value); if (!cwd) setCwd(profiles.find((item) => item.id === event.target.value)?.profile.working_directory ?? ""); }}><option value="">选择配置</option>{profileID && !profiles.some((item) => item.id === profileID) && <option value={profileID}>原默认配置暂不可用</option>}{profiles.map((profile) => <option key={profile.id} value={profile.id}>{profile.name} · {profile.profile.adapter.toUpperCase()}</option>)}</select></label>
      <label>工作位置<select value={location} disabled={busy} onChange={(event) => setLocation(event.target.value)}><option value="current">当前目录</option><option value="worktree">新建 worktree</option></select></label>
      <Button disabled={!valid || !cwd || !profileID || !locationReady || busy} onClick={() => void start(false)}>{busy ? "启动中…" : "启动 Agent"}</Button><Button variant="outline" disabled={!valid || !cwd || !locationReady || busy} onClick={() => void start(true)}>普通终端</Button><Button variant="ghost" onClick={onManageProfiles}>管理配置</Button>
    </div>
    {location === "worktree" && <div className="start-agent-worktree"><label>新 worktree 目录<Input value={worktreePath} disabled={busy} onChange={(event) => setWorktreePath(event.target.value)} placeholder="开发机上尚不存在的绝对路径" /></label><label>新分支<Input value={branch} disabled={busy} onChange={(event) => setBranch(event.target.value)} placeholder="feat/agent-task" /></label><label>起始版本<Input value={ref} disabled={busy} onChange={(event) => setRef(event.target.value)} placeholder="HEAD" /></label><p className="muted">从已提交版本创建，不包含当前目录的未提交改动。</p></div>}
    {selected?.binding && current?.binding && bindingKey(selected.binding) !== bindingKey(current.binding) && <p className="error-box" role="alert">启动环境的绑定已变化。<Button size="sm" variant="outline" onClick={() => onSelect(current)}>选择当前环境</Button></p>}
    {!directoryMatches && <p className="error-box" role="alert">所选项目目录的环境绑定不可用，请重新选择项目目录或编辑项目。</p>}
    {error && <p className="error-box" role="alert">{error}</p>}
    {runner?.binding && <DirectoryPicker binding={runner.binding} path={cwd || "/"} open={browsing} onOpenChange={setBrowsing} onSelect={(path) => { setCwd(path); setDirectoryID(""); }} />}
  </div>;
}
