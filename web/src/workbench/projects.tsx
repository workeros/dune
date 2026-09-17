import { useEffect, useState, type FormEvent } from "react";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "../components/ui/dialog";
import { bindingKey, errorText, listAll, request, type ProfileRecord, type Runner } from "../lib/api";
import type { Project, ProjectDirectory } from "./model";

export function ProjectEditor({ prefix, project, runners, profiles, onClose, onSaved }: {
  prefix: string; project?: Project; runners: Runner[]; profiles: ProfileRecord[]; onClose: () => void; onSaved: (project?: Project) => Promise<void>;
}) {
  const [name, setName] = useState(project?.name ?? ""), [directories, setDirectories] = useState<ProjectDirectory[]>(project?.directories ?? []);
  const [profileID, setProfileID] = useState(project?.default_profile?.id ?? ""), [error, setError] = useState(""), [busy, setBusy] = useState(false), [deleting, setDeleting] = useState(false);
  const [profileRevision, setProfileRevision] = useState(project?.default_profile?.revision);
  const selectedProfile = profiles.find((profile) => profile.id === profileID);
  const bound = runners.filter((runner) => runner.binding);
  const changeDirectory = (id: string, change: Partial<ProjectDirectory>) => setDirectories((old) => old.map((item) => item.id === id ? { ...item, ...change } : item));
  const save = async (event: FormEvent) => {
    event.preventDefault(); setBusy(true); setError("");
    try {
      const default_profile = profileID && profileRevision ? { id: profileID, revision: profileRevision } : undefined;
      const saved = await request<Project>(`${prefix}/projects${project ? `/${encodeURIComponent(project.id)}` : ""}`, { method: project ? "PUT" : "POST", body: JSON.stringify({ name: name.trim(), directories, default_profile, revision: project?.revision ?? 0 }) });
      await onSaved(saved); onClose();
    } catch (cause) { setError(errorText(cause)); } finally { setBusy(false); }
  };
  const remove = async () => {
    if (!project) return;
    setBusy(true); setError("");
    try { await request(`${prefix}/projects/${encodeURIComponent(project.id)}?revision=${project.revision}`, { method: "DELETE" }); await onSaved(); onClose(); }
    catch (cause) { setError(errorText(cause)); } finally { setBusy(false); }
  };
  return <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}><DialogContent className="project-editor"><DialogTitle className="text-xl font-bold">{project ? "编辑项目" : "新建项目"}</DialogTitle><DialogDescription className="muted my-3">把不同开发环境中的目录放在同一个项目里。</DialogDescription>
    <form onSubmit={(event) => void save(event)} className="grid gap-4">
      <label>项目名称<Input value={name} onChange={(event) => setName(event.target.value)} required maxLength={120} /></label>
      <label>默认 Agent 配置<select value={profileID} onChange={(event) => { setProfileID(event.target.value); setProfileRevision(profiles.find((profile) => profile.id === event.target.value)?.revision); }}><option value="">每次选择</option>{profileID && !selectedProfile && <option value={profileID}>原配置暂不可用</option>}{profiles.map((profile) => <option key={profile.id} value={profile.id}>{profile.name} · {profile.profile.adapter.toUpperCase()}</option>)}</select></label>
      {profileID && <p className="muted text-sm">固定使用修订 {profileRevision}{selectedProfile && selectedProfile.revision !== profileRevision && <Button type="button" size="sm" variant="ghost" onClick={() => setProfileRevision(selectedProfile.revision)}>改用当前修订 {selectedProfile.revision}</Button>}</p>}
      <fieldset className="grid gap-3"><legend className="mb-2 text-sm font-semibold">项目目录</legend>{directories.map((directory, index) => <div className="project-directory" key={directory.id}>
        <label>开发环境 {index + 1}<select aria-label={`目录 ${index + 1} 的开发环境`} value={bindingKey(directory.binding)} onChange={(event) => { const runner = bound.find((runner) => bindingKey(runner.binding) === event.target.value); if (runner?.binding) changeDirectory(directory.id, { binding: runner.binding }); }}>
          {!bound.some((runner) => bindingKey(runner.binding) === bindingKey(directory.binding)) && <option value={bindingKey(directory.binding)}>原环境绑定已失效</option>}{bound.map((runner) => <option key={runner.id} value={bindingKey(runner.binding)}>{runner.name}{runner.online ? "" : " · 离线"}</option>)}</select></label>
        <label>目录路径<Input aria-label={`目录 ${index + 1} 的路径`} value={directory.path} onChange={(event) => changeDirectory(directory.id, { path: event.target.value })} placeholder="/workspace/project" required /></label>
        <Button type="button" variant="ghost" size="sm" onClick={() => setDirectories((old) => old.filter((item) => item.id !== directory.id))}>移除目录 {index + 1}</Button>
      </div>)}<Button type="button" variant="outline" disabled={!bound.length || directories.length >= 32} onClick={() => setDirectories((old) => [...old, { id: crypto.randomUUID(), binding: bound[0].binding!, path: "" }])}>添加目录</Button>{!bound.length && <p className="muted">接入开发环境后，可以添加目录。</p>}</fieldset>
      {error && <p className="error-box" role="alert">{error}</p>}
      <div className="flex flex-wrap gap-2"><Button type="submit" disabled={busy}>{busy ? "保存中…" : "保存项目"}</Button><Button type="button" variant="ghost" disabled={busy} onClick={onClose}>取消</Button>{project && <Button type="button" variant="ghost" disabled={busy} onClick={() => setDeleting(true)}>删除项目</Button>}</div>
      {deleting && <div className="error-box"><p className="mb-3">删除项目记录？Agent 和开发机目录会保留。</p><Button type="button" variant="destructive" disabled={busy} onClick={() => void remove()}>确认删除项目</Button></div>}
    </form>
  </DialogContent></Dialog>;
}

export function useProjects(prefix: string) {
  const [projects, setProjects] = useState<Project[]>([]), [error, setError] = useState("");
  const refresh = async () => {
    try {
      const next = await listAll<Project>(`${prefix}/projects`);
      setProjects(next); setError("");
    } catch (cause) { setError(errorText(cause)); }
  };
  useEffect(() => { void refresh(); }, [prefix]);
  return { projects, error, refresh };
}
