export type UnknownRecord = Record<string, unknown>;
export type Permission = { id: string; params: { toolCall: { title?: string; [key: string]: unknown }; options: { optionId: string; name: string; kind: string }[] } };
export type State = { revision: number; ready: boolean; busy: string; session_id: string; cwd: string; can_list: boolean; can_load: boolean; error?: string; stop_reason?: string; permissions: Permission[]; list?: { sessions: { sessionId: string; cwd: string; title?: string }[]; nextCursor?: string } };
export type Update = { sessionUpdate?: string; session_update?: string; messageId?: string; message_id?: string; content?: unknown; title?: string; toolCallId?: string; tool_call_id?: string; status?: string; kind?: string; rawInput?: unknown; raw_input?: unknown; rawOutput?: unknown; raw_output?: unknown; _meta?: unknown; meta?: unknown };
export type MessageEntry = { type: "message"; key: string; role: "user" | "agent"; text: string; messageID?: string };
export type ToolEntry = { type: "tool"; key: string; id: string; title?: string; name?: string; kind?: string; status: "running" | "complete" | "error"; content?: string; input?: string; output?: string };
export type ActivityEntry = { type: "activity"; key: string; kind: string; value: unknown };
export type Entry = MessageEntry | ToolEntry | ActivityEntry;
export type StreamEntry = { bytes: number; key: number; direction: "input" | "output"; message: unknown; receivedAt: Date };
export type BrowserEvent = { type: string; payload?: unknown; error?: string; data?: string };

let entrySequence = 0;
export const nextEntryKey = (prefix: string) => `${prefix}-${++entrySequence}`;
const maxBrowserEntries = 5000;
const maxMergedMessageChars = 512 * 1024;

export const isRecord = (value: unknown): value is UnknownRecord => !!value && typeof value === "object" && !Array.isArray(value);
export const readString = (value: UnknownRecord, ...keys: string[]) => {
 for (const key of keys) if (typeof value[key] === "string" && value[key]) return value[key] as string;
 return undefined;
};
export const parseJSON = (value: string): unknown => { try { return JSON.parse(value) as unknown; } catch { return value; } };
const textFromContent = (value: unknown): string => {
 if (typeof value === "string") return value;
 if (Array.isArray(value)) return value.map(textFromContent).filter(Boolean).join("\n");
 if (!isRecord(value)) return "";
 if (value.type === "text" && typeof value.text === "string") return value.text;
 return "content" in value ? textFromContent(value.content) : "";
};
export const formatValue = (value: unknown): string => {
 if (typeof value === "string") {
  const parsed = parseJSON(value);
  return typeof parsed === "string" ? parsed : JSON.stringify(parsed, null, 2);
 }
 if (value === undefined || value === null) return "";
 try { return JSON.stringify(value, null, 2); } catch { return String(value); }
};
const toolStatus = (value?: string): ToolEntry["status"] => {
 const status = value?.toLowerCase();
 if (["failed", "failure", "error", "cancelled", "canceled"].includes(status ?? "")) return "error";
 if (["completed", "complete", "success", "succeeded"].includes(status ?? "")) return "complete";
 return "running";
};
const updateEntries = (entries: Entry[], update: Update): Entry[] => {
 const kind = update.sessionUpdate ?? update.session_update ?? "session_update";
 const messageID = update.messageId ?? update.message_id;
 if (["user_message_chunk", "agent_message_chunk", "user_message", "agent_message"].includes(kind)) {
  const text = textFromContent(update.content);
  if (!text) return entries;
  const role = kind.startsWith("user_") ? "user" : "agent";
  const chunk = kind.endsWith("_chunk");
  const last = entries.at(-1);
  if (chunk && last?.type === "message" && last.role === role && last.messageID === messageID && last.text.length + text.length <= maxMergedMessageChars) {
   return [...entries.slice(0, -1), { ...last, text: last.text + text }];
  }
  return [...entries, { type: "message", key: nextEntryKey("message"), role, text, messageID }];
 }
 if (kind === "tool_call" || kind === "tool_call_update") {
  const id = update.toolCallId ?? update.tool_call_id;
  if (!id) return [...entries, { type: "activity", key: nextEntryKey("activity"), kind, value: update }];
  const rawMeta = update._meta ?? update.meta;
  const meta = isRecord(rawMeta) ? rawMeta : {};
  const genius = isRecord(meta.genius) ? meta.genius : {};
  const name = readString(genius, "tool_name", "toolName");
  const description = readString(genius, "tool_description", "toolDescription");
  const next: Partial<ToolEntry> = {
   title: update.title,
   name,
   kind: update.kind,
   status: toolStatus(update.status),
   content: description || textFromContent(update.content),
   input: formatValue(update.rawInput ?? update.raw_input),
   output: formatValue(update.rawOutput ?? update.raw_output),
  };
  const index = entries.findIndex((entry) => entry.type === "tool" && entry.id === id);
  if (index >= 0) return entries.map((entry, entryIndex) => entryIndex === index && entry.type === "tool" ? {
   ...entry,
   title: next.title || entry.title,
   name: next.name || entry.name,
   kind: next.kind || entry.kind,
   status: update.status ? next.status! : entry.status,
   content: next.content || entry.content,
   input: next.input || entry.input,
   output: next.output || entry.output,
  } : entry);
  return [...entries, { type: "tool", key: `tool-${id}`, id, status: next.status!, title: next.title, name: next.name, kind: next.kind, content: next.content, input: next.input, output: next.output }];
 }
 return [...entries, { type: "activity", key: nextEntryKey("activity"), kind, value: update }];
};

type Conversation = {
 entries: Entry[]; sizes: number[]; bytes: number; gap: boolean;
 streamEntries: StreamEntry[]; streamBytes: number; streamOpen: boolean; sequence: number;
};
type ConversationAction = { type: "update"; update: Update } | { type: "notice"; value: unknown } | { type: "reset" } | { type: "gap" } | { type: "toggleStream" } | { type: "stream"; direction: "input" | "output"; message: unknown };
export const initialConversation: Conversation = { entries: [], sizes: [], bytes: 0, gap: true, streamEntries: [], streamBytes: 0, streamOpen: false, sequence: 0 };
const entryBytes = (value: unknown) => new TextEncoder().encode(formatValue(value)).length;
const maxConversationBytes = 1024 * 1024;

export function conversationReducer(state: Conversation, action: ConversationAction): Conversation {
 if (action.type === "gap") return { ...state, gap: true };
 if (action.type === "reset") return { ...state, entries: [], sizes: [], bytes: 0, gap: false };
 if (action.type === "toggleStream") return { ...state, streamOpen: !state.streamOpen, streamEntries: [], streamBytes: 0 };
 if (action.type === "stream") {
  if (!state.streamOpen) return state;
  const bytes = entryBytes(action.message);
  if (bytes > maxConversationBytes) return state;
  const streamEntries = [...state.streamEntries, { key: state.sequence + 1, bytes, direction: action.direction, message: action.message, receivedAt: new Date() }];
  let total = state.streamBytes + bytes, from = 0;
  while (total > maxConversationBytes || streamEntries.length - from > maxBrowserEntries) total -= streamEntries[from++].bytes;
  return { ...state, streamEntries: from ? streamEntries.slice(from) : streamEntries, streamBytes: total, sequence: state.sequence + 1 };
 }
 const entries: Entry[] = action.type === "update" ? updateEntries(state.entries, action.update) : [...state.entries, { type: "activity", key: nextEntryKey("notice"), kind: "Dune · 大型 ACP 内容已省略", value: action.value }];
 if (entries === state.entries) return state;
 let total = state.bytes, from = 0;
 // Unchanged entries keep their measured size. Only the updated tool/message
 // or new entry is serialized, even when the conversation contains 5000 items.
 const sizes = entries.map((entry, index) => {
  if (entry === state.entries[index]) return state.sizes[index];
  const size = entryBytes(entry);
  total += size - (state.sizes[index] ?? 0);
  return size;
 });
 while (total > maxConversationBytes || entries.length - from > maxBrowserEntries) total -= sizes[from++];
 return { ...state, entries: from ? entries.slice(from) : entries, sizes: from ? sizes.slice(from) : sizes, bytes: total, gap: state.gap || from > 0 || action.type === "notice" };
}
