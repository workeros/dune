import { useEffect, useRef, useState } from "react";
import { Button } from "../components/ui/button";
import { call, errorText, type Binding, type Page } from "../lib/api";

type Entry = { name: string; is_dir: boolean };
export function Files({ binding, cwd }: { binding: Binding; cwd: string }) {
  const [path, setPath] = useState(cwd), [entries, setEntries] = useState<Entry[]>([]), [cursor, setCursor] = useState("");
  const [file, setFile] = useState(""), [content, setContent] = useState(""), [error, setError] = useState(""), [busy, setBusy] = useState(false);
  const [truncated, setTruncated] = useState(false);
  const generation = useRef(0);
  const load = async (after = "") => {
    const request = ++generation.current;
    setBusy(true); setError("");
    try {
      const page = await call<Page<Entry>>(binding, "files", { action: "list_page", path, cursor: after });
      if (request !== generation.current) return;
      setEntries((old) => [...(after ? old : []), ...page.items]); setCursor(page.next_cursor ?? "");
    } catch (cause) { if (request === generation.current) setError(errorText(cause)); }
    finally { if (request === generation.current) setBusy(false); }
  };
  useEffect(() => { setFile(""); setContent(""); setEntries([]); setCursor(""); void load(); return () => { generation.current++; }; }, [binding, path]);
  const open = async (entry: Entry) => {
    const next = `${path.replace(/\/$/, "")}/${entry.name}`;
    if (entry.is_dir) { setPath(next); return; }
    const request = ++generation.current;
    setBusy(true); setError(""); setFile(next); setContent(""); setTruncated(false);
    try {
      const chunk = await call<{ data: string; eof: boolean }>(binding, "files", { action: "read", path: next, offset: 0, length: 32768 });
      if (request !== generation.current) return;
      const bytes = Uint8Array.from(atob(chunk.data), (char) => char.charCodeAt(0));
      if (bytes.includes(0)) throw new Error("这是二进制文件，请在终端中查看。");
      setContent(new TextDecoder().decode(bytes)); setTruncated(!chunk.eof);
    } catch (cause) { if (request === generation.current) setError(errorText(cause)); }
    finally { if (request === generation.current) setBusy(false); }
  };
  return <div className="file-review"><header><span className="font-mono" title={file || path}>{file || path}</span><Button size="sm" variant="ghost" disabled={busy} onClick={() => { if (file) setFile(""); else setPath(path.split("/").slice(0, -1).join("/") || "/"); }}>{file ? "返回目录" : "上一级"}</Button></header>
    {error && <p className="error-box m-2" role="alert">{error}</p>}{busy && <p className="muted p-2" role="status">读取中…</p>}
    {file ? <><pre>{content}</pre>{truncated && <p className="muted p-3">仅显示前 32 KiB，可在终端查看完整文件。</p>}</> : <nav aria-label="审阅文件">{entries.map((entry) => <button key={entry.name} disabled={busy} onClick={() => void open(entry)}>{entry.is_dir ? "▸ " : ""}{entry.name}</button>)}{cursor && <Button size="sm" variant="ghost" disabled={busy} onClick={() => void load(cursor)}>加载更多</Button>}</nav>}
  </div>;
}
