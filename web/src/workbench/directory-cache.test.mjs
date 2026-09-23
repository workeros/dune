import test from "node:test";
import assert from "node:assert/strict";
import { DirectoryCache } from "./directory-cache.ts";

function agent(revision, title = "自动标题", incarnation = "original") {
 const binding = { runner_id: "runner", fabric_id: "fabric", machine_id: "machine", revision: 1 };
 const runtime = { observation: { epoch: "connector", revision }, id: "runtime", incarnation, generation: 1, adapter: "acp", title: "default", session_metadata: { revision, conversation_id: "conversation", title } };
 return { agent_ref: `ref-${revision}`, target: { binding, runtime: { ...runtime } }, runtime };
}

test("final event wins delayed discovery above JavaScript safe integers, clear remains explicit", () => {
 const cache = new DirectoryCache(), batch = cache.begin("one");
 cache.apply(batch, { subscription_id: "one", kind: "member", agent: agent("9007199254740994") });
 cache.mergePage(batch, { items: [agent("9007199254740993", "older")] });
 assert.equal(cache.snapshot()[0].runtime.session_metadata.title, "自动标题");
 const absent = agent("9007199254740994");
 absent.runtime.observation.revision = "9007199254740995";
 absent.runtime.availability = "unavailable"; delete absent.runtime.session_metadata;
 cache.mergePage(batch, { items: [absent] });
 assert.equal(cache.snapshot()[0].runtime.session_metadata.revision, "9007199254740994");
 assert.equal(cache.snapshot()[0].agent_ref, "ref-9007199254740994");
 assert.equal(cache.snapshot()[0].runtime.availability, "unavailable");
 const cleared = agent("9007199254740995", null); cleared.runtime.observation.revision = "9007199254740996";
 cache.apply(batch, { subscription_id: "one", kind: "metadata", agent: cleared });
 assert.equal(cache.snapshot()[0].runtime.session_metadata.title, null);
});

for (const reason of ["removal", "replacement", "revocation"]) test(`${reason} invalidates already resolved pages and buffered events across resubscription`, () => {
 const cache = new DirectoryCache(), old = cache.begin("one");
 const held = agent("9007199254740999", "late higher title");
 cache.mergePage(old, { items: [held] });
 cache.apply(old, { subscription_id: "one", kind: "invalidated" });
 assert.equal(cache.mergePage(old, { items: [held] }), false);
 const current = cache.begin("two");
 cache.mergePage(current, { items: [agent("1", "replacement", "new")] });
 assert.equal(cache.apply(old, { subscription_id: "one", kind: "member", agent: held }), false);
 assert.equal(cache.mergePage(old, { items: [held] }), false);
 assert.equal(cache.apply(current, { subscription_id: "two", kind: "metadata", agent: held }), false);
 assert.equal(cache.snapshot().length, 1);
 assert.equal(cache.snapshot()[0].runtime.incarnation, "new");
});

for (const newerFromPage of [false, true]) test(`full Runtime ordering with unchanged title: ${newerFromPage ? "late event" : "late page"}`, () => {
 const cache = new DirectoryCache(), batch = cache.begin("one");
 const pending = agent("9007199254740994"); pending.runtime.state = "running";
 const confirmed = structuredClone(pending);
 confirmed.runtime.observation.revision = "9007199254740995";
 confirmed.agent_ref = "confirmed-reference";
 confirmed.runtime.native_session = { id: "native", cwd: "/workspace" };
 const exited = structuredClone(confirmed);
 exited.runtime.observation.revision = "9007199254740996";
 Object.assign(exited.runtime, { state: "exited", exit_code: 0, stop_reason: "completed" });
 const page = value => cache.mergePage(batch, { items: [value] });
 const event = value => cache.apply(batch, { subscription_id: "one", kind: "member", agent: value });
 const accept = newerFromPage ? page : event, delayed = newerFromPage ? event : page;
 for (const value of [pending, confirmed, exited]) { assert.equal(accept(value), true); assert.deepEqual(cache.snapshot(), [value]); }
 for (const value of [pending, confirmed, exited]) { assert.equal(delayed(value), true); assert.deepEqual(cache.snapshot(), [exited]); }
});

test("observation epoch changes require a fresh subscription and fence old discovery", () => {
 const cache = new DirectoryCache(), batch = cache.begin("one"), old = agent("100");
 cache.mergePage(batch, { items: [old] });
 const current = structuredClone(old); current.runtime.observation = { epoch: "reconnected", revision: "1" };
 assert.equal(cache.apply(batch, { subscription_id: "one", kind: "member", agent: current }), false);
 assert.deepEqual(cache.snapshot(), []);
 const fresh = cache.begin("two");
 assert.equal(cache.mergePage(fresh, { items: [current] }), true);
 assert.equal(cache.mergePage(batch, { items: [old] }), false);
 assert.deepEqual(cache.snapshot(), [current]);
});
