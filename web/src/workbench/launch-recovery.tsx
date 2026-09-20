import { useEffect, useState } from "react";
import { Button } from "../components/ui/button";
import { bindingKey, errorText, post, type AgentRuntime, type Runner } from "../lib/api";
import type { LaunchReceipt } from "./launch";
import { launchesChanged, readLaunches, recordLaunchReceipt, removeLaunch, type LaunchScope, type SavedLaunch } from "./launch-records";

const admissionLabels = { unknown: "接纳未确认", accepted: "已接纳", not_accepted: "未接纳", expired: "回执已过期" };
const stageLabels: Record<string, string> = { accepted: "启动已接纳", worktree: "工作目录准备结果待确认", setup: "准备步骤结果待确认", host_starting: "进程启动结果待确认", started: "会话已启动", failed: "启动失败", unknown: "启动结果未知" };

export function LaunchRecovery({ scope, runners, onOpen }: { scope: LaunchScope; runners: Runner[]; onOpen: (runner: Runner, runtime: AgentRuntime) => void }) {
  const [items, setItems] = useState<SavedLaunch[]>([]), [error, setError] = useState(""), [reading, setReading] = useState("");
  const [confirmed, setConfirmed] = useState<Record<string, AgentRuntime>>({});
  useEffect(() => {
    const refresh = () => { try { setItems(readLaunches(scope)); } catch (cause) { setError(errorText(cause)); } };
    refresh(); window.addEventListener(launchesChanged, refresh);
    return () => window.removeEventListener(launchesChanged, refresh);
  }, [scope.accountID, scope.prefix]);
  const query = async (saved: SavedLaunch) => {
    setReading(saved.submission_id); setError("");
    setConfirmed((old) => { const next = { ...old }; delete next[saved.submission_id]; return next; });
    try {
      const receipt = await post<LaunchReceipt>(`${scope.prefix}/agents/launch-submission`, { submission_id: saved.submission_id, binding: saved.binding });
      recordLaunchReceipt(scope, saved.submission_id, receipt);
      const runtime = receipt.runtime;
      if (receipt.admission === "accepted" && receipt.stage === "started" && runtime?.id && runtime.incarnation && Number.isSafeInteger(runtime.generation) && runtime.generation > 0 && ["acp", "pty"].includes(runtime.adapter)) {
        setConfirmed((old) => ({ ...old, [saved.submission_id]: runtime }));
      }
    } catch (cause) { setError(`${errorText(cause)}；已保留原提交记录，可再次查询。`); }
    finally { setReading(""); }
  };
  if (!items.length && !error) return null;
  return <details className="border-t px-3 py-2 text-xs" aria-label="启动恢复记录"><summary>启动记录 · {items.length} · 刷新后可查询</summary>
    <p className="muted my-2">查询原启动不会重新启动 Agent。记录仅保存在当前账号的浏览器标签页中；移除记录不会停止会话。</p>
    {error && <p role="alert" className="error-box">{error}</p>}
    <div className="max-h-48 overflow-auto">{items.map((item) => {
      const runner = runners.find((runner) => bindingKey(runner.binding) === bindingKey(item.binding)), runtime = confirmed[item.submission_id];
      return <div className="flex flex-wrap items-center gap-2 border-b py-2" key={item.submission_id}>
        <strong>{runner?.name ?? "原环境绑定不可用"}</strong><time>{new Date(item.created_at).toLocaleTimeString()}</time><code title={item.submission_id}>{item.submission_id.slice(0, 8)}</code>
        <span role="status">{item.admission === "accepted" && item.stage ? stageLabels[item.stage] ?? "启动阶段待确认" : admissionLabels[item.admission ?? "unknown"]}</span>
        <Button size="sm" variant="ghost" disabled={!!reading} onClick={() => void query(item)}>{reading === item.submission_id ? "查询中…" : "查询原启动"}</Button>
        {runtime && <Button size="sm" variant="outline" disabled={!runner?.online || !!reading} onClick={() => { if (runner?.online) onOpen(runner, runtime); }}>打开原会话</Button>}
        <Button size="sm" variant="ghost" disabled={!!reading} onClick={() => { try { removeLaunch(scope, item.submission_id); } catch (cause) { setError(errorText(cause)); } }}>移除本地记录</Button>
      </div>;
    })}</div>
  </details>;
}
