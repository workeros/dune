import { useEffect, useRef, useState } from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { Icon } from "@iconify/react";
import historyIcon from "@iconify-icons/ri/history-line";
import { Button } from "./ui/button";
import { socketURL, call, errorText, type Runtime } from "@/lib/api";

export function TerminalPane({ machine, runtime }: { machine: string; runtime: Runtime }) {
  const container = useRef<HTMLDivElement>(null);
  const terminal = useRef<XTerm | undefined>(undefined);
  const [status, setStatus] = useState("连接中"), [error, setError] = useState("");
  const [connected, setConnected] = useState(false);
  useEffect(() => {
    if (!container.current) return;
    const term = new XTerm({ fontFamily: '"SFMono-Regular",Consolas,monospace', fontSize: 14, lineHeight: 1.2, scrollback: 0, cursorBlink: true, theme: { background: "#202923", foreground: "#f1eedf", cursor: "#ef887e", selectionBackground: "#607363" } });
    terminal.current = term;
    const fit = new FitAddon(); term.loadAddon(fit); term.open(container.current);
    let socket: WebSocket | undefined, disposed = false, exited = false, retry = 300;
    let reconnect: ReturnType<typeof setTimeout> | undefined;
    const send = (value: unknown) => { if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(value)); };
    const resize = () => { const size = fit.proposeDimensions(); if (!size) return; const cols = Math.min(400, Math.max(2, size.cols)), rows = Math.min(200, Math.max(2, size.rows)); term.resize(cols, rows); send({ type: "resize", cols, rows }); };
    const connect = () => {
      if (disposed) return;
      setStatus("连接中");
      const url = socketURL(`/api/machines/${encodeURIComponent(machine)}/sessions/${encodeURIComponent(runtime.id)}/events`);

      socket = new WebSocket(url);
      socket.onopen = () => { retry = 300; setConnected(true); setStatus("已连接"); setError(""); term.reset(); resize(); term.focus(); };
      socket.onmessage = (event) => {
        try {
          const message = JSON.parse(event.data) as { type: string; data?: string; binary?: boolean; payload?: number; error?: string };
          if (message.type === "data" && message.data) term.write(message.binary ? Uint8Array.from(atob(message.data), (char) => char.charCodeAt(0)) : message.data);
          if (message.type === "exit") { exited = true; setStatus(`已退出 · ${message.payload} · 可浏览保留历史`); }
          if (message.type === "error") setError(message.error ?? "连接中断，未确认的输入不会自动重发。");
        } catch { setError("收到无效终端响应"); }
      };
      socket.onclose = () => {
        if (disposed) return;
        setConnected(false);
        if (exited) setStatus("已退出");
        else { setStatus("已断开，正在重连"); reconnect = setTimeout(connect, retry); retry = Math.min(retry * 2, 5000); }
      };
    };
    const input = term.onData((data) => send({ type: "input", data }));
    const binary = term.onBinary((data) => send({ type: "input", data: btoa(data), binary: true }));
    const observer = new ResizeObserver(resize); observer.observe(container.current);
    connect();
    return () => { disposed = true; clearTimeout(reconnect); observer.disconnect(); input.dispose(); binary.dispose(); socket?.close(); term.dispose(); terminal.current = undefined; };
  }, [machine, runtime.id]);
  const browse = async (action: "older" | "newer" | "close") => {
    try { await call(machine, "runtime.history", { action }, runtime); setError(""); terminal.current?.focus(); }
    catch (e) { setError(errorText(e)); }
  };
  return <div className="flex min-h-0 flex-1 flex-col overflow-hidden rounded-b-xl">
    <div className="flex flex-wrap items-center justify-between gap-2 border-b border-foreground/20 px-4 py-2 text-xs"><span role="status">{status}</span><div className="flex gap-1"><Button variant="ghost" size="sm" disabled={!connected} onClick={() => void browse("older")}><Icon icon={historyIcon} />历史 / 更早一页</Button><Button variant="ghost" size="sm" disabled={!connected} onClick={() => void browse("newer")}>更新一页</Button><Button variant="ghost" size="sm" disabled={!connected} onClick={() => void browse("close")}>返回终端</Button></div></div>
    <p className="bg-secondary px-4 py-2 text-xs">使用滚轮或 Page Up / Page Down 浏览历史，按 Esc 返回终端。</p>
    {error && <div className="error-box m-2" role="alert">{error}</div>}
    <div ref={container} className="terminal-container" aria-label="远端终端" />
  </div>;
}
