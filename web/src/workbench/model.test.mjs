import test from "node:test";
import assert from "node:assert/strict";
import { addPane, emptyView, geometry, leaves, removePane } from "./model.ts";

const pane = (id) => ({ target: { binding: { runner_id: id, fabric_id: "attached", machine_id: id, revision: 1 }, runtime: { id, incarnation: "boot", generation: 1, adapter: "pty" } } });
test("splitting and removing preserve existing leaf references and pinned review", () => {
  let sequence = 0; const id = () => String(++sequence);
  let view = addPane(emptyView, pane("one"), "horizontal", id);
  const original = view.root;
  view.review_pane = original.id;
  view = addPane(view, pane("two"), "vertical", id);
  assert.equal(leaves(view.root)[0], original);
  view = addPane(view, pane("one"), "horizontal", id);
  assert.equal(leaves(view.root).length, 2); assert.equal(view.focus_pane, original.id);
  assert.deepEqual(geometry(view.root).panes.map(({ rect }) => rect.height), [50, 50]);
  view = removePane(view, leaves(view.root)[1].id);
  assert.equal(view.root, original); assert.equal(view.review_pane, original.id);
  view = removePane(view, original.id);
  assert.equal(view.root, null); assert.equal(view.review_pane, undefined);
});
