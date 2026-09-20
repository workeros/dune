export type ModelDescription = {
 conversation_id: string; revision: string; phase: string; origin: string;
 open_outcome: string; open_error: { code: string; detail: string } | null;
 head_order: string; retained_from_order: string; retained_entry_count: number;
 prefix_evicted: boolean; content_omitted: boolean; context_incomplete: boolean;
 native_history_coverage: string; state?: Record<string, unknown>;
};
export type ModelEntry = {
 entry_id: string; order: string; entry_revision: string; type: string; turn_id?: string;
 content_omitted: boolean; context_incomplete: boolean;
 message?: { role: string; channel: string; status: string; content: unknown[]; tail?: unknown[] };
 tool?: { tool_call_id: string; status: string; status_reason?: string; fields: Record<string, unknown> };
 turn?: { state: string; stop_reason?: string; error?: { detail: string } };
 activity?: { update_type: string; data: unknown };
};
export type ModelPage = { conversation: ModelDescription; entries: ModelEntry[]; through_order: string; next_cursor?: string; has_more: boolean; range_evicted: boolean };
export type ModelGet = { conversation: ModelDescription; entries: ModelEntry[]; missing: { entry_id: string; reason: string }[]; unprocessed_entry_ids: string[] };
export type ModelChange = { conversation_id: string; previous_revision: string; revision: string; changed_entry_ids?: string[]; invalidates_all: boolean };
export type ModelCache = {
 conversation?: ModelDescription; entries: Record<string, ModelEntry>; dirty: Record<string, string>;
 notifiedThrough: string; latestRevision?: string; cursor?: string; pageLoaded: boolean;
 rangeEvicted: boolean; browserTruncated: boolean;
};
export const emptyModel: ModelCache = { entries: {}, dirty: {}, notifiedThrough: "0", pageLoaded: false, rangeEvicted: false, browserTruncated: false };
export function integer(value: string): bigint {
 if (typeof value !== "string" || !/^(0|[1-9][0-9]{0,19})$/.test(value)) throw new Error("无效的会话修订或位置");
 return BigInt(value);
}
const newer = (a: string, b: string) => integer(a) > integer(b);
const maximum = (a: string, b: string) => newer(a, b) ? a : b;
export const orderedEntries = (cache: ModelCache) => Object.values(cache.entries).sort((a, b) => integer(a.order) < integer(b.order) ? -1 : 1);
export type ModelAction =
 | { type: "select"; conversation: ModelDescription | null }
 | { type: "notify"; change: ModelChange }
 | { type: "page"; page: ModelPage; direction: "latest" | "older" }
 | { type: "get"; result: ModelGet };

export function modelReducer(cache: ModelCache, action: ModelAction): ModelCache {
 if (action.type === "select") {
  const description = action.conversation;
  if (!description) return emptyModel;
  integer(description.revision); integer(description.retained_from_order);
  if (cache.conversation?.conversation_id !== description.conversation_id) return { ...emptyModel, conversation: description, notifiedThrough: description.revision, latestRevision: description.revision };
  // State discovery updates metadata, never claims to have read entry bodies.
  return mergeSnapshot(cache, description, []);
 }
 if (action.type === "notify") {
  const next = action.change;
  if (next.conversation_id !== cache.conversation?.conversation_id) return cache;
  if (!newer(next.revision, cache.notifiedThrough)) return cache;
  let dirty = { ...cache.dirty };
  const invalidate = next.invalidates_all || newer(next.previous_revision, cache.notifiedThrough);
  const ids = invalidate ? Object.keys(cache.entries) : next.changed_entry_ids ?? [];
  for (const id of ids) dirty[id] = maximum(dirty[id] ?? "0", next.revision);
  let latestRevision = cache.latestRevision;
  if (invalidate || ids.length === 0) latestRevision = maximum(latestRevision ?? "0", next.revision);
  // Slow fetching cannot accumulate an unbounded list of IDs already evicted
  // by the server. Refresh the retained browser window and the recent page.
  if (Object.keys(dirty).length > 1200) {
   dirty = Object.fromEntries(Object.keys(cache.entries).map((id) => [id, next.revision]));
   latestRevision = next.revision;
  }
  return { ...cache, dirty, latestRevision, notifiedThrough: next.revision };
 }
 const response = action.type === "page" ? action.page : action.result;
 if (response.conversation.conversation_id !== cache.conversation?.conversation_id) return cache;
 let result = mergeSnapshot(cache, response.conversation, response.entries);
 const dirty = { ...result.dirty };
 const processed = response.entries.map((entry) => entry.entry_id);
 if (action.type === "get") {
  for (const missing of action.result.missing) {
   processed.push(missing.entry_id);
   const current = result.entries[missing.entry_id];
   if (current && !newer(current.entry_revision, response.conversation.revision)) delete result.entries[missing.entry_id];
  }
 }
 // A newer page only satisfies IDs actually included in that snapshot. Its
 // global revision cannot acknowledge a changed tool in a previously read page.
 for (const id of processed) if (dirty[id] && !newer(dirty[id], response.conversation.revision)) delete dirty[id];
 result = { ...result, dirty };
 if (action.type === "page") {
  if (action.direction === "older" || !cache.pageLoaded) result.cursor = action.page.has_more ? action.page.next_cursor : undefined;
  result.pageLoaded = true;
  result.rangeEvicted ||= action.page.range_evicted;
  if (action.direction === "latest" && result.latestRevision && !newer(result.latestRevision, response.conversation.revision)) result.latestRevision = undefined;
 }
 return result;
}

function mergeSnapshot(cache: ModelCache, description: ModelDescription, incoming: ModelEntry[]): ModelCache {
 const current = cache.conversation;
 const conversation = !current || !newer(current.revision, description.revision) ? description : current;
 const from = maximum(current?.retained_from_order ?? "1", description.retained_from_order);
 const entries = { ...cache.entries }, dirty = { ...cache.dirty };
 for (const entry of incoming) {
  integer(entry.entry_revision); integer(entry.order);
  if (integer(entry.order) < integer(from)) continue;
  const existing = entries[entry.entry_id];
  if (!existing || newer(entry.entry_revision, existing.entry_revision)) entries[entry.entry_id] = entry;
 }
 for (const [id, entry] of Object.entries(entries)) if (integer(entry.order) < integer(from)) { delete entries[id]; delete dirty[id]; }
 let bytes = 0, count = 0, browserTruncated = cache.browserTruncated;
 const sorted = Object.values(entries).sort((a, b) => integer(a.order) > integer(b.order) ? -1 : 1);
 for (const entry of sorted) {
  bytes += new TextEncoder().encode(JSON.stringify(entry)).length; count++;
  if (bytes > 4 * 1024 * 1024 || count > 1000) { delete entries[entry.entry_id]; delete dirty[entry.entry_id]; browserTruncated = true; }
 }
 return { ...cache, conversation, entries, dirty, browserTruncated };
}
