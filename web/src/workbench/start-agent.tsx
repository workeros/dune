import { useEffect, useState } from "react";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { bindingKey, call, errorText, post, request, runnerPath, type AgentRuntime, type Profile, type ProfileRecord, type Runner } from "../lib/api";
import { DirectoryPicker } from "./directory-picker";
import type { Project } from "./model";

export function StartAgent({ runners, selected, onSelect, profiles, project, onStarted, onManageProfiles }: {
  runners: Runner[]; selected?: Runner; onSelect: (runner: Runner) => void; profiles: ProfileRecord[]; project?: Project;
  onStarted: (runner: Runner, runtime: AgentRuntime) => void; onManageProfiles: () => void;
}) {
  const [profileID, setProfileID] = useState(""), [cwd, setCwd] = useState(""), [directoryID, setDirectoryID] = useState("");
  const [busy, setBusy] = useState(false), [error, setError] = useState(""), [browsing, setBrowsing] = useState(false);
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
    try {
      let profile: Profile;
      if (terminal) profile = { version: 1, kind: "agent", working_directory: cwd, adapter: "pty", setup: { steps: [] }, start: { argv: ["/bin/sh"] } };
      else {
        const configured = profiles.find((item) => item.id === profileID);
        const selection = project?.default_profile?.id === profileID ? project.default_profile : configured ? { id: configured.id, revision: configured.revision } : undefined;
        if (!selection) throw new Error("请选择 Agent 配置。");
        const record = await request<ProfileRecord>(`/api/v1/profiles/${encodeURIComponent(selection.id)}?revision=${selection.revision}`);
        profile = { ...record.profile, working_directory: cwd };
      }
      const runtime = await post<AgentRuntime>(runnerPath(runner.binding, "sessions"), profile);
      onStarted(runner, runtime);
    } catch (cause) { setError(errorText(cause)); } finally { setBusy(false); }
  };
  return <div className="start-agent">
    <div className="start-agent-fields">
      <label>启动环境<select value={runner?.id ?? ""} onChange={(event) => { const selected = runners.find((item) => item.id === event.target.value); if (selected) { onSelect(selected); setCwd(""); setDirectoryID(""); setError(""); } }}><option value="" disabled>选择开发环境</option>{runners.map((item) => <option key={item.id} value={item.id} disabled={!item.binding || !item.online}>{item.name}{item.online ? "" : " · 离线"}</option>)}</select></label>
      {!!project?.directories.length && <label>项目目录<select value={directoryID} onChange={(event) => selectDirectory(event.target.value)}><option value="">自定义目录</option>{project.directories.map((directory) => <option key={directory.id} value={directory.id}>{runners.find((runner) => runner.id === directory.binding.runner_id)?.name ?? "原环境"} · {directory.path}</option>)}</select></label>}
      <label className="start-directory">工作目录<div className="flex gap-1"><Input value={cwd} onChange={(event) => { setCwd(event.target.value); setDirectoryID(""); }} placeholder="开发机上的绝对路径" /><Button variant="outline" aria-label="浏览工作目录" disabled={!valid} onClick={() => setBrowsing(true)}>浏览</Button></div></label>
      <label>Agent 配置<select value={profileID} onChange={(event) => { setProfileID(event.target.value); if (!cwd) setCwd(profiles.find((item) => item.id === event.target.value)?.profile.working_directory ?? ""); }}><option value="">选择配置</option>{profileID && !profiles.some((item) => item.id === profileID) && <option value={profileID}>原默认配置暂不可用</option>}{profiles.map((profile) => <option key={profile.id} value={profile.id}>{profile.name} · {profile.profile.adapter.toUpperCase()}</option>)}</select></label>
      <Button disabled={!valid || !cwd || !profileID || busy} onClick={() => void start(false)}>{busy ? "启动中…" : "启动 Agent"}</Button><Button variant="outline" disabled={!valid || !cwd || busy} onClick={() => void start(true)}>普通终端</Button><Button variant="ghost" onClick={onManageProfiles}>管理配置</Button>
    </div>
    {selected?.binding && current?.binding && bindingKey(selected.binding) !== bindingKey(current.binding) && <p className="error-box" role="alert">启动环境的绑定已变化。<Button size="sm" variant="outline" onClick={() => onSelect(current)}>选择当前环境</Button></p>}
    {!directoryMatches && <p className="error-box" role="alert">所选项目目录的环境绑定不可用，请重新选择项目目录或编辑项目。</p>}
    {error && <p className="error-box" role="alert">{error}</p>}
    {runner?.binding && <DirectoryPicker binding={runner.binding} path={cwd || "/"} open={browsing} onOpenChange={setBrowsing} onSelect={(path) => { setCwd(path); setDirectoryID(""); }} />}
  </div>;
}
