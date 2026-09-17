import { useEffect, useRef, useState } from "react";
import { errorText, post, request } from "../lib/api";
import { targetKey, type Agent, type ReadMarker } from "./model";

const markerKey = (marker: ReadMarker) => `${targetKey(marker.target)}:${marker.epoch}`;
export function useReadMarkers(prefix: string, agents: Agent[], focused?: Agent) {
  const [positions, setPositions] = useState<Record<string, number>>({}), [error, setError] = useState("");
  const [visible, setVisible] = useState(!document.hidden && document.hasFocus());
  const sent = useRef(new Map<string, number>());
  useEffect(() => {
    const update = () => setVisible(!document.hidden && document.hasFocus());
    window.addEventListener("focus", update); window.addEventListener("blur", update); document.addEventListener("visibilitychange", update);
    return () => { window.removeEventListener("focus", update); window.removeEventListener("blur", update); document.removeEventListener("visibilitychange", update); };
  }, []);
  const markers = agents.flatMap((agent) => agent.runtime.activity ? [{ target: agent.target, epoch: agent.runtime.activity.epoch, sequence: 0 }] : []);
  const signature = JSON.stringify(markers);
  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const loaded: ReadMarker[] = [];
        for (let offset = 0; offset < markers.length; offset += 64) loaded.push(...(await post<{ items: ReadMarker[] }>(`${prefix}/workbench/read-markers/query`, { items: markers.slice(offset, offset + 64) })).items);
        if (alive) setPositions((old) => { const next = { ...old }; for (const marker of loaded) next[markerKey(marker)] = Math.max(next[markerKey(marker)] ?? 0, marker.sequence); return next; });
      } catch (cause) { if (alive) setError(`已读记录读取失败：${errorText(cause)}`); }
    })();
    return () => { alive = false; };
  }, [prefix, signature]);
  const activity = focused?.runtime.activity, focusKey = focused ? targetKey(focused.target) : "";
  useEffect(() => {
    if (!visible || !focused || !activity) return;
    const marker = { target: focused.target, epoch: activity.epoch, sequence: activity.sequence }, key = markerKey(marker);
    if ((positions[key] ?? 0) >= activity.sequence || (sent.current.get(key) ?? 0) >= activity.sequence) return;
    sent.current.set(key, activity.sequence);
    void request<ReadMarker>(`${prefix}/workbench/read-markers`, { method: "PUT", body: JSON.stringify(marker) }).then((saved) => {
      setPositions((old) => ({ ...old, [key]: Math.max(old[key] ?? 0, saved.sequence) })); setError("");
    }).catch((cause) => setError(`已读记录尚未确认保存：${errorText(cause)}`));
  }, [prefix, focusKey, activity?.epoch, activity?.sequence, visible, positions]);
  return { error, unread: (agent: Agent) => {
    const activity = agent.runtime.activity;
    return !!activity && activity.sequence > (positions[`${targetKey(agent.target)}:${activity.epoch}`] ?? 0);
  } };
}
