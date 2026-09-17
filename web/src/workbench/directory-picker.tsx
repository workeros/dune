import { useEffect, useRef, useState } from "react";
import { Icon } from "@iconify/react";
import folderIcon from "@iconify-icons/ri/folder-open-line";
import refreshIcon from "@iconify-icons/ri/refresh-line";
import { Button } from "../components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "../components/ui/dialog";
import { call, errorText, type Page, type Binding } from "../lib/api";
export function DirectoryPicker({ binding, path, open, onOpenChange, onSelect }: { binding: Binding; path: string; open: boolean; onOpenChange: (open: boolean) => void; onSelect: (path: string) => void }) {
  type DirectoryEntry = { name: string; is_dir: boolean };
  const [current, setCurrent] = useState(path), [dirs, setDirs] = useState<DirectoryEntry[]>([]), [cursor, setCursor] = useState(""), [busy, setBusy] = useState(false), [error, setError] = useState("");
  const generation = useRef(0);
  useEffect(() => { if (open) setCurrent(path); }, [open]);
  const load = async (after: string, epoch: number) => {
    setBusy(true); setError("");
    try {
      const page = await call<Page<DirectoryEntry>>(binding, "files", { action: "list_page", path: current, cursor: after });
      if (epoch !== generation.current) return;
      setDirs((old) => [...(after ? old : []), ...page.items.filter((entry) => entry.is_dir)]);
      setCursor(page.next_cursor ?? "");
    } catch (e) { if (epoch === generation.current) setError(errorText(e)); }
    finally { if (epoch === generation.current) setBusy(false); }
  };
  useEffect(() => {
    const epoch = ++generation.current;
    setDirs([]); setCursor("");
    if (open && current) void load("", epoch);
    return () => { generation.current++; };
  }, [open, current, binding]);
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent><DialogTitle className="mb-2 text-xl font-bold">选择项目目录</DialogTitle><DialogDescription className="muted mb-4">按名称逐页浏览当前开发机。目录变化后可重新加载。</DialogDescription><p className="mb-3 break-all font-mono text-sm">{current}</p><div className="mb-5 max-h-64 overflow-auto rounded border border-foreground/20"><button className="flex w-full gap-2 border-b border-foreground/10 p-3 text-left text-sm hover:bg-secondary" onClick={() => setCurrent(current.split("/").slice(0, -1).join("/") || "/")}><Icon icon={folderIcon} />.. 上一级</button>{dirs.map((dir) => <button key={dir.name} className="flex w-full gap-2 p-3 text-left text-sm hover:bg-secondary" onClick={() => setCurrent(`${current.replace(/\/$/, "")}/${dir.name}`)}><Icon icon={folderIcon} />{dir.name}</button>)}{cursor && <Button variant="ghost" disabled={busy} onClick={() => void load(cursor, generation.current)}>加载更多</Button>}{busy && <p role="status" className="p-3 text-sm">正在读取目录…</p>}</div>{error && <div className="error-box mb-4" role="alert">{error}</div>}<div className="flex gap-2"><Button variant="outline" disabled={busy} onClick={() => void load("", ++generation.current)}>重新加载</Button><Button disabled={!!error || busy} onClick={() => { onSelect(current); onOpenChange(false); }}>使用此目录</Button></div></DialogContent></Dialog>;
}

