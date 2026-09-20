import { useEffect, useRef, useState, type FormEvent } from "react";
import { Icon } from "@iconify/react";
import serverIcon from "@iconify-icons/ri/server-line";
import addIcon from "@iconify-icons/ri/add-line";
import logoutIcon from "@iconify-icons/ri/logout-box-r-line";
import refreshIcon from "@iconify-icons/ri/refresh-line";
import settingsIcon from "@iconify-icons/ri/settings-3-line";
import deleteIcon from "@iconify-icons/ri/delete-bin-line";
import { Button } from "@/components/ui/button";
import { Input, Textarea } from "@/components/ui/input";
import { Dialog, DialogContent, DialogTitle, DialogDescription } from "@/components/ui/dialog";
import { ProfileManager } from "@/components/profiles";
import { ParallelWorkbench } from "@/workbench/page";
import { RunnerDetails } from "@/workbench/runner-details";
import { Brand } from "@/components/brand";
import { DesertAtmosphere } from "@/components/desert-atmosphere";
import { APIError, request, post, listAll, errorText, type Page, type User, bindingKey, type Runner, type StartupInfo, type ManagedField, type ManagedTemplate, type ManagedUnavailableReason, type ManagedOperation, type ManagedCreation } from "@/lib/api";

export function App() {
  const [user, setUser] = useState<User | null>(), [runners, setRunners] = useState<Runner[]>([]), [selected, setSelected] = useState<Runner>(), [adding, setAdding] = useState(false), [provisioning, setProvisioning] = useState(false), [error, setError] = useState(new URL(window.location.href).searchParams.get("login_error") === "1" ? "企业登录未完成或已过期，请重新登录。" : "");
  const [managingRunner, setManagingRunner] = useState(false);
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
      const items = await listAll<Runner>("/api/v1/runners");
      if (epoch !== discoveryEpoch.current) return;
      setRunners(items);
      setError("");
    } catch (e) { if (epoch !== discoveryEpoch.current) return; setRunners([]); if (e instanceof APIError && e.status === 401) setUser(null); else setError(errorText(e)); }
    finally { if (epoch === discoveryEpoch.current) setDiscoveryLoading(false); }
  };
  const resetPage = () => { setDiscoveryLoading(true); void refresh(); };
  useEffect(() => { if (!user) return; void refresh(); const timer = setInterval(refresh, 5000); return () => { clearInterval(timer); discoveryEpoch.current++; }; }, [user?.id]);
  if (startupError) return <main className="paper-grid grid min-h-screen place-items-center p-6"><DesertAtmosphere /><section className="paper-card grid max-w-md gap-4 p-8"><Brand /><h1 className="text-xl font-bold">暂时无法打开工作台</h1><p className="error-box" role="alert">{startupError}</p><Button onClick={() => void loadStartup()}>重试</Button></section></main>;
  if (!startup || user === undefined) return <main className="grid min-h-screen place-items-center paper-grid"><DesertAtmosphere /><span role="status">正在打开工作台…</span></main>;
  if (!user) return <Auth startup={startup} onLogin={(user) => { discoveryEpoch.current++; setDiscoveryLoading(true); setRunners([]); setSelected(undefined); setUser(user); setError(""); }} initialError={error} />;
  const current = runners.find((r) => r.id === selected?.id);
  const matchingBinding = selected && current && bindingKey(selected.binding) === bindingKey(current.binding);
  const workspace = matchingBinding && selected.binding ? { ...current, binding: selected.binding } : undefined;
  return <div className="workspace">
    <aside className="sidebar"><Brand /><div className="machine-section"><div className="mb-3 flex items-center justify-between"><span className="muted font-semibold">开发环境</span><Button variant="ghost" size="icon" aria-label="刷新环境列表" onClick={resetPage}><Icon icon={refreshIcon} /></Button></div><nav className="machine-nav" aria-label="开发环境">{runners.map((m) => <button key={m.id} className={`machine-row ${selected?.id === m.id ? "active" : ""}`} onClick={() => setSelected(m)}><Icon icon={serverIcon} width={21} /><span className="min-w-0 flex-1"><span className="block truncate text-sm font-semibold">{m.name}</span><span className="mt-1 block text-xs text-muted-foreground"><i className={`status-dot ${m.online ? "online" : ""}`} />{!m.binding ? m.kind === "managed" ? "准备中" : "未绑定" : m.online ? "在线" : "离线"}{m.os && <> · {m.os === "darwin" ? "macOS" : m.os}</>}</span></span></button>)}</nav>{startup.attached && <Button className="mt-2 w-full" variant="outline" onClick={() => setAdding(true)}><Icon icon={addIcon} />接入开发机</Button>}{startup.managed && <Button className="mt-2 w-full" variant="outline" onClick={() => setProvisioning(true)}><Icon icon={addIcon} />创建托管环境</Button>}{!startup.tenant_scoped && <Button className="mt-2 w-full" variant="outline" onClick={() => setProfilesOpen(true)}><Icon icon={settingsIcon} />管理 Profile</Button>}<Button className="mt-2 w-full" variant="ghost" disabled={!selected} onClick={() => setManagingRunner(true)}>管理选中环境</Button></div><div className="sidebar-footer mt-auto border-t border-foreground/20 pt-4"><p className="mb-4 text-xs leading-relaxed text-muted-foreground">任务在开发环境中运行。<br />关闭网页，任务继续。</p><div className="flex items-center gap-2"><span className="min-w-0 flex-1 truncate text-xs" title={user.email}>{user.email || user.id}</span><Button variant="ghost" size="icon" aria-label="退出登录" onClick={() => void post("/api/v1/auth/logout", {}).then(() => setUser(null)).catch((e) => setError(errorText(e)))}><Icon icon={logoutIcon} /></Button></div></div></aside>
    <main className="workbench paper-grid"><DesertAtmosphere />{error && <div className="error-box m-4" role="alert">{error}</div>}<ParallelWorkbench key={user.id} accountID={user.id} runners={runners} runnersLoading={discoveryLoading} selectedRunner={selected} onSelectRunner={setSelected} profileVersion={profileVersion} onManageProfiles={() => setProfilesOpen(true)} /></main>
    <Dialog open={managingRunner} onOpenChange={setManagingRunner}><DialogContent><DialogTitle className="mb-4 text-xl font-bold">{selected?.name ?? "开发环境"}</DialogTitle><DialogDescription className="sr-only">查看开发环境状态和连接设置。</DialogDescription>{workspace ? <RunnerDetails runner={workspace} onRefresh={refresh} onRemoved={() => { setSelected(undefined); setManagingRunner(false); }} /> : selected && current && !matchingBinding ? <div className="grid gap-3"><p>环境绑定已变化。重新选择启动环境不会改变已有分屏的目标。</p><Button onClick={() => setSelected(current)}>选择当前环境</Button></div> : current && !current.binding ? current.kind === "managed" ? <ManagedPending runner={current} onRefresh={refresh} /> : <AttachedPending runner={current} onRefresh={refresh} /> : <p>该环境暂不可访问，请刷新列表。</p>}</DialogContent></Dialog>
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
