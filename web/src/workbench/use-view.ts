import { useCallback, useEffect, useRef, useState, type SetStateAction } from "react";
import { APIError, errorText, request } from "../lib/api";
import { emptyView, leaves, type SavedView, type ViewSpec } from "./model";

export function useView(prefix: string) {
  const [view, setView] = useState<ViewSpec>(emptyView), [revision, setRevision] = useState(0);
  const [loaded, setLoaded] = useState(false), [saving, setSaving] = useState(false), [error, setError] = useState("");
  const [saved, setSaved] = useState(JSON.stringify(emptyView));
  const currentView = useRef(view);
  const change = useCallback((update: SetStateAction<ViewSpec>) => {
    const next = typeof update === "function" ? update(currentView.current) : update;
    currentView.current = next; setView(next);
  }, []);
  const alive = useRef(true), loading = useRef(false);
  const reload = useCallback(async () => {
    if (saving || loading.current) return;
    loading.current = true; setLoaded(false); setError("");
    try {
      const result = await request<SavedView>(`${prefix}/workbench/views/main`);
      if (!alive.current) return;
      const next = { root: result.root, focus_pane: result.focus_pane ?? leaves(result.root)[0]?.id, review_pane: result.review_pane };
      change(next); setRevision(result.revision); setSaved(JSON.stringify(next)); setLoaded(true);
    } catch (cause) { if (alive.current) setError(errorText(cause)); }
    finally { loading.current = false; }
  }, [prefix, saving]);
  useEffect(() => { alive.current = true; void reload(); return () => { alive.current = false; }; }, [prefix]);
  const serialized = JSON.stringify(view), dirty = serialized !== saved;
  useEffect(() => {
    if (!loaded || saving || error || !dirty) return;
    const timer = setTimeout(() => {
      setSaving(true);
      void request<SavedView>(`${prefix}/workbench/views/main`, { method: "PUT", body: JSON.stringify({ ...view, revision }) }).then((result) => {
        if (alive.current) { setRevision(result.revision); setSaved(serialized); }
      }).catch((cause) => {
        if (alive.current) setError(cause instanceof APIError && cause.status === 409 ? "另一个窗口已更新布局。当前现场已保留，请加载已保存布局后再调整。" : `布局尚未确认保存：${errorText(cause)}`);
      }).finally(() => { if (alive.current) setSaving(false); });
    }, 400);
    return () => clearTimeout(timer);
  }, [loaded, saving, error, dirty, serialized, revision, prefix]);
  return { view, change, loaded, saving: saving || dirty, error, reload };
}
