import { useEffect, useRef, useState } from "react";
import { Icon } from "@iconify/react";
import refreshIcon from "@iconify-icons/ri/refresh-line";
import { Button } from "../components/ui/button";
import { call, errorText, type Binding } from "../lib/api";

export function Diff({ binding, cwd }: { binding: Binding; cwd: string }) {
  const [diff, setDiff] = useState(""), [error, setError] = useState(""), [staged, setStaged] = useState(false), [busy, setBusy] = useState(false);
  const generation = useRef(0);
  const load = async () => {
    const request = ++generation.current;
    setBusy(true); setError("");
    try {
      const result = await call<{ stdout: string; stderr: string; exit_code: number; truncated: boolean }>(binding, "git", { action: "diff", directory: cwd, staged });
      if (request !== generation.current) return;
      if (result.exit_code !== 0) throw new Error(result.stderr || "Git diff 失败");
      setDiff(result.stdout);
      if (result.truncated) setError("diff 超过显示上限，当前仅展示部分内容。");
    } catch (cause) { if (request === generation.current) setError(errorText(cause)); }
    finally { if (request === generation.current) setBusy(false); }
  };
  useEffect(() => { void load(); return () => { generation.current++; }; }, [binding, cwd, staged]);
  return <div className="flex min-h-0 flex-1 flex-col"><div className="flex justify-between gap-3 border-b border-foreground/20 p-3"><select aria-label="Diff 范围" value={staged ? "staged" : "working"} onChange={(e) => setStaged(e.target.value === "staged")}><option value="working">工作区修改</option><option value="staged">暂存区修改</option></select><Button variant="ghost" onClick={() => void load()} disabled={busy}><Icon icon={refreshIcon} />刷新</Button></div>{error && <div className="error-box m-3" role="alert">{error}</div>}<pre className="min-h-0 flex-1 overflow-auto p-4 text-xs leading-6">{busy ? "读取开发机 diff…" : diff ? diff.split("\n").map((line, i) => <div key={i} className={line.startsWith("+") ? "bg-mint/40 text-[#285437]" : line.startsWith("-") ? "bg-primary/20 text-destructive" : ""}>{line || " "}</div>) : !error && "当前范围没有 Git diff。未跟踪的新文件不会包含在 git diff 中。"}</pre></div>;
}
