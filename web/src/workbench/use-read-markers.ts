import { useEffect, useState } from "react";
import { targetKey, type Agent } from "./model";

// Read status belongs to this browser view; it never writes application metadata.
export function useReadMarkers(scope: string, focused?: Agent) {
  const [read, setRead] = useState<{ scope: string; positions: Record<string, number> }>({ scope, positions: {} });
  const [visible, setVisible] = useState(!document.hidden && document.hasFocus());
  useEffect(() => {
    const update = () => setVisible(!document.hidden && document.hasFocus());
    window.addEventListener("focus", update);
    window.addEventListener("blur", update);
    document.addEventListener("visibilitychange", update);
    return () => {
      window.removeEventListener("focus", update);
      window.removeEventListener("blur", update);
      document.removeEventListener("visibilitychange", update);
    };
  }, []);
  const activity = focused?.runtime.activity;
  const key = focused && activity ? `${targetKey(focused.target)}:${activity.epoch}` : "";
  const sequence = activity?.sequence ?? 0;
  useEffect(() => {
    if (!visible || !key) return;
    setRead((previous) => {
      const positions = previous.scope === scope ? previous.positions : {};
      if ((positions[key] ?? 0) >= sequence) return previous;
      return { scope, positions: { ...positions, [key]: sequence } };
    });
  }, [scope, key, sequence, visible]);
  return { unread: (agent: Agent) => {
    const activity = agent.runtime.activity;
    const positions = read.scope === scope ? read.positions : {};
    return !!activity && activity.sequence > (positions[`${targetKey(agent.target)}:${activity.epoch}`] ?? 0);
  } };
}
