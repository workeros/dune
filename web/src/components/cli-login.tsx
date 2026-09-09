import { useEffect, useState } from "react";
import { Brand } from "@/components/brand";
import { DesertAtmosphere } from "@/components/desert-atmosphere";
import { Button } from "@/components/ui/button";
import { errorText, post, request, type User } from "@/lib/api";

const storageKey = () => `dune-cli-login:${new URL(".", window.location.href).pathname}`;

export function pendingCLIRequest(): string {
  const id = new URL(window.location.href).searchParams.get("cli_login") ?? sessionStorage.getItem(storageKey()) ?? "";
  if (!/^[a-f0-9]{32}$/.test(id)) return "";
  sessionStorage.setItem(storageKey(), id);
  return id;
}

export function clearCLIRequest() {
  sessionStorage.removeItem(storageKey());
  window.history.replaceState(null, "", new URL(".", window.location.href));
}

type Review = { id: string; code: string; site: string; expires_at: number; confirmed: boolean };

export function CLILogin({ id, user, onDone }: { id: string; user: User; onDone: () => void }) {
  const [review, setReview] = useState<Review>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState("");
  useEffect(() => {
    let active = true;
    request<Review>(`/api/auth/cli/${id}`).then((value) => {
      if (!active) return;
      setReview(value);
      if (value.confirmed) { clearCLIRequest(); setResult("此请求已确认，请回到终端继续。"); }
    }).catch((e) => { if (active) setError(errorText(e)); });
    return () => { active = false; };
  }, [id]);
  const decide = async (approve: boolean) => {
    if (!review) return;
    setBusy(true); setError("");
    try {
      await post(`/api/auth/cli/${id}`, { code: review.code, approve });
      clearCLIRequest();
      setResult(approve ? "已确认，请回到终端继续。" : "已拒绝此登录请求。");
    } catch (e) { setError(errorText(e)); }
    finally { setBusy(false); }
  };
  return <main className="paper-grid grid min-h-screen place-items-center p-6"><DesertAtmosphere /><section className="paper-card grid w-full max-w-lg gap-5 p-8">
    <Brand /><h1 className="text-2xl font-bold">确认 CLI 登录</h1>
    {result ? <p role="status">{result}</p> : review ? <>
      <p className="muted">将以 <strong className="text-foreground">{user.email || "当前账号"}</strong> 登录终端中的 Dune。</p>
      <p className="break-all text-sm">站点：{review.site}</p>
      <div className="rounded-lg border border-foreground/20 bg-mint p-5 text-center"><span className="block text-xs">请核对终端显示的代码</span><strong className="mt-2 block font-mono text-3xl tracking-widest">{review.code}</strong></div>
      <p className="muted text-sm leading-6">仅确认你刚刚在自己的终端发起、且核对码一致的请求。CLI 最长登录八小时；退出当前浏览器会话也会撤销此 CLI 登录。</p>
      <div className="flex gap-3"><Button disabled={busy} onClick={() => void decide(true)}>确认登录</Button><Button variant="outline" disabled={busy} onClick={() => void decide(false)}>拒绝</Button></div>
    </> : !error && <p role="status">正在读取登录请求…</p>}
    {error && <p className="error-box" role="alert">{error}</p>}
    <Button variant="ghost" onClick={() => { clearCLIRequest(); onDone(); }}>返回工作台</Button>
  </section></main>;
}
