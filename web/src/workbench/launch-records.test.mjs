import test, { beforeEach } from "node:test";
import assert from "node:assert/strict";
import { readLaunches, recordLaunchReceipt, removeLaunch, saveLaunch } from "./launch-records.ts";

const scope = { accountID: "owner", prefix: "/api/v1" };
const binding = { runner_id: "runner", fabric_id: "fabric", machine_id: "machine", revision: 1 };
const target = { owner_id: "owner", runner_id: "runner", fabric_id: "fabric", machine_id: "machine", binding_revision: 1 };
let stored;
beforeEach(() => {
  stored = new Map();
  globalThis.sessionStorage = { getItem: (key) => stored.get(key) ?? null, setItem: (key, value) => stored.set(key, value) };
  globalThis.window = new EventTarget();
});

test("64 unknown launches remain separately queryable; reads and other accounts cannot evict them", () => {
  for (let id = 0; id < 64; id++) saveLaunch(scope, binding, `launch-${id}`);
  assert.throws(() => saveLaunch(scope, binding, "overflow"), /已满/);
  assert.equal(readLaunches(scope).length, 64);
  assert.deepEqual(readLaunches({ ...scope, accountID: "another" }), []);
  saveLaunch({ ...scope, accountID: "another" }, binding, "separate");
  assert.equal(readLaunches(scope)[0].submission_id, "launch-0");
  removeLaunch(scope, "launch-0");
  saveLaunch(scope, binding, "replacement");
  assert.equal(readLaunches(scope).length, 64);
});

test("receipts must match original launch scope and persist no business content", () => {
  saveLaunch(scope, binding, "launch");
  const receipt = { submission_id: "launch", target, admission: "accepted", stage: "started", runtime: { env: { SECRET: "private" } }, worktree: { path: "/private" } };
  for (const wrong of [{ ...target, owner_id: "another" }, { ...target, binding_revision: 2 }, { ...target, runtime_id: "runtime" }]) {
    assert.throws(() => recordLaunchReceipt(scope, "launch", { ...receipt, target: wrong }), /身份不一致/);
  }
  recordLaunchReceipt(scope, "launch", receipt);
  assert.equal(readLaunches(scope)[0].admission, "accepted");
  assert.ok(![...stored.values()][0].includes("private"));
});

test("corrupt or unavailable storage does not get overwritten", () => {
  saveLaunch(scope, binding, "original");
  const key = [...stored.keys()][0];
  stored.set(key, '[{"submission_id":"broken"}]');
  assert.throws(() => saveLaunch(scope, binding, "next"), /损坏/);
  assert.equal(stored.get(key), '[{"submission_id":"broken"}]');
  stored.clear();
  sessionStorage.setItem = () => { throw new Error("quota exceeded"); };
  assert.throws(() => saveLaunch(scope, binding, "next"), /quota/);
  assert.equal(stored.size, 0);
});
