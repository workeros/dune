import { useRef, type ReactNode, type PointerEvent } from "react";
import { geometry, type LayoutNode, type Leaf, type Rect, type Split } from "./model";

export function SplitCanvas({ root, focus, onFocus, onRatio, render }: { root: LayoutNode | null; focus?: string; onFocus: (id: string) => void; onRatio: (id: string, ratio: number) => void; render: (leaf: Leaf) => ReactNode }) {
  const container = useRef<HTMLDivElement>(null), drag = useRef<{ split: Split; rect: Rect } | undefined>(undefined);
  const { panes, dividers } = geometry(root);
  const move = (event: PointerEvent) => {
    const current = drag.current, bounds = container.current?.getBoundingClientRect();
    if (!current || !bounds) return;
    const horizontal = current.split.direction === "horizontal";
    const position = horizontal ? (event.clientX - bounds.left) / bounds.width * 100 : (event.clientY - bounds.top) / bounds.height * 100;
    const ratio = (position - (horizontal ? current.rect.x : current.rect.y)) / (horizontal ? current.rect.width : current.rect.height);
    onRatio(current.split.id, Math.max(0.1, Math.min(0.9, ratio)));
  };
  return <div ref={container} className="split-canvas" aria-label="并行会话" onPointerMove={move} onPointerUp={() => { drag.current = undefined; }} onPointerCancel={() => { drag.current = undefined; }}>
    {panes.map(({ leaf, rect }) => <section key={leaf.id} className={`agent-pane ${focus === leaf.id ? "is-focused" : ""}`} style={{ left: `${rect.x}%`, top: `${rect.y}%`, width: `${rect.width}%`, height: `${rect.height}%` }} data-pane-id={leaf.id} onClick={(event) => { if (!(event.target instanceof Element) || !event.target.closest("button")) onFocus(leaf.id); }} onFocusCapture={(event) => { if (!(event.target instanceof Element) || !event.target.closest("button")) onFocus(leaf.id); }}>{render(leaf)}</section>)}
    {dividers.map(({ split, rect }) => {
      const horizontal = split.direction === "horizontal";
      return <div key={split.id} className={`split-handle ${horizontal ? "is-vertical" : "is-horizontal"}`} role="separator" tabIndex={0} aria-label="调整分屏比例" aria-orientation={horizontal ? "vertical" : "horizontal"} aria-valuenow={Math.round(split.ratio * 100)} aria-valuemin={10} aria-valuemax={90}
        style={horizontal ? { left: `${rect.x + rect.width * split.ratio}%`, top: `${rect.y}%`, height: `${rect.height}%` } : { top: `${rect.y + rect.height * split.ratio}%`, left: `${rect.x}%`, width: `${rect.width}%` }}
        onPointerDown={(event) => { drag.current = { split, rect }; event.currentTarget.setPointerCapture(event.pointerId); event.preventDefault(); }}
        onKeyDown={(event) => { const delta = horizontal ? { ArrowLeft: -0.05, ArrowRight: 0.05 } : { ArrowUp: -0.05, ArrowDown: 0.05 }; if (event.key in delta) { event.preventDefault(); onRatio(split.id, Math.max(0.1, Math.min(0.9, split.ratio + delta[event.key as keyof typeof delta]!))); } }} />;
    })}
  </div>;
}
