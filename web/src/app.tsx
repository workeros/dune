import { lazy, Suspense, useEffect, useRef, useState, type FormEvent } from "react";
import { Icon } from "@iconify/react";
import serverIcon from "@iconify-icons/ri/server-line";
import terminalIcon from "@iconify-icons/ri/terminal-box-line";
import addIcon from "@iconify-icons/ri/add-line";
import logoutIcon from "@iconify-icons/ri/logout-box-r-line";
import refreshIcon from "@iconify-icons/ri/refresh-line";
import codeIcon from "@iconify-icons/ri/code-s-slash-line";
import settingsIcon from "@iconify-icons/ri/settings-3-line";
import folderIcon from "@iconify-icons/ri/folder-open-line";
import playIcon from "@iconify-icons/ri/play-line";
import stopIcon from "@iconify-icons/ri/stop-line";
import deleteIcon from "@iconify-icons/ri/delete-bin-line";
import disconnectIcon from "@iconify-icons/ri/link-unlink-m";
import { Button } from "@/components/ui/button";
import { Input, Textarea } from "@/components/ui/input";
import { Dialog, DialogContent, DialogTitle, DialogDescription } from "@/components/ui/dialog";
const ACPPane = lazy(() => import("@/components/acp").then((module) => ({ default: module.ACPPane })));
const TerminalPane = lazy(() => import("@/components/terminal").then((module) => ({ default: module.TerminalPane })));
import { ProfileManager } from "@/components/profiles";
import { Brand } from "@/components/brand";
import { DesertAtmosphere } from "@/components/desert-atmosphere";
import { APIError, request, post, call, errorText, type Page, type User, runnerPath, bindingKey, runtimeKey, type Binding, type Runner, type BoundRunner, type Runtime, type ProfileRecord, type Profile, type StartupInfo, type ManagedField, type ManagedTemplate, type ManagedUnavailableReason, type ManagedOperation, type ManagedCreation } from "@/lib/api";

export function App() {
  const [user, setUser] = useState<User | null>(), [runners, setRunners] = useState<Runner[]>([]), [selected, setSelected] = useState<Runner>(), [adding, setAdding] = useState(false), [provisioning, setProvisioning] = useState(false), [error, setError] = useState(new URL(window.location.href).searchParams.get("login_error") === "1" ? "企业登录未完成或已过期，请重新登录。" : "");
  const [pageCursor, setPageCursor] = useState(""), [nextCursor, setNextCursor] = useState(""), [previousCursors, setPreviousCursors] = useState<string[]>([]);
  const [profilesOpen, setProfilesOpen] = useState(false), [profileVersion, setProfileVersion] = useState(0);
  const discoveryEpoch = useRef(0);
  const [discoveryLoading, setDiscoveryLoading] = useState(true);
  const [startup, setStartup] = useState<StartupInfo>(), [startupError, setStartupError] = useState("");
  const loadStartup = async () => {
    setStartupError("");
    try { setStartup(await request<StartupInfo>("/api/v1/bootstrap")); }
    catch (e) { setStartupError(errorText(e)); }
  };
  useEffect(() => { void loadStartup(); }, []);
  useEffect(() => { request<User>("/api/v1/me").then((authenticated) => { setUser(authenticated); }).catch((e) => { setUser(null); if (!(e instanceof APIError && e.status === 401)) setError(errorText(e)); }); }, []);
  const refresh = async () => {
    const epoch = ++discoveryEpoch.current;
    try {
      const page = await request<Page<Runner>>(`/api/v1/runners?cursor=${encodeURIComponent(pageCursor)}`);
      if (epoch !== discoveryEpoch.current) return;
      setRunners(page.items); setNextCursor(page.next_cursor ?? "");
      setError("");
    } catch (e) { if (epoch !== discoveryEpoch.current) return; setRunners([]); setNextCursor(""); if (e instanceof APIError && e.status === 401) setUser(null); else setError(e instanceof APIError && e.status === 400 && pageCursor ? "分页已失效，请刷新列表回到第一页。" : errorText(e)); }
    finally { if (epoch === discoveryEpoch.current) setDiscoveryLoading(false); }
  };
  const resetPage = () => { setDiscoveryLoading(true); setPreviousCursors([]); if (pageCursor) { setRunners([]); setNextCursor(""); setSelected(undefined); setPageCursor(""); } else void refresh(); };
  useEffect(() => { if (!user) return; void refresh(); const timer = setInterval(refresh, 5000); return () => { clearInterval(timer); discoveryEpoch.current++; }; }, [user?.id, pageCursor]);
  if (startupError) return <main className="paper-grid grid min-h-screen place-items-center p-6"><DesertAtmosphere /><section className="paper-card grid max-w-md gap-4 p-8"><Brand /><h1 className="text-xl font-bold">暂时无法打开工作台</h1><p className="error-box" role="alert">{startupError}</p><Button onClick={() => void loadStartup()}>重试</Button></section></main>;
  if (!startup || user === undefined) return <main className="grid min-h-screen place-items-center paper-grid"><DesertAtmosphere /><span role="status">正在打开工作台…</span></main>;
  if (!user) return <Auth startup={startup} onLogin={(user) => { discoveryEpoch.current++; setDiscoveryLoading(true); setRunners([]); setSelected(undefined); setPageCursor(""); setNextCursor(""); setPreviousCursors([]); setUser(user); setError(""); }} initialError={error} />;
  const current = runners.find((r) => r.id === selected?.id);
  const matchingBinding = selected && current && bindingKey(selected.binding) === bindingKey(current.binding);
  const workspace = matchingBinding && selected.binding ? { ...current, binding: selected.binding } : undefined;
  const renderWorkbench = () => {
    if (workspace) return <Workspace key={bindingKey(workspace.binding)} runner={workspace} profileVersion={profileVersion} onManageProfiles={() => setProfilesOpen(true)} onRefresh={refresh} onRevoke={async () => { setSelected(undefined); await refresh(); }} />;
    if (discoveryLoading && !runners.length) return <div className="m-auto p-8 muted" role="status">正在加载环境…</div>;
    if (error) return <div className="m-auto p-8"><p className="muted mb-4">暂时无法获取可访问的环境。</p><Button variant="outline" onClick={resetPage}>重新加载列表</Button></div>;
    if (selected && current && !matchingBinding) return <div className="m-auto max-w-lg p-8"><h1 className="mb-3 text-xl font-bold">环境绑定已变化</h1><p className="muted mb-5 leading-7">原工作区已关闭。确认进入 {current.name} 的当前环境后，才能继续操作。</p><Button onClick={() => setSelected(current)}>进入当前环境</Button></div>;
    if (selected && !current) return <div className="m-auto max-w-lg p-8"><h1 className="mb-3 text-xl font-bold">该环境暂不可访问</h1><p className="muted leading-7">请刷新列表或选择其他环境。</p></div>;
    if (selected && current && !current.binding) return current.kind === "managed" ? <ManagedPending runner={current} onRefresh={refresh} /> : <AttachedPending runner={current} onRefresh={refresh} />;
    if (runners.length > 0) return <section className="workspace-prompt paper-card m-auto p-7"><div className="mb-5 inline-flex rounded-xl border border-foreground bg-yellow/40 p-3"><Icon icon={serverIcon} width={25} /></div><h1 className="mb-3 text-2xl font-bold">选择一个开发环境</h1><p className="muted leading-7">从左侧列表选择环境，查看或继续其中的工作。</p></section>;
    if ((pageCursor || nextCursor)) return <div className="m-auto max-w-lg p-8"><h1 className="mb-3 text-xl font-bold">本页暂无可访问的环境</h1><p className="muted leading-7">可以继续翻页，或刷新列表回到第一页。</p></div>;
    return <div className="m-auto max-w-lg p-8"><div className="mb-6 inline-flex rounded-xl border border-foreground bg-mint p-4"><Icon icon={serverIcon} width={30} /></div><h1 className="mb-3 text-3xl font-bold tracking-tight">准备一个开发环境。</h1><p className="muted mb-7 leading-7">接入已有 Linux 或 macOS 开发机，或按站点模板创建托管环境。代码和会话历史留在开发环境中。</p><div className="flex flex-wrap gap-3">{startup.attached && <Button onClick={() => setAdding(true)}><Icon icon={addIcon} />接入开发机</Button>}{startup.managed && <Button variant="outline" onClick={() => setProvisioning(true)}><Icon icon={addIcon} />创建托管环境</Button>}{!startup.attached && !startup.managed && <p className="muted">此站点未开放环境接入。</p>}</div></div>;
  };
  return <div className="workspace">
    <aside className="sidebar"><Brand /><div className="machine-section"><div className="mb-3 flex items-center justify-between"><span className="muted font-semibold">开发环境</span><Button variant="ghost" size="icon" aria-label="刷新环境列表" onClick={resetPage}><Icon icon={refreshIcon} /></Button></div><nav className="machine-nav" aria-label="开发环境">{runners.map((m) => <button key={m.id} className={`machine-row ${selected?.id === m.id ? "active" : ""}`} onClick={() => setSelected(m)}><Icon icon={serverIcon} width={21} /><span className="min-w-0 flex-1"><span className="block truncate text-sm font-semibold">{m.name}</span><span className="mt-1 block text-xs text-muted-foreground"><i className={`status-dot ${m.online ? "online" : ""}`} />{!m.binding ? m.kind === "managed" ? "准备中" : "未绑定" : m.online ? "在线" : "离线"}{m.os && <> · {m.os === "darwin" ? "macOS" : m.os}</>}</span></span></button>)}</nav>{(previousCursors.length > 0 || nextCursor) && <nav className="mt-3 flex flex-wrap gap-2" aria-label="环境分页"><Button variant="outline" size="sm" disabled={!previousCursors.length} onClick={() => { setDiscoveryLoading(true); setRunners([]); setNextCursor(""); setSelected(undefined); setPageCursor(previousCursors.at(-1) ?? ""); setPreviousCursors((items) => items.slice(0, -1)); }}>上一页</Button><Button variant="outline" size="sm" disabled={!nextCursor} onClick={() => { setDiscoveryLoading(true); setRunners([]); setNextCursor(""); setSelected(undefined); setPreviousCursors((items) => [...items, pageCursor]); setPageCursor(nextCursor); }}>下一页</Button></nav>}{startup.attached && <Button className="mt-2 w-full" variant="outline" onClick={() => setAdding(true)}><Icon icon={addIcon} />接入开发机</Button>}{startup.managed && <Button className="mt-2 w-full" variant="outline" onClick={() => setProvisioning(true)}><Icon icon={addIcon} />创建托管环境</Button>}{!startup.tenant_scoped && <Button className="mt-2 w-full" variant="outline" onClick={() => setProfilesOpen(true)}><Icon icon={settingsIcon} />管理 Profile</Button>}</div><div className="sidebar-footer mt-auto border-t border-foreground/20 pt-4"><p className="mb-4 text-xs leading-relaxed text-muted-foreground">任务在开发环境中运行。<br />关闭网页，任务继续。</p><div className="flex items-center gap-2"><span className="min-w-0 flex-1 truncate text-xs" title={user.email}>{user.email || user.id}</span><Button variant="ghost" size="icon" aria-label="退出登录" onClick={() => void post("/api/v1/auth/logout", {}).then(() => setUser(null)).catch((e) => setError(errorText(e)))}><Icon icon={logoutIcon} /></Button></div></div></aside>
    <main className="workbench paper-grid"><DesertAtmosphere />{error && <div className="error-box m-4" role="alert">{error}</div>}<Suspense fallback={<p className="m-auto" role="status">正在加载工作区…</p>}>{renderWorkbench()}</Suspense></main>
    {!startup.tenant_scoped && <ProfileManager key={user.id} open={profilesOpen} onOpenChange={setProfilesOpen} onSaved={() => setProfileVersion((version) => version + 1)} />}
    {startup.attached && <AddMachine open={adding} onOpenChange={setAdding} onRefresh={refresh} />}
    {startup.managed && <AddManaged ownerID={user.id} open={provisioning} onOpenChange={setProvisioning} onCreated={async (runner) => { setSelected(runner); resetPage(); }} />}
  </div>;
}

function Auth({ startup, onLogin, initialError }: { startup: StartupInfo; onLogin: (user: User) => void; initialError: string }) {
  const passwordLogin = startup.login_methods.find((method) => method.kind === "password");
  const externalLogin = startup.login_methods.find((method) => method.kind === "external" || method.kind === "enterprise");
  const [register, setRegister] = useState(false), [email, setEmail] = useState(""), [password, setPassword] = useState(""), [error, setError] = useState(initialError), [busy, setBusy] = useState(false);
  const submit = async (event: FormEvent) => { event.preventDefault(); setBusy(true); setError(""); try { onLogin(await post<User>(register ? "/api/v1/auth/register" : passwordLogin!.url, { email, password })); } catch (e) { setError(errorText(e)); } finally { setBusy(false); } };
  return <main className="paper-grid flex min-h-screen items-center justify-center p-6"><DesertAtmosphere /><div className="w-full max-w-md"><div className="mb-8"><Brand /></div><section className="paper-card p-8"><p className="mb-2 text-xs font-semibold uppercase tracking-[.15em] text-muted-foreground">Your machine. Your workspace.</p><h1 className="mb-2 text-2xl font-bold">{register ? "开始使用 Dune" : "回到你的工作台"}</h1><p className="muted mb-7">在浏览器中，继续开发机上的工作。</p>{passwordLogin ? <form onSubmit={submit} className="grid gap-5"><label>邮箱<Input type="email" autoComplete="username" required value={email} onChange={(e) => setEmail(e.target.value)} /></label><label>密码<Input type="password" autoComplete={register ? "new-password" : "current-password"} minLength={register ? 12 : undefined} maxLength={256} required value={password} onChange={(e) => setPassword(e.target.value)} />{register && <span className="text-xs font-normal text-muted-foreground">至少 12 个字符</span>}</label>{error && <div className="error-box" role="alert">{error}</div>}<Button type="submit" disabled={busy}>{busy ? "请稍候…" : register ? "注册并进入工作台" : "登录"}</Button></form> : externalLogin ? <div className="grid gap-4">{error && <div className="error-box" role="alert">{error}</div>}<Button onClick={() => { window.location.assign(externalLogin.url); }}>使用企业账号登录</Button></div> : <p role="status">此站点暂未提供可用的登录方式。</p>}{passwordLogin && startup.local_registration && <button className="mt-6 text-sm underline decoration-foreground/30 underline-offset-4" onClick={() => { setRegister(!register); setError(""); }}>{register ? "已有账号？登录" : "第一次使用？自由注册"}</button>}{passwordLogin && !startup.local_registration && <p className="muted mt-6 text-sm">此站点未开放本地注册，请使用已有账号登录。</p>}</section><p className="mt-6 text-center text-xs text-muted-foreground">无需预装 Agent，即可接入开发机。</p></div></main>;
}

function managedRequestBody(template: ManagedTemplate, name: string, values: Record<string, string | boolean>, requestKey: string): string {
  const parameters = template.fields.flatMap((field) => {
    const value = values[field.name];
    if (field.type === "integer") {
      const raw = typeof value === "string" ? value.trim() : "";
      if (!raw && !field.required) return [];
      if (!/^-?(0|[1-9]\d*)$/.test(raw)) throw new Error(`${field.label} 必须是十进制整数。`);
      const integer = BigInt(raw), minimum = BigInt(field.minimum!), maximum = BigInt(field.maximum!);
      if (integer < minimum || integer > maximum) throw new Error(`${field.label} 必须在 ${field.minimum} 到 ${field.maximum} 之间。`);
      return [`${JSON.stringify(field.name)}:${raw}`];
    }
    if (field.type === "boolean") return [`${JSON.stringify(field.name)}:${value === true}`];
    const text = typeof value === "string" ? value : "";
    if (!text && !field.required) return [];
    if (!text.trim() && field.required) throw new Error(`请填写${field.label}。`);
    if (new TextEncoder().encode(text).length > (field.max_length ?? 0)) throw new Error(`${field.label}过长。`);
    return [`${JSON.stringify(field.name)}:${JSON.stringify(text)}`];
  });
  return `{"request_key":${JSON.stringify(requestKey)},"request":{"name":${JSON.stringify(name)},"fabric_id":${JSON.stringify(template.fabric_id)},"template_id":${JSON.stringify(template.id)},"template_version":${JSON.stringify(template.version)},"parameters":{${parameters.join(",")}}}}`;
}

function managedRequestKey(): string {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return `web-${Array.from(bytes, (value) => value.toString(16).padStart(2, "0")).join("")}`;
}

const managedUnavailableReason: Record<ManagedUnavailableReason, string> = {
  maintenance: "提供方维护中", capacity: "提供方容量不足", configuration: "提供方配置未就绪",
  unreachable: "暂时无法连接提供方", unknown: "提供方状态未知",
};

function managedTemplateLabel(template: ManagedTemplate): string {
  const reason = template.unavailable_reason ? managedUnavailableReason[template.unavailable_reason] : managedUnavailableReason.unknown;
  return `${template.name} · ${template.version}${template.available ? "" : ` · ${reason}`}`;
}

function AddManaged({ ownerID, open, onOpenChange, onCreated }: { ownerID: string; open: boolean; onOpenChange: (open: boolean) => void; onCreated: (runner: Runner) => Promise<void> }) {
  const [templates, setTemplates] = useState<ManagedTemplate[]>([]), [templateKey, setTemplateKey] = useState(""), [name, setName] = useState(""), [values, setValues] = useState<Record<string, string | boolean>>({}), [error, setError] = useState(""), [busy, setBusy] = useState(false), [loading, setLoading] = useState(false);
  const requestKey = useRef(managedRequestKey());
  const selected = templates.find((template) => JSON.stringify([template.fabric_id, template.id, template.version]) === templateKey);
  useEffect(() => {
    if (!open) return;
    let alive = true;
    setLoading(true); setError("");
    request<{ items: ManagedTemplate[] }>(`/api/v1/managed/tenants/${encodeURIComponent(ownerID)}/templates`).then(({ items }) => {
      if (!alive) return;
      const initial = items.find((template) => template.available) ?? items[0];
      setTemplates(items); setTemplateKey(initial ? JSON.stringify([initial.fabric_id, initial.id, initial.version]) : "");
    }).catch((e) => { if (alive) setError(errorText(e)); }).finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [open, ownerID]);
  useEffect(() => { setValues({}); }, [templateKey]);
  const close = (value: boolean) => {
    onOpenChange(value);
    if (!value) { setName(""); setValues({}); setError(""); requestKey.current = managedRequestKey(); }
  };
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!selected?.available) return;
    setBusy(true); setError("");
    try {
      const body = managedRequestBody(selected, name, values, requestKey.current);
      const created = await request<ManagedCreation>(`/api/v1/managed/tenants/${encodeURIComponent(ownerID)}/runners`, { method: "POST", body });
      close(false); await onCreated(created.runner);
    } catch (e) { setError(errorText(e)); } finally { setBusy(false); }
  };
  return <Dialog open={open} onOpenChange={close}><DialogContent><DialogTitle className="mb-2 text-xl font-bold">创建托管环境</DialogTitle><DialogDescription className="muted mb-6">选择当前账号可用的模板。提交后可以离开页面，创建会在后台继续。</DialogDescription>{loading ? <p role="status">正在读取可用模板…</p> : templates.length ? <form className="grid gap-4" onSubmit={submit}><label>环境名称<Input value={name} onChange={(e) => setName(e.target.value)} required maxLength={120} placeholder="例如：项目开发环境" disabled={!selected?.available} /></label><label>模板<select value={templateKey} onChange={(e) => setTemplateKey(e.target.value)}>{templates.map((template) => { const key = JSON.stringify([template.fabric_id, template.id, template.version]); return <option key={key} value={key} disabled={!template.available}>{managedTemplateLabel(template)}</option>; })}</select></label>{selected && !selected.available && <div className="error-box" role="status">{selected.unavailable_reason ? managedUnavailableReason[selected.unavailable_reason] : managedUnavailableReason.unknown}，当前不能创建新环境。</div>}{selected?.fields.map((field) => <ManagedInput key={field.name} field={field} value={values[field.name]} disabled={!selected.available} onChange={(value) => setValues((current) => ({ ...current, [field.name]: value }))} />)}{error && <div className="error-box" role="alert">{error}</div>}<Button type="submit" disabled={busy || !selected?.available}>{busy ? "提交中…" : "创建环境"}</Button></form> : !error && <p className="muted">当前账号没有可用的托管模板。</p>}{error && !templates.length && <div className="error-box" role="alert">{error}</div>}</DialogContent></Dialog>;
}

function ManagedInput({ field, value, disabled, onChange }: { field: ManagedField; value: string | boolean | undefined; disabled?: boolean; onChange: (value: string | boolean) => void }) {
  if (field.type === "boolean") return <label className="flex items-center gap-3"><input type="checkbox" checked={value === true} disabled={disabled} onChange={(e) => onChange(e.target.checked)} /><span>{field.label}{field.required ? " *" : ""}</span></label>;
  if (field.choices?.length) return <label>{field.label}<select required={field.required} disabled={disabled} value={typeof value === "string" ? value : ""} onChange={(e) => onChange(e.target.value)}><option value="">请选择</option>{field.choices.map((choice) => <option key={choice} value={choice}>{choice}</option>)}</select></label>;
  return <label>{field.label}<Input type="text" inputMode={field.type === "integer" ? "numeric" : "text"} required={field.required} disabled={disabled} maxLength={field.type === "string" ? field.max_length : undefined} value={typeof value === "string" ? value : ""} onChange={(e) => onChange(e.target.value)} />{field.type === "integer" && <span className="text-xs font-normal text-muted-foreground">范围：{field.minimum} 至 {field.maximum}</span>}</label>;
}

const managedStage: Record<string, string> = { queued: "等待后台处理", creating: "正在创建资源", bootstrapping: "正在配置开发环境", cleaning_up: "能力不满足，正在清理资源", waiting_connection: "等待开发机连接", renewing: "正在续期", pausing: "正在暂停环境", resuming: "正在恢复环境", closing_access: "正在关闭访问", destroying: "正在删除资源", succeeded: "操作已完成", failed: "操作失败" };
const managedRenewalReason: Record<string, string> = { RENEW: "已安排自动续期", NOT_DUE: "尚未到续期时间", FACTS_UNKNOWN: "等待提供方事实", EXPIRY_REQUIRES_RECONCILIATION: "到期状态需要重新确认", MUTATION_PENDING: "等待现有变更完成", FIRST_CONNECTION_TIMEOUT: "首次连接宽限期已结束，自动续期已停止", BOOTSTRAP_FAILED: "环境配置失败，自动续期已停止", DESTROYING: "资源正在删除，自动续期已停止", RESOURCE_GONE: "资源已删除，自动续期已停止", RESOURCE_UNKNOWN: "尚未确认资源，自动续期已停止", DISABLED: "自动续期已关闭", POLICY_ERROR: "续期策略暂时不可用，将自动重试", POLICY_INVALID: "续期策略返回了无效决定，将自动重试" };

function ManagedPending({ runner, onRefresh }: { runner: Runner; onRefresh: () => Promise<void> }) {
  const [status, setStatus] = useState<ManagedOperation>(), [error, setError] = useState(""), [destroying, setDestroying] = useState(false), [confirming, setConfirming] = useState(false);
  const destroyKey = useRef(managedRequestKey());
  const load = async () => {
    try { const next = await request<ManagedOperation>(`/api/v1/managed/runners/${encodeURIComponent(runner.id)}`); setStatus(next); setError(""); if (next.finished && next.outcome === "succeeded") await onRefresh(); }
    catch (e) { setError(errorText(e)); }
  };
  useEffect(() => { let alive = true; const refresh = async () => { if (alive) await load(); }; void refresh(); const timer = setInterval(() => void refresh(), 2500); return () => { alive = false; clearInterval(timer); }; }, [runner.id]);
  const destroy = async () => {
    setDestroying(true); setError("");
    try { await request(`/api/v1/managed/runners/${encodeURIComponent(runner.id)}`, { method: "DELETE", body: JSON.stringify({ request_key: destroyKey.current }) }); setConfirming(false); await onRefresh(); }
    catch (e) { setError(errorText(e)); } finally { setDestroying(false); }
  };
  const detail = status?.action === "destroy" ? status.finished && status.outcome === "succeeded" ? "供应商资源已确认删除，访问保持关闭。" : status.finished ? "自动删除已停止，访问保持关闭。" : "正在撤销访问并确认删除供应商资源。" : status?.outcome === "failed" ? "创建已停止。可以删除这个环境后重新创建。" : "创建会在后台继续，可以关闭此页面。";
  return <div className="m-auto w-full max-w-xl p-8"><section className="paper-card p-7"><div className="mb-5 inline-flex rounded-xl border border-foreground bg-yellow p-3"><Icon icon={serverIcon} width={28} /></div><h1 className="mb-2 text-2xl font-bold">{runner.name}</h1><p className="mb-2 font-semibold" role="status">{status ? managedStage[status.stage ?? ""] ?? status.stage : "正在读取生命周期状态…"}</p><p className="muted leading-7">{detail}</p>{status?.credential_state === "degraded" && <div className="error-box mt-4" role="status">Git 凭据刷新失败；环境仍在运行，后台会继续重试。</div>}{status?.resource_ref && <p className="mt-4 break-all text-xs text-muted-foreground">资源：{status.resource_ref}</p>}{status?.expires_at && <p className="mt-1 text-xs text-muted-foreground">有效至：{new Date(status.expires_at).toLocaleString()}</p>}{status?.renewal_reason && <p className="mt-1 text-xs text-muted-foreground">续期策略：{managedRenewalReason[status.renewal_reason] ?? status.renewal_reason}</p>}{status?.renewal_until && <p className="mt-1 text-xs text-muted-foreground">计划续期至：{new Date(status.renewal_until).toLocaleString()}</p>}{status?.renewal_next_check_at && <p className="mt-1 text-xs text-muted-foreground">下次检查：{new Date(status.renewal_next_check_at).toLocaleString()}</p>}{status?.access_close_outcome && <p className="mt-1 text-xs text-muted-foreground">访问关闭：{status.access_close_outcome === "revoked" ? "门禁已撤销，现有连接最多 5 秒内关闭" : "正在核对访问状态"}</p>}{error && <div className="error-box mt-4" role="alert">{error}</div>}<div className="mt-6 flex gap-3"><Button variant="outline" onClick={() => void load()}><Icon icon={refreshIcon} />刷新状态</Button>{status?.action !== "destroy" && <Button variant="ghost" onClick={() => setConfirming(true)}><Icon icon={deleteIcon} />删除环境</Button>}</div></section><Dialog open={confirming} onOpenChange={setConfirming}><DialogContent><DialogTitle className="mb-3 text-xl font-bold">删除 {runner.name}？</DialogTitle><DialogDescription className="muted mb-6">撤销访问后，现有连接最多在 5 秒内停止转发；后台继续删除供应商资源。</DialogDescription><Button variant="destructive" disabled={destroying} onClick={() => void destroy()}>{destroying ? "提交中…" : "确认删除"}</Button></DialogContent></Dialog></div>;
}

function AttachedPending({ runner, onRefresh }: { runner: Runner; onRefresh: () => Promise<void> }) {
  const [current, setCurrent] = useState<Runner | null>(runner), [message, setMessage] = useState(""), [busy, setBusy] = useState(false);
  const inspect = async () => {
    try {
      const next = await request<Runner>(`/api/v1/runners/${encodeURIComponent(runner.id)}`);
      setCurrent(next);
      setMessage(next.binding ? "绑定已提交。配置完整时使用 repair；没有有效凭据时，进入此环境解绑后重新生成命令。" : "仍在等待接入。提交前失败可重试原命令；结果未知时请先取消此接入。");
      return next;
    } catch (e) {
      if (e instanceof APIError && e.status === 404) { setCurrent(null); setMessage("此接入已失效，可以生成新命令。"); return null; }
      throw e;
    }
  };
  const cancel = async () => {
    setBusy(true);
    try { await request(`/api/v1/enrollments/${encodeURIComponent(runner.id)}`, { method: "DELETE" }); setCurrent(null); setMessage("接入已取消。"); await onRefresh(); }
    catch (e) { setMessage(`取消结果需核对：${errorText(e)}`); try { await inspect(); } catch (checkError) { setMessage(`状态核对失败：${errorText(checkError)}。请稍后刷新，不要自动重试。`); } }
    finally { setBusy(false); }
  };
  return <section className="m-auto max-w-lg space-y-3 p-5"><h2 className="font-semibold">{runner.name} · 接入状态</h2><p className="break-all font-mono text-xs">Runner ID: {runner.id}</p><p className="text-sm" role="status">{message || "本地配置完整时可使用 repair 补完安装；没有有效凭据且结果未知时，先核对并撤销这个接入。"}</p><div className="flex gap-2"><Button size="sm" variant="outline" disabled={busy} onClick={() => { setBusy(true); void inspect().catch((e) => setMessage(errorText(e))).finally(() => setBusy(false)); }}>核对状态</Button>{current && !current.binding && <Button size="sm" variant="ghost" disabled={busy} onClick={() => void cancel()}>取消待接入</Button>}{!current && <Button size="sm" onClick={() => void onRefresh()}>刷新环境列表</Button>}</div></section>;
}

function AddMachine({ open, onOpenChange, onRefresh }: { open: boolean; onOpenChange: (open: boolean) => void; onRefresh: () => Promise<void> }) {
  const commandRef = useRef<HTMLTextAreaElement>(null);
  const [name, setName] = useState(""), [result, setResult] = useState<{ runner: Runner; command: string; expires_at: number }>(), [error, setError] = useState(""), [busy, setBusy] = useState(false), [copied, setCopied] = useState(false);
  const submit = async (event: FormEvent) => { event.preventDefault(); setBusy(true); setError(""); try { setResult(await post("/api/v1/enrollments", { name })); } catch (e) { setError(errorText(e)); } finally { setBusy(false); } };
  return <Dialog open={open} onOpenChange={(value) => { onOpenChange(value); if (!value) { setResult(undefined); setCopied(false); } }}><DialogContent><DialogTitle className="mb-2 text-xl font-bold">接入开发机</DialogTitle><DialogDescription className="muted mb-6">支持 Linux 和 macOS。机器主动连接 Gateway，不需要开放入站端口。</DialogDescription><form onSubmit={submit} className="grid gap-4"><label>机器名称<Input placeholder="例如：日常 Linux 开发机" value={name} onChange={(e) => setName(e.target.value)} required maxLength={120} /></label><Button disabled={busy || !!result} type="submit">{busy ? "生成中…" : "生成一次性绑定命令"}</Button></form>{result && <div className="mt-5 grid gap-3"><p className="text-sm">通过 SSH 登录开发机，执行安装与绑定命令：</p><Textarea ref={commandRef} aria-label="安装与绑定命令" readOnly rows={5} value={result.command} className="break-all bg-secondary font-mono text-xs" onFocus={(event) => event.currentTarget.select()} /><p className="text-xs text-muted-foreground">有效至 {new Date(result.expires_at * 1000).toLocaleTimeString()}，仅可使用一次。可选中命令后使用系统复制快捷键。</p><Button variant="outline" onClick={() => { if (navigator.clipboard) void navigator.clipboard.writeText(result.command).then(() => setCopied(true)).catch((e) => setError(errorText(e))); else { commandRef.current?.focus(); commandRef.current?.select(); } }}>{copied ? "已复制" : navigator.clipboard ? "复制命令" : "选择命令"}</Button></div>}{result && <AttachedPending runner={result.runner} onRefresh={async () => { setResult(undefined); await onRefresh(); }} />}{error && <div className="error-box mt-4" role="alert">{error}</div>}</DialogContent></Dialog>;
}

function Workspace({ runner, onRefresh, onRevoke, profileVersion, onManageProfiles }: { runner: BoundRunner; profileVersion: number; onManageProfiles: () => void; onRefresh: () => Promise<void>; onRevoke: () => Promise<void> }) {
  const [sessions, setSessions] = useState<Runtime[]>([]), [selected, setSelected] = useState<Runtime>(), [configs, setConfigs] = useState<ProfileRecord[]>([]), [configID, setConfigID] = useState(""), [cwd, setCwd] = useState(""), [error, setError] = useState(""), [busy, setBusy] = useState(false), [browsing, setBrowsing] = useState(false), [tab, setTab] = useState<"terminal" | "diff">("terminal"), [revoking, setRevoking] = useState(false), [managedStatus, setManagedStatus] = useState<ManagedOperation>(), [stateBusy, setStateBusy] = useState(false);
  const alive = useRef(true);
  const destroyKey = useRef(managedRequestKey());
  const stateKey = useRef(managedRequestKey());
  const loadSessions = async () => { const list = await call<Runtime[]>(runner.binding, "runtime.list"); if (alive.current) { setSessions(list); } };
  useEffect(() => {
    let current = true;
    request<Page<ProfileRecord>>("/api/v1/profiles").then((page) => { if (current) setConfigs(page.items.filter((record) => record.profile.kind === "agent")); }).catch((e) => { if (current) setError(errorText(e)); });
    return () => { current = false; };
  }, [profileVersion]);
  useEffect(() => { alive.current = true; if (!runner.online) return () => { alive.current = false; }; void Promise.all([call<{ home: string }>(runner.binding, "machine.info").then((info) => { if (alive.current) setCwd((old) => old || info.home); }), loadSessions()]).catch((e) => { if (alive.current) setError(errorText(e)); }); const timer = setInterval(() => void loadSessions().catch((e) => { if (alive.current) setError(errorText(e)); }), 4000); return () => { alive.current = false; clearInterval(timer); }; }, [runner.online]);
  const loadManagedStatus = async () => { if (runner.kind !== "managed") return; const status = await request<ManagedOperation>(`/api/v1/managed/runners/${encodeURIComponent(runner.id)}`); if (alive.current) setManagedStatus(status); };
  useEffect(() => { if (runner.kind !== "managed") return; void loadManagedStatus().catch((e) => setError(errorText(e))); const timer = setInterval(() => void loadManagedStatus().catch((e) => setError(errorText(e))), 2500); return () => clearInterval(timer); }, [runner.id]);
  const currentRuntime = sessions.find((r) => r.id === selected?.id);
  const selectedRuntime = selected && currentRuntime && runtimeKey(selected) === runtimeKey(currentRuntime) ? currentRuntime : undefined;
  const start = async (terminal = false) => {
    setBusy(true); setError("");
    try {
      const config = configs.find((c) => c.id === configID);
      if (!terminal && !config) throw new Error("请先选择或添加 Agent 配置。");
      const profile: Profile = terminal ? { version: 1, kind: "agent", working_directory: cwd, adapter: "pty", setup: { steps: [] }, start: { argv: ["/bin/sh"] } } : { ...config!.profile, working_directory: cwd };
      const runtime = await post<Runtime>(runnerPath(runner.binding, "sessions"), profile);
      await loadSessions(); setSelected(runtime); setTab("terminal");
    } catch (e) { setError(errorText(e)); } finally { setBusy(false); }
  };
  const sessionAction = async (operation: string) => { if (!selectedRuntime) return; setError(""); try { await call(runner.binding, operation, {}, selectedRuntime); await loadSessions(); if (operation === "runtime.forget") setSelected(undefined); } catch (e) { setError(errorText(e)); } };
  const changeManagedState = async (action: "pause" | "resume") => { setStateBusy(true); setError(""); try { await post(`/api/v1/managed/runners/${encodeURIComponent(runner.id)}/${action}`, { request_key: stateKey.current }); stateKey.current = managedRequestKey(); await Promise.all([loadManagedStatus(), onRefresh()]); } catch (e) { setError(errorText(e)); } finally { setStateBusy(false); } };
  return <><header className="work-header"><div><h1 className="flex items-center gap-2 text-lg font-bold"><Icon icon={serverIcon} />{runner.name}</h1><p className="mt-1 text-xs text-muted-foreground"><i className={`status-dot ${runner.online ? "online" : ""}`} />{managedStatus?.access_suspended ? managedStage[managedStatus.stage ?? ""] ?? "访问已暂停" : runner.online ? "已连接开发机" : "开发机离线，历史暂不可访问"} · {runner.os}/{runner.arch}</p></div><div className="flex gap-2">{runner.kind === "managed" && managedStatus?.capabilities?.pause_resume && !managedStatus.access_suspended && managedStatus.resource_state === "ready" && <Button variant="outline" disabled={stateBusy} onClick={() => void changeManagedState("pause")}>{stateBusy ? "提交中…" : "暂停环境"}</Button>}<Button variant="outline" onClick={onManageProfiles}><Icon icon={settingsIcon} />Agent 配置</Button><Button variant="ghost" size="icon" aria-label="解绑开发机" onClick={() => setRevoking(true)}><Icon icon={deleteIcon} /></Button></div></header>
	    {error && <div className="error-box mx-6 mt-4" role="alert">{error}</div>}
	    {managedStatus?.credential_state === "degraded" && <div className="error-box mx-6 mt-4" role="status">Git 凭据刷新失败；环境仍在运行，后台会继续重试。</div>}
	    {managedStatus?.access_suspended ? <div className="m-auto w-full max-w-lg p-8"><section className="paper-card p-7 text-center"><h2 className="mb-3 text-xl font-bold">{managedStatus.resource_state === "paused" ? "环境已暂停" : managedStage[managedStatus.stage ?? ""] ?? "正在确认环境状态"}</h2><p className="muted mb-6 leading-7">终端与 Agent 进程、tmux 会话和持久化磁盘会保留。恢复完成并重新连接后，访问会自动开放。</p>{managedStatus.resource_state === "paused" && (managedStatus.finished || managedStatus.action !== "resume") && <Button disabled={stateBusy} onClick={() => void changeManagedState("resume")}>{stateBusy ? "提交中…" : "恢复环境"}</Button>}</section></div> : <>
	    <div className="flex flex-wrap gap-3 px-6 pt-4"><div className="flex min-w-60 flex-1 items-center gap-2"><Icon icon={folderIcon} /><Input aria-label="项目工作目录" className="font-mono" value={cwd} onChange={(e) => setCwd(e.target.value)} placeholder="开发机上的绝对路径" /><Button variant="outline" size="icon" aria-label="浏览项目目录" disabled={!runner.online} onClick={() => setBrowsing(true)}><Icon icon={folderIcon} /></Button></div><select aria-label="Agent 启动配置" value={configID} onChange={(e) => { setConfigID(e.target.value); const selected = configs.find((record) => record.id === e.target.value); if (selected) setCwd(selected.profile.working_directory); }}><option value="">选择 Agent</option>{configs.map((c) => <option key={c.id} value={c.id}>{c.name} · {c.profile.adapter.toUpperCase()}</option>)}</select><Button disabled={!runner.online || busy || !cwd} onClick={() => void start()}><Icon icon={playIcon} />启动 Agent</Button><Button variant="outline" disabled={!runner.online || busy || !cwd} onClick={() => void start(true)}><Icon icon={terminalIcon} />普通终端</Button></div>
	    <div className="session-layout"><nav className="session-list" aria-label="会话列表"><p className="muted mb-2 text-xs font-semibold">会话 · {sessions.length}</p>{sessions.map((runtime) => <button className={`session-row ${selected && runtimeKey(runtime) === runtimeKey(selected) ? "active" : ""}`} key={runtimeKey(runtime)} aria-label={`连接会话 ${runtime.title || runtime.adapter} ${runtime.id.slice(0, 6)}`} aria-current={selected && runtimeKey(runtime) === runtimeKey(selected) ? "true" : undefined} onClick={() => { setSelected(runtime); setTab("terminal"); }}><span className="block truncate text-sm font-semibold">{runtime.title || runtime.adapter}</span><span className="mt-1 block text-xs text-muted-foreground">{runtime.state === "running" ? "运行中" : runtime.stop_reason === "timed_out" ? "已超时" : `已退出 · ${runtime.exit_code}`} · {runtime.id.slice(0, 6)}</span>{(!selected || runtimeKey(runtime) !== runtimeKey(selected)) && <span className="mt-1 block text-xs text-muted-foreground">点击连接</span>}</button>)}{!sessions.length && <p className="text-xs leading-5 text-muted-foreground">从上方打开终端，或启动一个 Agent。</p>}</nav><section className="paper-card flex min-h-0 min-w-0 flex-col overflow-hidden">{selectedRuntime ? <><div className="flex flex-wrap items-center justify-between gap-2 border-b border-foreground/20 p-2"><div className="flex gap-1"><Button variant={tab === "terminal" ? "outline" : "ghost"} size="sm" onClick={() => setTab("terminal")}><Icon icon={terminalIcon} />{selectedRuntime.adapter === "acp" ? "对话" : "终端"}</Button><Button variant={tab === "diff" ? "outline" : "ghost"} size="sm" onClick={() => setTab("diff")}><Icon icon={codeIcon} />Git diff</Button></div><span className="min-w-0 flex-1 truncate px-2 font-mono text-xs text-muted-foreground" title={selectedRuntime.working_directory}>{selectedRuntime.working_directory}</span><Button variant="ghost" size="sm" onClick={() => setSelected(undefined)}><Icon icon={disconnectIcon} />断开连接</Button>{selectedRuntime.state === "running" ? <Button variant="ghost" size="sm" onClick={() => void sessionAction("runtime.stop")}><Icon icon={stopIcon} />{selectedRuntime.adapter === "pty" ? "结束并清除历史" : "停止"}</Button> : <Button variant="ghost" size="sm" onClick={() => void sessionAction("runtime.forget")}><Icon icon={deleteIcon} />删除会话与历史</Button>}</div>{tab === "terminal" ? selectedRuntime.adapter === "pty" ? <TerminalPane key={runtimeKey(selectedRuntime)} binding={runner.binding} runtime={selectedRuntime} /> : <ACPPane key={runtimeKey(selectedRuntime)} binding={runner.binding} runtime={selectedRuntime} /> : <Diff binding={runner.binding} cwd={selectedRuntime.working_directory || cwd} />}</> : <div className="m-auto p-8 text-center"><Icon icon={terminalIcon} width={36} className="mx-auto mb-4" /><h2 className="mb-2 text-lg font-bold">{selected ? "会话执行身份已变化或不可用" : sessions.length ? "选择一个会话再连接" : "准备开始工作"}</h2><p className="muted max-w-sm leading-6">{selected ? "原连接已关闭，请从会话列表重新选择。未确认的输入或操作不会重发。" : sessions.length ? "现有会话继续在开发机上运行。点击左侧会话才会连接，进入或刷新页面不会自动接入。" : "选择项目目录，打开普通终端。也可以保存自己的 Agent 命令，再从这里启动。"}</p></div>}</section></div>
	    </>}

    <DirectoryPicker binding={runner.binding} path={cwd} open={browsing} onOpenChange={setBrowsing} onSelect={setCwd} />
    <Dialog open={revoking} onOpenChange={setRevoking}><DialogContent><DialogTitle className="mb-3 text-xl font-bold">{runner.kind === "managed" ? "删除" : "解绑"} {runner.name}？</DialogTitle><DialogDescription className="muted mb-6">{runner.kind === "managed" ? "撤销访问后，现有连接最多在 5 秒内停止转发；后台继续删除供应商资源。" : "当前网页连接会被撤销。不会删除开发机文件，也不会终止远端 Agent 或终端进程。"}</DialogDescription><Button variant="destructive" onClick={() => { const action = runner.kind === "managed" ? request(`/api/v1/managed/runners/${encodeURIComponent(runner.id)}`, { method: "DELETE", body: JSON.stringify({ request_key: destroyKey.current }) }) : request(runnerPath(runner.binding, "binding"), { method: "DELETE" }); void action.then(onRevoke).catch((e) => setError(errorText(e))); }}>确认{runner.kind === "managed" ? "删除" : "解绑"}</Button></DialogContent></Dialog>
  </>;
}

function DirectoryPicker({ binding, path, open, onOpenChange, onSelect }: { binding: Binding; path: string; open: boolean; onOpenChange: (open: boolean) => void; onSelect: (path: string) => void }) {
  type DirectoryEntry = { name: string; is_dir: boolean };
  const [current, setCurrent] = useState(path), [dirs, setDirs] = useState<DirectoryEntry[]>([]), [cursor, setCursor] = useState(""), [busy, setBusy] = useState(false), [error, setError] = useState("");
  const generation = useRef(0);
  useEffect(() => { if (open) setCurrent(path); }, [open]);
  const load = async (after: string, epoch: number) => {
    setBusy(true); setError("");
    try {
      const page = await call<Page<DirectoryEntry>>(binding, "files", { action: "list_page", path: current, cursor: after });
      if (epoch !== generation.current) return;
      setDirs((old) => [...(after ? old : []), ...page.items.filter((entry) => entry.is_dir)]);
      setCursor(page.next_cursor ?? "");
    } catch (e) { if (epoch === generation.current) setError(errorText(e)); }
    finally { if (epoch === generation.current) setBusy(false); }
  };
  useEffect(() => {
    const epoch = ++generation.current;
    setDirs([]); setCursor("");
    if (open && current) void load("", epoch);
    return () => { generation.current++; };
  }, [open, current, binding]);
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent><DialogTitle className="mb-2 text-xl font-bold">选择项目目录</DialogTitle><DialogDescription className="muted mb-4">按名称逐页浏览当前开发机。目录变化后可重新加载。</DialogDescription><p className="mb-3 break-all font-mono text-sm">{current}</p><div className="mb-5 max-h-64 overflow-auto rounded border border-foreground/20"><button className="flex w-full gap-2 border-b border-foreground/10 p-3 text-left text-sm hover:bg-secondary" onClick={() => setCurrent(current.split("/").slice(0, -1).join("/") || "/")}><Icon icon={folderIcon} />.. 上一级</button>{dirs.map((dir) => <button key={dir.name} className="flex w-full gap-2 p-3 text-left text-sm hover:bg-secondary" onClick={() => setCurrent(`${current.replace(/\/$/, "")}/${dir.name}`)}><Icon icon={folderIcon} />{dir.name}</button>)}{cursor && <Button variant="ghost" disabled={busy} onClick={() => void load(cursor, generation.current)}>加载更多</Button>}{busy && <p role="status" className="p-3 text-sm">正在读取目录…</p>}</div>{error && <div className="error-box mb-4" role="alert">{error}</div>}<div className="flex gap-2"><Button variant="outline" disabled={busy} onClick={() => void load("", ++generation.current)}>重新加载</Button><Button disabled={!!error || busy} onClick={() => { onSelect(current); onOpenChange(false); }}>使用此目录</Button></div></DialogContent></Dialog>;
}

function Diff({ binding, cwd }: { binding: Binding; cwd: string }) {
  const [diff, setDiff] = useState(""), [error, setError] = useState(""), [staged, setStaged] = useState(false), [busy, setBusy] = useState(false);
  const load = async () => { setBusy(true); setError(""); try { const result = await call<{ stdout: string; stderr: string; exit_code: number; truncated: boolean }>(binding, "git", { action: "diff", directory: cwd, staged }); if (result.exit_code !== 0) throw new Error(result.stderr || "Git diff 失败"); setDiff(result.stdout); if (result.truncated) setError("diff 超过显示上限，当前仅展示部分内容。"); } catch (e) { setError(errorText(e)); } finally { setBusy(false); } };
  useEffect(() => { void load(); }, [binding, cwd, staged]);
  return <div className="flex min-h-0 flex-1 flex-col"><div className="flex justify-between gap-3 border-b border-foreground/20 p-3"><select aria-label="Diff 范围" value={staged ? "staged" : "working"} onChange={(e) => setStaged(e.target.value === "staged")}><option value="working">工作区修改</option><option value="staged">暂存区修改</option></select><Button variant="ghost" onClick={() => void load()} disabled={busy}><Icon icon={refreshIcon} />刷新</Button></div>{error && <div className="error-box m-3" role="alert">{error}</div>}<pre className="min-h-0 flex-1 overflow-auto p-4 text-xs leading-6">{busy ? "读取开发机 diff…" : diff ? diff.split("\n").map((line, i) => <div key={i} className={line.startsWith("+") ? "bg-mint/40 text-[#285437]" : line.startsWith("-") ? "bg-primary/20 text-destructive" : ""}>{line || " "}</div>) : !error && "当前范围没有 Git diff。未跟踪的新文件不会包含在 git diff 中。"}</pre></div>;
}
