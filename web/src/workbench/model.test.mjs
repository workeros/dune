import test from "node:test";
import assert from "node:assert/strict";
import { addPane, emptyView, geometry, leaves, removePane, restorePane } from "./model.ts";

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

test("recovery preserves pane identity, keeps other connections and deduplicates an already open target", () => {
  const old = { id: "old", pane: { ...pane("old"), session_record_id: "native" } }, other = { id: "other", pane: pane("other") };
  const view = { root: { id: "split", direction: "horizontal", ratio: 0.6, children: [old, other] }, focus_pane: "other", review_pane: "old" };
  const agent = { target: pane("restored").target }, session = { id: "native", project_id: "project", directory_id: "directory" };
  const restored = restorePane(view, "old", agent, session);
  assert.equal(restored.root.children[1], other); assert.equal(restored.root.children[0].id, "old");
  assert.equal(restored.focus_pane, "other"); assert.equal(restored.review_pane, "old");
  assert.equal(restored.root.children[0].pane.target, agent.target);
  assert.equal(restorePane(view, "old", agent, { id: "another-session" }), view);
  const existing = { ...view, root: { ...view.root, children: [old, { id: "already-open", pane: { target: agent.target } }] }, focus_pane: "old" };
  const deduplicated = restorePane(existing, "old", agent, session);
  assert.equal(deduplicated.root.id, "already-open"); assert.equal(deduplicated.focus_pane, "already-open"); assert.equal(deduplicated.review_pane, "already-open");
});
