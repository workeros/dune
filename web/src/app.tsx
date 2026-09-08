import { useEffect, useRef, useState, type FormEvent } from "react";
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
import { ACPPane } from "@/components/acp";
import { TerminalPane } from "@/components/terminal";
import { Brand } from "@/components/brand";
import { CLILogin, pendingCLIRequest } from "@/components/cli-login";
import { APIError, request, post, call, errorText, type Page, type User, runnerPath, bindingKey, runtimeKey, type Binding, type Runner, type BoundRunner, type Runtime, type AgentConfig, type StartupInfo, type ManagedField, type ManagedTemplate, type ManagedOperation, type ManagedCreation, type ManagedReview } from "@/lib/api";

export function App() {
  const [cliRequest, setCLIRequest] = useState(pendingCLIRequest);
  const [user, setUser] = useState<User | null>(), [runners, setRunners] = useState<Runner[]>([]), [selected, setSelected] = useState<Runner>(), [adding, setAdding] = useState(false), [provisioning, setProvisioning] = useState(false), [error, setError] = useState(new URL(window.location.href).searchParams.get("login_error") === "1" ? "企业登录未完成或已过期，请重新登录。" : "");
  const [pageCursor, setPageCursor] = useState(""), [nextCursor, setNextCursor] = useState(""), [previousCursors, setPreviousCursors] = useState<string[]>([]);
  const discoveryEpoch = useRef(0);
  const [discoveryLoading, setDiscoveryLoading] = useState(true);
  const [startup, setStartup] = useState<StartupInfo>(), [startupError, setStartupError] = useState("");
  const loadStartup = async () => {
    setStartupError("");
    try { setStartup(await request<StartupInfo>("/api/bootstrap")); }
    catch (e) { setStartupError(errorText(e)); }
  };
  useEffect(() => { void loadStartup(); }, []);
  useEffect(() => { request<User>("/api/me").then(setUser).catch((e) => { setUser(null); if (!(e instanceof APIError && e.status === 401)) setError(errorText(e)); }); }, []);
  const refresh = async () => {
    const epoch = ++discoveryEpoch.current;
    try {
      const page = await request<Page<Runner>>(`/api/runners?cursor=${encodeURIComponent(pageCursor)}`);
      if (epoch !== discoveryEpoch.current) return;
      setRunners(page.items); setNextCursor(page.next_cursor ?? "");
      setError("");
    } catch (e) { if (epoch !== discoveryEpoch.current) return; setRunners([]); setNextCursor(""); if (e instanceof APIError && e.status === 401) setUser(null); else setError(e instanceof APIError && e.status === 400 && pageCursor ? "分页已失效，请刷新列表回到第一页。" : errorText(e)); }
    finally { if (epoch === discoveryEpoch.current) setDiscoveryLoading(false); }
  };
  const resetPage = () => { setDiscoveryLoading(true); setPreviousCursors([]); if (pageCursor) { setRunners([]); setNextCursor(""); setSelected(undefined); setPageCursor(""); } else void refresh(); };
  useEffect(() => { if (!user || cliRequest) return; void refresh(); const timer = setInterval(refresh, 5000); return () => { clearInterval(timer); discoveryEpoch.current++; }; }, [user?.id, cliRequest, pageCursor]);
  if (startupError) return <main className="paper-grid grid min-h-screen place-items-center p-6"><section className="paper-card grid max-w-md gap-4 p-8"><Brand /><h1 className="text-xl font-bold">暂时无法打开工作台</h1><p className="error-box" role="alert">{startupError}</p><Button onClick={() => void loadStartup()}>重试</Button></section></main>;
  if (!startup || user === undefined) return <main className="grid min-h-screen place-items-center paper-grid"><span role="status">正在打开工作台…</span></main>;
  if (!user) return <Auth startup={startup} onLogin={(user) => { discoveryEpoch.current++; setDiscoveryLoading(true); setRunners([]); setSelected(undefined); setPageCursor(""); setNextCursor(""); setPreviousCursors([]); setUser(user); setError(""); }} initialError={error} />;
  if (cliRequest) return <CLILogin id={cliRequest} user={user} onDone={() => setCLIRequest("")} />;
  const current = runners.find((r) => r.id === selected?.id);
  const matchingBinding = selected && current && bindingKey(selected.binding) === bindingKey(current.binding);
  const workspace = matchingBinding && selected.binding ? { ...current, binding: selected.binding } : undefined;
  const renderWorkbench = () => {
    if (workspace) return <Workspace key={bindingKey(workspace.binding)} runner={workspace} onRevoke={async () => { setSelected(undefined); await refresh(); }} />;
    if (discoveryLoading && !runners.length) return <div className="m-auto p-8 muted" role="status">正在加载环境…</div>;
    if (error) return <div className="m-auto p-8"><p className="muted mb-4">暂时无法获取可访问的环境。</p><Button variant="outline" onClick={resetPage}>重新加载列表</Button></div>;
    if (selected && current && !matchingBinding) return <div className="m-auto max-w-lg p-8"><h1 className="mb-3 text-xl font-bold">环境绑定已变化</h1><p className="muted mb-5 leading-7">原工作区已关闭。确认进入 {current.name} 的当前环境后，才能继续操作。</p><Button onClick={() => setSelected(current)}>进入当前环境</Button></div>;
    if (selected && !current) return <div className="m-auto max-w-lg p-8"><h1 className="mb-3 text-xl font-bold">该环境暂不可访问</h1><p className="muted leading-7">请刷新列表或选择其他环境。</p></div>;
    if (selected && current && !current.binding) return current.kind === "managed" ? <ManagedPending runner={current} onRefresh={refresh} /> : <div className="m-auto max-w-lg p-8"><h1 className="mb-3 text-xl font-bold">{current.name} 尚未绑定开发机</h1><p className="muted leading-7">绑定可用后，再进入环境开始工作。</p></div>;
    if (runners.length > 0) return <div className="m-auto max-w-lg p-8"><h1 className="mb-3 text-xl font-bold">选择一个开发环境</h1><p className="muted leading-7">从列表选择环境，查看或继续其中的工作。</p></div>;
    if ((pageCursor || nextCursor)) return <div className="m-auto max-w-lg p-8"><h1 className="mb-3 text-xl font-bold">本页暂无可访问的环境</h1><p className="muted leading-7">可以继续翻页，或刷新列表回到第一页。</p></div>;
    return <div className="m-auto max-w-lg p-8"><div className="mb-6 inline-flex rounded-xl border border-foreground bg-mint p-4"><Icon icon={serverIcon} width={30} /></div><h1 className="mb-3 text-3xl font-bold tracking-tight">准备一个开发环境。</h1><p className="muted mb-7 leading-7">接入已有 Linux 或 macOS 开发机，或按站点模板创建托管环境。代码和会话历史留在开发环境中。</p><div className="flex flex-wrap gap-3">{startup.attached && <Button onClick={() => setAdding(true)}><Icon icon={addIcon} />接入开发机</Button>}{startup.managed && <Button variant="outline" onClick={() => setProvisioning(true)}><Icon icon={addIcon} />创建托管环境</Button>}{!startup.attached && !startup.managed && <p className="muted">此站点未开放环境接入。</p>}</div></div>;
  };
  return <div className="workspace">
    <aside className="sidebar"><Brand /><div className="machine-section"><div className="mb-3 flex items-center justify-between"><span className="muted font-semibold">开发环境</span><Button variant="ghost" size="icon" aria-label="刷新环境列表" onClick={resetPage}><Icon icon={refreshIcon} /></Button></div><nav className="machine-nav" aria-label="开发环境">{runners.map((m) => <button key={m.id} className={`machine-row ${selected?.id === m.id ? "active" : ""}`} onClick={() => setSelected(m)}><Icon icon={serverIcon} width={21} /><span className="min-w-0 flex-1"><span className="block truncate text-sm font-semibold">{m.name}</span><span className="mt-1 block text-xs text-muted-foreground"><i className={`status-dot ${m.online ? "online" : ""}`} />{!m.binding ? m.kind === "managed" ? "准备中" : "未绑定" : m.online ? "在线" : "离线"}{m.os && <> · {m.os === "darwin" ? "macOS" : m.os}</>}</span></span></button>)}</nav>{(previousCursors.length > 0 || nextCursor) && <nav className="mt-3 flex flex-wrap gap-2" aria-label="环境分页"><Button variant="outline" size="sm" disabled={!previousCursors.length} onClick={() => { setDiscoveryLoading(true); setRunners([]); setNextCursor(""); setSelected(undefined); setPageCursor(previousCursors.at(-1) ?? ""); setPreviousCursors((items) => items.slice(0, -1)); }}>上一页</Button><Button variant="outline" size="sm" disabled={!nextCursor} onClick={() => { setDiscoveryLoading(true); setRunners([]); setNextCursor(""); setSelected(undefined); setPreviousCursors((items) => [...items, pageCursor]); setPageCursor(nextCursor); }}>下一页</Button></nav>}{startup.attached && <Button className="mt-2 w-full" variant="outline" onClick={() => setAdding(true)}><Icon icon={addIcon} />接入开发机</Button>}{startup.managed && <Button className="mt-2 w-full" variant="outline" onClick={() => setProvisioning(true)}><Icon icon={addIcon} />创建托管环境</Button>}</div><div className="sidebar-footer mt-auto border-t border-foreground/20 pt-4"><p className="mb-4 text-xs leading-relaxed text-muted-foreground">任务在开发环境中运行。<br />关闭网页，任务继续。</p><div className="flex items-center gap-2"><span className="min-w-0 flex-1 truncate text-xs" title={user.email}>{user.email || "企业用户"}</span><Button variant="ghost" size="icon" aria-label="退出登录" onClick={() => void post("/api/auth/logout", {}).then(() => setUser(null)).catch((e) => setError(errorText(e)))}><Icon icon={logoutIcon} /></Button></div></div></aside>
    <main className="workbench paper-grid">{error && <div className="error-box m-4" role="alert">{error}</div>}{renderWorkbench()}</main>
    {startup.attached && <AddMachine open={adding} onOpenChange={setAdding} />}
    {startup.managed && <AddManaged open={provisioning} onOpenChange={setProvisioning} onCreated={async (runner) => { setSelected(runner); resetPage(); }} />}
  </div>;
}

function Auth({ startup, onLogin, initialError }: { startup: StartupInfo; onLogin: (user: User) => void; initialError: string }) {
  const passwordLogin = startup.login_methods.find((method) => method.kind === "password");
  const externalLogin = startup.login_methods.find((method) => method.kind === "external");
  const [register, setRegister] = useState(false), [email, setEmail] = useState(""), [password, setPassword] = useState(""), [error, setError] = useState(initialError), [busy, setBusy] = useState(false);
  const submit = async (event: FormEvent) => { event.preventDefault(); setBusy(true); setError(""); try { onLogin(await post<User>(register ? "/api/auth/register" : passwordLogin!.url, { email, password })); } catch (e) { setError(errorText(e)); } finally { setBusy(false); } };
  return <main className="paper-grid flex min-h-screen items-center justify-center p-6"><div className="w-full max-w-md"><div className="mb-8"><Brand /></div><section className="paper-card p-8"><p className="mb-2 text-xs font-semibold uppercase tracking-[.15em] text-muted-foreground">Your machine. Your workspace.</p><h1 className="mb-2 text-2xl font-bold">{register ? "开始使用 Dune" : "回到你的工作台"}</h1><p className="muted mb-7">在浏览器中，继续开发机上的工作。</p>{passwordLogin ? <form onSubmit={submit} className="grid gap-5"><label>邮箱<Input type="email" autoComplete="username" required value={email} onChange={(e) => setEmail(e.target.value)} /></label><label>密码<Input type="password" autoComplete={register ? "new-password" : "current-password"} minLength={register ? 12 : undefined} maxLength={256} required value={password} onChange={(e) => setPassword(e.target.value)} />{register && <span className="text-xs font-normal text-muted-foreground">至少 12 个字符</span>}</label>{error && <div className="error-box" role="alert">{error}</div>}<Button type="submit" disabled={busy}>{busy ? "请稍候…" : register ? "注册并进入工作台" : "登录"}</Button></form> : externalLogin ? <div className="grid gap-4">{error && <div className="error-box" role="alert">{error}</div>}<Button onClick={() => { window.location.assign(externalLogin.url); }}>使用企业账号登录</Button></div> : <p role="status">此站点暂未提供可用的登录方式。</p>}{passwordLogin && startup.local_registration && <button className="mt-6 text-sm underline decoration-foreground/30 underline-offset-4" onClick={() => { setRegister(!register); setError(""); }}>{register ? "已有账号？登录" : "第一次使用？自由注册"}</button>}{passwordLogin && !startup.local_registration && <p className="muted mt-6 text-sm">此站点未开放本地注册，请使用已有账号登录。</p>}</section><p className="mt-6 text-center text-xs text-muted-foreground">无需预装 Agent，即可接入开发机。</p></div></main>;
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

function AddManaged({ open, onOpenChange, onCreated }: { open: boolean; onOpenChange: (open: boolean) => void; onCreated: (runner: Runner) => Promise<void> }) {
  const [templates, setTemplates] = useState<ManagedTemplate[]>([]), [templateKey, setTemplateKey] = useState(""), [name, setName] = useState(""), [values, setValues] = useState<Record<string, string | boolean>>({}), [error, setError] = useState(""), [busy, setBusy] = useState(false), [loading, setLoading] = useState(false);
  const requestKey = useRef(managedRequestKey());
  const selected = templates.find((template) => JSON.stringify([template.fabric_id, template.id, template.version]) === templateKey);
  useEffect(() => {
    if (!open) return;
    let alive = true;
    setLoading(true); setError("");
    request<{ items: ManagedTemplate[] }>("/api/managed/templates").then(({ items }) => {
      if (!alive) return;
      setTemplates(items); setTemplateKey(items[0] ? JSON.stringify([items[0].fabric_id, items[0].id, items[0].version]) : "");
    }).catch((e) => { if (alive) setError(errorText(e)); }).finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [open]);
  useEffect(() => { setValues({}); }, [templateKey]);
  const close = (value: boolean) => {
    onOpenChange(value);
    if (!value) { setName(""); setValues({}); setError(""); requestKey.current = managedRequestKey(); }
  };
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!selected) return;
    setBusy(true); setError("");
    try {
      const body = managedRequestBody(selected, name, values, requestKey.current);
      const created = await request<ManagedCreation>("/api/managed/runners", { method: "POST", body });
      close(false); await onCreated(created.runner);
    } catch (e) { setError(errorText(e)); } finally { setBusy(false); }
  };
  return <Dialog open={open} onOpenChange={close}><DialogContent><DialogTitle className="mb-2 text-xl font-bold">创建托管环境</DialogTitle><DialogDescription className="muted mb-6">选择管理员提供的模板。提交后可以离开页面，创建会在后台继续。</DialogDescription>{loading ? <p role="status">正在读取可用模板…</p> : templates.length ? <form className="grid gap-4" onSubmit={submit}><label>环境名称<Input value={name} onChange={(e) => setName(e.target.value)} required maxLength={120} placeholder="例如：项目开发环境" /></label><label>模板<select value={templateKey} onChange={(e) => setTemplateKey(e.target.value)}>{templates.map((template) => { const key = JSON.stringify([template.fabric_id, template.id, template.version]); return <option key={key} value={key}>{template.name} · {template.version}</option>; })}</select></label>{selected?.fields.map((field) => <ManagedInput key={field.name} field={field} value={values[field.name]} onChange={(value) => setValues((current) => ({ ...current, [field.name]: value }))} />)}{error && <div className="error-box" role="alert">{error}</div>}<Button type="submit" disabled={busy}>{busy ? "提交中…" : "创建环境"}</Button></form> : !error && <p className="muted">当前账号没有可用的托管模板。</p>}{error && !templates.length && <div className="error-box" role="alert">{error}</div>}</DialogContent></Dialog>;
}

function ManagedInput({ field, value, onChange }: { field: ManagedField; value: string | boolean | undefined; onChange: (value: string | boolean) => void }) {
  if (field.type === "boolean") return <label className="flex items-center gap-3"><input type="checkbox" checked={value === true} onChange={(e) => onChange(e.target.checked)} /><span>{field.label}{field.required ? " *" : ""}</span></label>;
  if (field.choices?.length) return <label>{field.label}<select required={field.required} value={typeof value === "string" ? value : ""} onChange={(e) => onChange(e.target.value)}><option value="">请选择</option>{field.choices.map((choice) => <option key={choice} value={choice}>{choice}</option>)}</select></label>;
  return <label>{field.label}<Input type="text" inputMode={field.type === "integer" ? "numeric" : "text"} required={field.required} maxLength={field.type === "string" ? field.max_length : undefined} value={typeof value === "string" ? value : ""} onChange={(e) => onChange(e.target.value)} />{field.type === "integer" && <span className="text-xs font-normal text-muted-foreground">范围：{field.minimum} 至 {field.maximum}</span>}</label>;
}

const managedStage: Record<string, string> = { queued: "等待后台处理", creating: "正在创建资源", bootstrapping: "正在配置开发环境", waiting_connection: "等待开发机首次连接", renewing: "正在续期", closing_access: "正在关闭访问", destroying: "正在删除资源", succeeded: "操作已完成", failed: "操作失败" };

function ManagedPending({ runner, onRefresh }: { runner: Runner; onRefresh: () => Promise<void> }) {
  const [status, setStatus] = useState<ManagedOperation>(), [error, setError] = useState(""), [destroying, setDestroying] = useState(false), [confirming, setConfirming] = useState(false);
  const [reviewing, setReviewing] = useState(false), [reviewBusy, setReviewBusy] = useState(false), [reviewMode, setReviewMode] = useState<"reconcile" | "candidate">("reconcile"), [candidate, setCandidate] = useState(""), [reason, setReason] = useState(""), [review, setReview] = useState<ManagedReview>();
  const destroyKey = useRef(managedRequestKey());
  const reviewKey = useRef(managedRequestKey());
  const load = async () => {
    try { const next = await request<ManagedOperation>(`/api/managed/runners/${encodeURIComponent(runner.id)}`); const latest = await request<ManagedReview>(`/api/managed/operations/${encodeURIComponent(next.id)}/reviews`).catch((e) => { if (e instanceof APIError && e.status === 404) return undefined; throw e; }); setStatus(next); setReview(latest); setError(""); if (next.finished && next.outcome === "succeeded") await onRefresh(); }
    catch (e) { setError(errorText(e)); }
  };
  useEffect(() => { let alive = true; const refresh = async () => { if (alive) await load(); }; void refresh(); const timer = setInterval(() => void refresh(), 2500); return () => { alive = false; clearInterval(timer); }; }, [runner.id]);
  const destroy = async () => {
    setDestroying(true); setError("");
    try { await request(`/api/managed/runners/${encodeURIComponent(runner.id)}`, { method: "DELETE", body: JSON.stringify({ request_key: destroyKey.current }) }); setConfirming(false); await onRefresh(); }
    catch (e) { setError(errorText(e)); } finally { setDestroying(false); }
  };
  const submitReview = async (event: FormEvent) => {
    event.preventDefault(); setReviewBusy(true); setError("");
    try {
      const accepted = await post<ManagedReview>(`/api/managed/operations/${encodeURIComponent(status!.id)}/reviews`, { request_key: reviewKey.current, mode: reviewMode, candidate_resource_ref: reviewMode === "candidate" ? candidate : "", reason });
      setReview(accepted); setReviewing(false); reviewKey.current = managedRequestKey(); setCandidate(""); setReason("");
    } catch (e) { setError(errorText(e)); } finally { setReviewBusy(false); }
  };
  const detail = status?.provider_outcome === "unknown" ? "外部结果尚未确认，后台只会查询，不会重复提交变更。" : status?.action === "destroy" ? status.finished && status.outcome === "succeeded" ? "供应商资源已确认删除，访问保持关闭。" : status.finished ? "自动删除已停止，访问保持关闭，管理员可根据已保存的结果继续处理。" : "访问已经关闭，后台正在确认并删除供应商资源。" : status?.outcome === "failed" ? "创建已停止。可以删除这个环境后重新创建。" : "创建会在后台继续，可以关闭此页面。";
  return <div className="m-auto w-full max-w-xl p-8"><section className="paper-card p-7"><div className="mb-5 inline-flex rounded-xl border border-foreground bg-yellow p-3"><Icon icon={serverIcon} width={28} /></div><h1 className="mb-2 text-2xl font-bold">{runner.name}</h1><p className="mb-2 font-semibold" role="status">{status ? managedStage[status.stage ?? ""] ?? status.stage : "正在读取生命周期状态…"}</p><p className="muted leading-7">{detail}</p>{status?.resource_ref && <p className="mt-4 break-all text-xs text-muted-foreground">资源：{status.resource_ref}</p>}{status?.expires_at && <p className="mt-1 text-xs text-muted-foreground">有效至：{new Date(status.expires_at).toLocaleString()}</p>}{status?.access_close_outcome && <p className="mt-1 text-xs text-muted-foreground">访问关闭：{status.access_close_outcome === "confirmed" ? "已确认" : status.access_close_outcome === "timed_out" ? "等待超时后继续" : "等待现有连接结束"}</p>}{review && <p className="mt-3 text-sm" role="status">人工核对：{review.completed_at ? review.outcome === "succeeded" ? "已由提供方确认" : review.outcome === "failed" ? "提供方确认失败" : "仍无法确认" : "已受理，等待后台核对"}{review.verified_resource_ref ? ` · ${review.verified_resource_ref}` : ""}</p>}{error && <div className="error-box mt-4" role="alert">{error}</div>}<div className="mt-6 flex gap-3"><Button variant="outline" onClick={() => void load()}><Icon icon={refreshIcon} />刷新状态</Button>{(status?.provider_outcome === "unknown" || status?.provider_outcome === "timed_out") && <Button variant="outline" onClick={() => setReviewing(true)}><Icon icon={refreshIcon} />人工核对</Button>}{status?.action !== "destroy" && <Button variant="ghost" onClick={() => setConfirming(true)}><Icon icon={deleteIcon} />删除环境</Button>}</div></section><Dialog open={reviewing} onOpenChange={setReviewing}><DialogContent><DialogTitle className="mb-2 text-xl font-bold">核对外部操作</DialogTitle><DialogDescription className="muted mb-6">后台只查询原操作。候选资源引用会先由提供方核验，不能直接改变绑定。</DialogDescription><form className="grid gap-4" onSubmit={submitReview}><label>核对方式<select value={reviewMode} onChange={(e) => setReviewMode(e.target.value as "reconcile" | "candidate")}><option value="reconcile">重新查询原操作</option>{status?.stage === "creating" && <option value="candidate">核验候选资源</option>}</select></label>{reviewMode === "candidate" && <label>候选资源引用<Input value={candidate} onChange={(e) => setCandidate(e.target.value)} required maxLength={1024} /></label>}<label>核对依据<Textarea value={reason} onChange={(e) => setReason(e.target.value)} required maxLength={512} rows={3} placeholder="例如：已在供应商控制台核对请求时间和标签" /></label><Button type="submit" disabled={reviewBusy || !status}>{reviewBusy ? "提交中…" : "提交核对请求"}</Button></form></DialogContent></Dialog><Dialog open={confirming} onOpenChange={setConfirming}><DialogContent><DialogTitle className="mb-3 text-xl font-bold">删除 {runner.name}？</DialogTitle><DialogDescription className="muted mb-6">访问会立即关闭，后台将继续确认并删除供应商资源。</DialogDescription><Button variant="destructive" disabled={destroying} onClick={() => void destroy()}>{destroying ? "提交中…" : "确认删除"}</Button></DialogContent></Dialog></div>;
}

function AddMachine({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const commandRef = useRef<HTMLTextAreaElement>(null);
  const [name, setName] = useState(""), [result, setResult] = useState<{ command: string; expires_at: number }>(), [error, setError] = useState(""), [busy, setBusy] = useState(false), [copied, setCopied] = useState(false);
  const submit = async (event: FormEvent) => { event.preventDefault(); setBusy(true); setError(""); try { setResult(await post("/api/enrollments", { name })); } catch (e) { setError(errorText(e)); } finally { setBusy(false); } };
  return <Dialog open={open} onOpenChange={(value) => { onOpenChange(value); if (!value) { setResult(undefined); setCopied(false); } }}><DialogContent><DialogTitle className="mb-2 text-xl font-bold">接入开发机</DialogTitle><DialogDescription className="muted mb-6">支持 Linux 和 macOS。机器主动连接 Gateway，不需要开放入站端口。</DialogDescription><form onSubmit={submit} className="grid gap-4"><label>机器名称<Input placeholder="例如：日常 Linux 开发机" value={name} onChange={(e) => setName(e.target.value)} required maxLength={120} /></label><Button disabled={busy} type="submit">{busy ? "生成中…" : "生成一次性绑定命令"}</Button></form>{result && <div className="mt-5 grid gap-3"><p className="text-sm">通过 SSH 登录开发机，执行安装与绑定命令：</p><Textarea ref={commandRef} aria-label="安装与绑定命令" readOnly rows={5} value={result.command} className="break-all bg-secondary font-mono text-xs" onFocus={(event) => event.currentTarget.select()} /><p className="text-xs text-muted-foreground">有效至 {new Date(result.expires_at * 1000).toLocaleTimeString()}，仅可使用一次。可选中命令后使用系统复制快捷键。</p><Button variant="outline" onClick={() => { if (navigator.clipboard) void navigator.clipboard.writeText(result.command).then(() => setCopied(true)).catch((e) => setError(errorText(e))); else { commandRef.current?.focus(); commandRef.current?.select(); } }}>{copied ? "已复制" : navigator.clipboard ? "复制命令" : "选择命令"}</Button></div>}{error && <div className="error-box mt-4" role="alert">{error}</div>}</DialogContent></Dialog>;
}

function Workspace({ runner, onRevoke }: { runner: BoundRunner; onRevoke: () => Promise<void> }) {
  const [sessions, setSessions] = useState<Runtime[]>([]), [selected, setSelected] = useState<Runtime>(), [configs, setConfigs] = useState<AgentConfig[]>([]), [configID, setConfigID] = useState(""), [cwd, setCwd] = useState(""), [error, setError] = useState(""), [busy, setBusy] = useState(false), [editing, setEditing] = useState(false), [browsing, setBrowsing] = useState(false), [tab, setTab] = useState<"terminal" | "diff">("terminal"), [revoking, setRevoking] = useState(false);
  const alive = useRef(true);
  const destroyKey = useRef(managedRequestKey());
  const loadSessions = async () => { const list = await call<Runtime[]>(runner.binding, "runtime.list"); if (alive.current) { setSessions(list); } };
  const loadConfigs = async () => { const list = await call<AgentConfig[]>(runner.binding, "agent.config", { action: "list" }); if (alive.current) setConfigs(list); };
  useEffect(() => { alive.current = true; if (!runner.online) return () => { alive.current = false; }; void Promise.all([call<{ home: string }>(runner.binding, "machine.info").then((info) => { if (alive.current) setCwd((old) => old || info.home); }), loadConfigs(), loadSessions()]).catch((e) => { if (alive.current) setError(errorText(e)); }); const timer = setInterval(() => void loadSessions().catch((e) => { if (alive.current) setError(errorText(e)); }), 4000); return () => { alive.current = false; clearInterval(timer); }; }, [runner.online]);
  const currentRuntime = sessions.find((r) => r.id === selected?.id);
  const selectedRuntime = selected && currentRuntime && runtimeKey(selected) === runtimeKey(currentRuntime) ? currentRuntime : undefined;
  const start = async (terminal = false) => {
    setBusy(true); setError("");
    try {
      const config = configs.find((c) => c.id === configID);
      if (!terminal && !config) throw new Error("请先选择或添加 Agent 配置。");
      const runtime = await post<Runtime>(runnerPath(runner.binding, "sessions"), { version: 1, kind: "agent", working_directory: cwd, adapter: terminal ? "pty" : config!.adapter, start: { argv: terminal ? ["/bin/sh"] : [config!.command, ...(config!.args ?? [])] }, env: terminal ? {} : config!.env, history_lines: terminal ? 0 : config!.history_lines });
      await loadSessions(); setSelected(runtime); setTab("terminal");
    } catch (e) { setError(errorText(e)); } finally { setBusy(false); }
  };
  const sessionAction = async (operation: string) => { if (!selectedRuntime) return; setError(""); try { await call(runner.binding, operation, {}, selectedRuntime); await loadSessions(); if (operation === "runtime.forget") setSelected(undefined); } catch (e) { setError(errorText(e)); } };
  return <><header className="work-header"><div><h1 className="flex items-center gap-2 text-lg font-bold"><Icon icon={serverIcon} />{runner.name}</h1><p className="mt-1 text-xs text-muted-foreground"><i className={`status-dot ${runner.online ? "online" : ""}`} />{runner.online ? "已连接开发机" : "开发机离线，历史暂不可访问"} · {runner.os}/{runner.arch}</p></div><div className="flex gap-2"><Button variant="outline" onClick={() => setEditing(true)} disabled={!runner.online}><Icon icon={settingsIcon} />Agent 配置</Button><Button variant="ghost" size="icon" aria-label="解绑开发机" onClick={() => setRevoking(true)}><Icon icon={deleteIcon} /></Button></div></header>
    {error && <div className="error-box mx-6 mt-4" role="alert">{error}</div>}
    <div className="flex flex-wrap gap-3 px-6 pt-4"><div className="flex min-w-60 flex-1 items-center gap-2"><Icon icon={folderIcon} /><Input aria-label="项目工作目录" className="font-mono" value={cwd} onChange={(e) => setCwd(e.target.value)} placeholder="开发机上的绝对路径" /><Button variant="outline" size="icon" aria-label="浏览项目目录" disabled={!runner.online} onClick={() => setBrowsing(true)}><Icon icon={folderIcon} /></Button></div><select aria-label="Agent 启动配置" value={configID} onChange={(e) => setConfigID(e.target.value)}><option value="">选择 Agent</option>{configs.map((c) => <option key={c.id} value={c.id}>{c.name} · {c.adapter.toUpperCase()}</option>)}</select><Button disabled={!runner.online || busy || !cwd} onClick={() => void start()}><Icon icon={playIcon} />启动 Agent</Button><Button variant="outline" disabled={!runner.online || busy || !cwd} onClick={() => void start(true)}><Icon icon={terminalIcon} />普通终端</Button></div>
    <div className="session-layout"><nav className="session-list" aria-label="会话列表"><p className="muted mb-2 text-xs font-semibold">会话 · {sessions.length}</p>{sessions.map((runtime) => <button className={`session-row ${selected && runtimeKey(runtime) === runtimeKey(selected) ? "active" : ""}`} key={runtimeKey(runtime)} aria-label={`连接会话 ${runtime.title || runtime.adapter} ${runtime.id.slice(0, 6)}`} aria-current={selected && runtimeKey(runtime) === runtimeKey(selected) ? "true" : undefined} onClick={() => { setSelected(runtime); setTab("terminal"); }}><span className="block truncate text-sm font-semibold">{runtime.title || runtime.adapter}</span><span className="mt-1 block text-xs text-muted-foreground">{runtime.state === "running" ? "运行中" : `已退出 · ${runtime.exit_code}`} · {runtime.id.slice(0, 6)}</span>{(!selected || runtimeKey(runtime) !== runtimeKey(selected)) && <span className="mt-1 block text-xs text-muted-foreground">点击连接</span>}</button>)}{!sessions.length && <p className="text-xs leading-5 text-muted-foreground">从上方打开终端，或启动一个 Agent。</p>}</nav><section className="paper-card flex min-h-0 min-w-0 flex-col overflow-hidden">{selectedRuntime ? <><div className="flex flex-wrap items-center justify-between gap-2 border-b border-foreground/20 p-2"><div className="flex gap-1"><Button variant={tab === "terminal" ? "outline" : "ghost"} size="sm" onClick={() => setTab("terminal")}><Icon icon={terminalIcon} />{selectedRuntime.adapter === "acp" ? "对话" : "终端"}</Button><Button variant={tab === "diff" ? "outline" : "ghost"} size="sm" onClick={() => setTab("diff")}><Icon icon={codeIcon} />Git diff</Button></div><span className="min-w-0 flex-1 truncate px-2 font-mono text-xs text-muted-foreground" title={selectedRuntime.working_directory}>{selectedRuntime.working_directory}</span><Button variant="ghost" size="sm" onClick={() => setSelected(undefined)}><Icon icon={disconnectIcon} />断开连接</Button>{selectedRuntime.state === "running" ? <Button variant="ghost" size="sm" onClick={() => void sessionAction("runtime.stop")}><Icon icon={stopIcon} />{selectedRuntime.adapter === "pty" ? "结束并清除历史" : "停止"}</Button> : <Button variant="ghost" size="sm" onClick={() => void sessionAction("runtime.forget")}><Icon icon={deleteIcon} />删除会话与历史</Button>}</div>{tab === "terminal" ? selectedRuntime.adapter === "pty" ? <TerminalPane key={runtimeKey(selectedRuntime)} binding={runner.binding} runtime={selectedRuntime} /> : <ACPPane key={runtimeKey(selectedRuntime)} binding={runner.binding} runtime={selectedRuntime} /> : <Diff binding={runner.binding} cwd={selectedRuntime.working_directory || cwd} />}</> : <div className="m-auto p-8 text-center"><Icon icon={terminalIcon} width={36} className="mx-auto mb-4" /><h2 className="mb-2 text-lg font-bold">{selected ? "会话执行身份已变化或不可用" : sessions.length ? "选择一个会话再连接" : "准备开始工作"}</h2><p className="muted max-w-sm leading-6">{selected ? "原连接已关闭，请从会话列表重新选择。未确认的输入或操作不会重发。" : sessions.length ? "现有会话继续在开发机上运行。点击左侧会话才会连接，进入或刷新页面不会自动接入。" : "选择项目目录，打开普通终端。也可以保存自己的 Agent 命令，再从这里启动。"}</p></div>}</section></div>
    <AgentEditor binding={runner.binding} open={editing} onOpenChange={setEditing} configs={configs} onSave={loadConfigs} />
    <DirectoryPicker binding={runner.binding} path={cwd} open={browsing} onOpenChange={setBrowsing} onSelect={setCwd} />
    <Dialog open={revoking} onOpenChange={setRevoking}><DialogContent><DialogTitle className="mb-3 text-xl font-bold">{runner.kind === "managed" ? "删除" : "解绑"} {runner.name}？</DialogTitle><DialogDescription className="muted mb-6">{runner.kind === "managed" ? "访问会立即关闭，后台将继续确认并删除供应商资源。" : "当前网页连接会被撤销。不会删除开发机文件，也不会终止远端 Agent 或终端进程。"}</DialogDescription><Button variant="destructive" onClick={() => { const action = runner.kind === "managed" ? request(`/api/managed/runners/${encodeURIComponent(runner.id)}`, { method: "DELETE", body: JSON.stringify({ request_key: destroyKey.current }) }) : request(runnerPath(runner.binding, "binding"), { method: "DELETE" }); void action.then(onRevoke).catch((e) => setError(errorText(e))); }}>确认{runner.kind === "managed" ? "删除" : "解绑"}</Button></DialogContent></Dialog>
  </>;
}

function AgentEditor({ binding, open, onOpenChange, configs, onSave }: { binding: Binding; open: boolean; onOpenChange: (open: boolean) => void; configs: AgentConfig[]; onSave: () => Promise<void> }) {
  const blank: AgentConfig = { id: "", name: "", command: "", args: [], env: {}, adapter: "pty", history_lines: 50000 };
  const [config, setConfig] = useState(blank), [args, setArgs] = useState("[]"), [env, setEnv] = useState("{}"), [error, setError] = useState(""), [busy, setBusy] = useState(false);
  const choose = (id: string) => { const next = configs.find((c) => c.id === id) ?? blank; setConfig(next); setArgs(JSON.stringify(next.args ?? [], null, 2)); setEnv(JSON.stringify(next.env ?? {}, null, 2)); setError(""); };
  const submit = async (event: FormEvent) => { event.preventDefault(); setBusy(true); setError(""); try { const parsedArgs: unknown = JSON.parse(args), parsedEnv: unknown = JSON.parse(env); if (!Array.isArray(parsedArgs) || parsedArgs.some((v) => typeof v !== "string")) throw new Error("参数必须是字符串 JSON 数组"); if (!parsedEnv || typeof parsedEnv !== "object" || Array.isArray(parsedEnv) || Object.values(parsedEnv).some((v) => typeof v !== "string")) throw new Error("环境变量必须是字符串值的 JSON 对象"); await call(binding, "agent.config", { action: "save", config: { ...config, args: parsedArgs, env: parsedEnv, history_lines: config.adapter === "pty" ? config.history_lines : 0 } }); await onSave(); onOpenChange(false); } catch (e) { setError(errorText(e)); } finally { setBusy(false); } };
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent><DialogTitle className="mb-2 text-xl font-bold">Agent 启动配置</DialogTitle><DialogDescription className="muted mb-5">保存在当前开发机。保存时无需安装或登录 Agent，启动时才检查命令是否可用。</DialogDescription><form className="grid gap-4" onSubmit={submit}><label>配置<select value={config.id} onChange={(e) => choose(e.target.value)}><option value="">新建配置</option>{configs.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></label><div className="grid grid-cols-[1fr_100px] gap-3"><label>名称<Input value={config.name} onChange={(e) => setConfig({ ...config, name: e.target.value })} required /></label><label>交互方式<select value={config.adapter} onChange={(e) => setConfig({ ...config, adapter: e.target.value as "pty" | "acp" })}><option value="pty">PTY</option><option value="acp">ACP</option></select></label></div><label>启动命令<Input className="font-mono" placeholder="例如 codex 或 /absolute/path/to/agent" value={config.command} onChange={(e) => setConfig({ ...config, command: e.target.value })} required /></label><label>参数（JSON 数组）<Textarea className="min-h-16 font-mono text-xs" value={args} onChange={(e) => setArgs(e.target.value)} spellCheck={false} /></label><label>环境变量（JSON 对象）<Textarea className="min-h-16 font-mono text-xs" value={env} onChange={(e) => setEnv(e.target.value)} spellCheck={false} /></label>{config.adapter === "pty" && <label>保留历史行数<Input type="number" min={1} max={200000} value={config.history_lines || 50000} onChange={(e) => setConfig({ ...config, history_lines: Number(e.target.value) })} /></label>}{error && <div className="error-box" role="alert">{error}</div>}<div className="flex justify-between gap-3">{config.id && <Button type="button" variant="ghost" onClick={() => void call(binding, "agent.config", { action: "delete", id: config.id }).then(onSave).then(() => choose("")).catch((e) => setError(errorText(e)))}>删除配置</Button>}<Button className="ml-auto" type="submit" disabled={busy}>{busy ? "保存中…" : "保存到开发机"}</Button></div></form></DialogContent></Dialog>;
}

function DirectoryPicker({ binding, path, open, onOpenChange, onSelect }: { binding: Binding; path: string; open: boolean; onOpenChange: (open: boolean) => void; onSelect: (path: string) => void }) {
  const [current, setCurrent] = useState(path), [dirs, setDirs] = useState<{ name: string; is_dir: boolean }[]>([]), [error, setError] = useState("");
  useEffect(() => { if (open) setCurrent(path); }, [open]);
  useEffect(() => { if (!open || !current) return; let alive = true; setError(""); call<{ name: string; is_dir: boolean }[]>(binding, "files", { action: "list", path: current }).then((entries) => { if (alive) setDirs(entries.filter((entry) => entry.is_dir).sort((a, b) => a.name.localeCompare(b.name))); }).catch((e) => { if (alive) setError(errorText(e)); }); return () => { alive = false; }; }, [open, current]);
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent><DialogTitle className="mb-2 text-xl font-bold">选择项目目录</DialogTitle><DialogDescription className="muted mb-4">浏览当前开发机上的目录。</DialogDescription><p className="mb-3 break-all font-mono text-sm">{current}</p><div className="mb-5 max-h-64 overflow-auto rounded border border-foreground/20"><button className="flex w-full gap-2 border-b border-foreground/10 p-3 text-left text-sm hover:bg-secondary" onClick={() => setCurrent(current.split("/").slice(0, -1).join("/") || "/")}><Icon icon={folderIcon} />.. 上一级</button>{dirs.map((dir) => <button key={dir.name} className="flex w-full gap-2 p-3 text-left text-sm hover:bg-secondary" onClick={() => setCurrent(`${current.replace(/\/$/, "")}/${dir.name}`)}><Icon icon={folderIcon} />{dir.name}</button>)}</div>{error && <div className="error-box mb-4" role="alert">{error}</div>}<Button disabled={!!error} onClick={() => { onSelect(current); onOpenChange(false); }}>使用此目录</Button></DialogContent></Dialog>;
}

function Diff({ binding, cwd }: { binding: Binding; cwd: string }) {
  const [diff, setDiff] = useState(""), [error, setError] = useState(""), [staged, setStaged] = useState(false), [busy, setBusy] = useState(false);
  const load = async () => { setBusy(true); setError(""); try { const result = await call<{ stdout: string; stderr: string; exit_code: number; truncated: boolean }>(binding, "git", { action: "diff", directory: cwd, staged }); if (result.exit_code !== 0) throw new Error(result.stderr || "Git diff 失败"); setDiff(result.stdout); if (result.truncated) setError("diff 超过显示上限，当前仅展示部分内容。"); } catch (e) { setError(errorText(e)); } finally { setBusy(false); } };
  useEffect(() => { void load(); }, [binding, cwd, staged]);
  return <div className="flex min-h-0 flex-1 flex-col"><div className="flex justify-between gap-3 border-b border-foreground/20 p-3"><select aria-label="Diff 范围" value={staged ? "staged" : "working"} onChange={(e) => setStaged(e.target.value === "staged")}><option value="working">工作区修改</option><option value="staged">暂存区修改</option></select><Button variant="ghost" onClick={() => void load()} disabled={busy}><Icon icon={refreshIcon} />刷新</Button></div>{error && <div className="error-box m-3" role="alert">{error}</div>}<pre className="min-h-0 flex-1 overflow-auto p-4 text-xs leading-6">{busy ? "读取开发机 diff…" : diff ? diff.split("\n").map((line, i) => <div key={i} className={line.startsWith("+") ? "bg-mint/40 text-[#285437]" : line.startsWith("-") ? "bg-primary/20 text-destructive" : ""}>{line || " "}</div>) : !error && "当前范围没有 Git diff。未跟踪的新文件不会包含在 git diff 中。"}</pre></div>;
}
