import { useCallback, useEffect, useRef, useState } from "react";
import { Button } from "../components/ui/button";
import { APIError, request as api, type AgentRuntime } from "../lib/api";
import type { AgentSession, ResumeResult } from "./launch";
import { targetFor, targetKey, type Agent } from "./model";

type Props = { prefix: string; recordID: string; current?: Agent; enabled: boolean; ready: boolean; onRefresh: () => Promise<void>; onRestored: (runtime: AgentRuntime, session: AgentSession) => void };

export function SessionRecovery({ prefix, recordID, current, enabled, ready, onRefresh, onRestored }: Props) {
  const [stored, setStored] = useState<AgentSession>(), [error, setError] = useState(""), [busy, setBusy] = useState(false);
  const [following, setFollowing] = useState(false), [uncertainRevision, setUncertainRevision] = useState<number>();
  const mounted = useRef(true), sequence = useRef(0);
  const remember = (next?: AgentSession) => { if (next?.id === recordID) setStored((old) => old && old.revision > next.revision ? old : next); };
  const refresh = useCallback(async () => {
    if (!enabled) return;
    const request = ++sequence.current;
    try {
      const session = await api<AgentSession>(`${prefix}/agent-sessions/${encodeURIComponent(recordID)}`);
      if (mounted.current && request === sequence.current) { remember(session); setError(""); }
    } catch (cause) { if (mounted.current && request === sequence.current) setError(recoveryError(cause)); }
  }, [prefix, recordID, enabled]);
  useEffect(() => { mounted.current = true; void refresh(); return () => { mounted.current = false; sequence.current++; }; }, [refresh]);
  const known = current?.session;
  const record = known && (!stored || known.revision >= stored.revision) ? known : stored;
  const confirmed = record?.status === "available" && record.attempt?.state === "ready" && record.selected;
  const reconnect = confirmed && current?.runtime.state === "running" && record.last_runtime && targetKey(current.target) === targetKey(targetFor(record.binding, record.last_runtime)) ? current : undefined;
  useEffect(() => {
    if (uncertainRevision !== undefined && record?.attempt?.state === "failed" && record.revision > uncertainRevision) setUncertainRevision(undefined);
    if (following && reconnect && record) { setFollowing(false); onRestored(reconnect.runtime, record); }
  }, [following, reconnect, record, onRestored, uncertainRevision]);
  const resume = async () => {
    if (!record || busy) return;
    if (reconnect) { onRestored(reconnect.runtime, record); return; }
    setBusy(true); setFollowing(true); setError("");
    try {
      const result = await api<ResumeResult>(`${prefix}/agent-sessions/${encodeURIComponent(recordID)}/resume`, { method: "POST", body: JSON.stringify({ revision: record.revision }) });
      if (!mounted.current) return;
      if (!result.session?.attempt) throw new Error("恢复响应不完整，请先检查恢复结果。");
      remember(result.session);
      if (result.runtime && result.session?.status === "available" && result.session.attempt?.state === "ready" && result.session.selected) {
        setFollowing(false); onRestored(result.runtime, result.session);
      }
    } catch (cause) {
      if (!mounted.current) return;
      const partial = cause instanceof APIError ? cause.result as ResumeResult | undefined : undefined;
      remember(partial?.session); setError(recoveryError(cause));
      const unknown = !(cause instanceof APIError) || ["RESULT_UNKNOWN", "INVALID_RESPONSE", "RECOVERY_INDEX_FAILED"].includes(cause.code);
      setUncertainRevision(unknown ? record.revision : undefined);
    } finally {
      if (mounted.current) { setBusy(false); void onRefresh(); }
    }
  };
  const recovering = record?.attempt?.kind === "resume" && ["starting", "capturing"].includes(record.attempt.state);
  const canResume = !!record?.native?.resume_supported && record.adapter === "acp" && record.status === "available" && ["ready", "failed"].includes(record.attempt?.state ?? "") && uncertainRevision === undefined;
  const message = recovering ? "正在恢复原生会话，等待确认。" : record?.status === "unknown" || uncertainRevision !== undefined ? "恢复结果尚未确认，请检查结果后再继续。" : reconnect ? "原生会话已经恢复，可以重新连接。" : canResume ? "使用保存的启动配置继续原生会话。" : record ? "此会话暂不可恢复。" : error ? "读取会话恢复记录失败。" : "正在读取会话恢复记录…";
  return <section className="agent-recovery" aria-label="会话恢复"><p role="status">{message}</p>{record?.reason && <small>{record.reason}</small>}{error && <p role="alert">{error}</p>}<div>
    {(canResume || reconnect || busy) && <Button size="sm" variant="outline" disabled={!enabled || !ready || busy} onClick={() => void resume()}>{busy ? "恢复中…" : reconnect ? "连接已恢复会话" : "继续会话"}</Button>}
    <Button size="sm" variant="ghost" disabled={!enabled || busy} onClick={() => { void refresh(); void onRefresh(); }}>检查恢复结果</Button>
  </div></section>;
}

function recoveryError(cause: unknown): string {
  if (cause instanceof APIError) {
    const messages: Record<string, string> = { RESULT_UNKNOWN: "请求结果未知，已保留恢复记录，请检查结果。", RECOVERY_INDEX_FAILED: "恢复记录尚未确认保存，请检查结果。", RECOVERY_UNAVAILABLE: "此会话缺少可用的原生恢复信息。", RECOVERY_STORAGE_CHANGED: "原会话的存储位置或执行账号已变化。", RUNTIME_ALIVE: "已有会话仍在运行，请刷新并重新连接。", STALE_SESSION: "原生会话已切换，请从 Agent 列表选择当前会话。", CONFLICT: "恢复记录已更新，请检查结果后再操作。", BINDING_CHANGED: "原开发环境绑定已变化，无法恢复到当前环境。" };
    if (messages[cause.code]) return messages[cause.code];
  }
  return cause instanceof Error ? cause.message : String(cause);
}
