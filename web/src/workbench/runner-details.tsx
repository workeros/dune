import { useEffect, useRef, useState } from "react";
import { Button } from "../components/ui/button";
import { errorText, post, request, runnerPath, type BoundRunner, type ManagedOperation } from "../lib/api";

export function RunnerDetails({ runner, onRefresh, onRemoved }: { runner: BoundRunner; onRefresh: () => Promise<void>; onRemoved: () => void }) {
  const [status, setStatus] = useState<ManagedOperation>(), [error, setError] = useState(""), [busy, setBusy] = useState(false), [confirming, setConfirming] = useState(false);
  const requestKey = useRef(crypto.randomUUID());
  const refresh = async () => { if (runner.kind === "managed") setStatus(await request<ManagedOperation>(`/api/v1/managed/runners/${encodeURIComponent(runner.id)}`)); };
  useEffect(() => { void refresh().catch((cause) => setError(errorText(cause))); }, [runner.id]);
  const change = async (action: "pause" | "resume" | "remove") => {
    setBusy(true); setError("");
    try {
      if (action === "remove") await request(runner.kind === "managed" ? `/api/v1/managed/runners/${encodeURIComponent(runner.id)}` : runnerPath(runner.binding, "binding"), { method: "DELETE", ...(runner.kind === "managed" ? { body: JSON.stringify({ request_key: requestKey.current }) } : {}) });
      else await post(`/api/v1/managed/runners/${encodeURIComponent(runner.id)}/${action}`, { request_key: requestKey.current });
      requestKey.current = crypto.randomUUID();
      await onRefresh();
      if (action === "remove") onRemoved(); else await refresh();
    } catch (cause) { setError(errorText(cause)); } finally { setBusy(false); }
  };
  return <div className="grid gap-4"><p className="muted">{runner.online ? "开发机在线" : "开发机离线"} · {runner.os}/{runner.arch}</p>
    {status?.access_suspended && <p className="muted">{status.resource_state === "paused" ? "环境已暂停，恢复后可继续会话。" : "正在确认环境状态，访问暂时关闭。"}</p>}
    {status?.credential_state === "degraded" && <p className="error-box">Git 凭据刷新失败；环境仍在运行，后台会继续重试。</p>}
    {error && <p className="error-box" role="alert">{error}</p>}
    <div className="flex flex-wrap gap-2"><Button variant="outline" disabled={busy} onClick={() => void Promise.all([refresh(), onRefresh()]).catch((cause) => setError(errorText(cause)))}>刷新状态</Button>
      {status?.capabilities?.pause_resume && status.resource_state === "ready" && !status.access_suspended && <Button variant="outline" disabled={busy} onClick={() => void change("pause")}>暂停环境</Button>}
      {status?.capabilities?.pause_resume && status.resource_state === "paused" && <Button variant="outline" disabled={busy} onClick={() => void change("resume")}>恢复环境</Button>}
      <Button variant="ghost" disabled={busy} onClick={() => setConfirming(true)}>{runner.kind === "managed" ? "删除环境" : "解绑开发机"}</Button></div>
    {confirming && <div className="error-box"><p className="mb-3">{runner.kind === "managed" ? "删除云环境及其资源？访问将被撤销。" : "解绑开发机？现有网页连接会关闭，开发机文件和进程会保留。"}</p><Button variant="destructive" disabled={busy} onClick={() => void change("remove")}>确认{runner.kind === "managed" ? "删除" : "解绑"}</Button></div>}
  </div>;
}
