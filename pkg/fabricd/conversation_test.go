package fabricd

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/aiomni/dune/pkg/api"
)

func conversationFixture(t *testing.T) *conversationSlot {
	t.Helper()
	slot := newConversationStore().register("runtime-test", "incarnation-test", nil)
	slot.begin(api.ACPAction{Action: "new", Cwd: "/work"})
	slot.opened("session-test", "/work", "succeeded", nil)
	return slot
}

func conversationUpdate(t *testing.T, slot *conversationSlot, turn string, value string) {
	t.Helper()
	var update map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &update); err != nil {
		t.Fatal(err)
	}
	slot.update(update, turn)
}

func conversationPage(t *testing.T, slot *conversationSlot) api.ACPConversationPage {
	t.Helper()
	page, err := slot.read(api.ACPConversationRead{ConversationID: slot.describe().ID})
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestConversationProtocolMergingAndImmutableSnapshots(t *testing.T) {
	slot := conversationFixture(t)
	slot.startTurn("operation-1", promptContent(api.ACPAction{Text: "question"}))
	conversationUpdate(t, slot, "turn-operation-1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hel"}}`)
	before := conversationPage(t, slot)
	encodedBefore := string(api.Payload(before))
	conversationUpdate(t, slot, "turn-operation-1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"lo"}}`)
	conversationUpdate(t, slot, "turn-operation-1", `{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}`)
	conversationUpdate(t, slot, "turn-operation-1", `{"sessionUpdate":"agent_message_chunk","content":{"type":"image","mimeType":"image/png","data":"AQID"}}`)
	page := conversationPage(t, slot)
	if len(page.Entries) != 5 {
		t.Fatalf("lost message/channel boundary: %+v", page.Entries)
	}
	message := page.Entries[2]
	if rawMessageText(message.Message.Content[0]) != "hello" || message.ID != before.Entries[2].ID || message.Revision <= before.Entries[2].Revision {
		t.Fatalf("incorrect chunk merge: %+v", message)
	}
	if page.Entries[3].Message.Channel != "thought" || !strings.Contains(string(page.Entries[4].Message.Content[0]), "image/png") {
		t.Fatal("thought or non-text block lost")
	}
	if string(api.Payload(before)) != encodedBefore {
		t.Fatal("published snapshot mutated")
	}
	slot.finishTurn(api.AgentOperation{Ref: "operation-1", State: "completed", StopReason: "end_turn"})
	conversationUpdate(t, slot, "", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"later"}}`)
	if last := conversationPage(t, slot).Entries; last[len(last)-1].TurnID != "" {
		t.Fatal("late update was attributed to finished turn")
	}
}

func rawMessageText(content json.RawMessage) string {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(content, &fields)
	return rawString(fields, "text")
}

func TestConversationToolPatchPreservesNullAndUnknownFields(t *testing.T) {
	slot := conversationFixture(t)
	conversationUpdate(t, slot, "known-turn", `{"sessionUpdate":"tool_call_update","toolCallId":"tool-1","status":"in_progress","title":"working"}`)
	partial := conversationPage(t, slot).Entries[0]
	if !partial.ContextIncomplete {
		t.Fatal("orphan tool lacks missing-context marker")
	}
	conversationUpdate(t, slot, "known-turn", `{"sessionUpdate":"tool_call","toolCallId":"tool-1","rawInput":{"query":"value"},"vendor":{"custom":1}}`)
	conversationUpdate(t, slot, "known-turn", `{"sessionUpdate":"tool_call_update","toolCallId":"tool-1","title":null,"rawOutput":[],"status":"completed"}`)
	entry := conversationPage(t, slot).Entries[0]
	if entry.ID != partial.ID || entry.Tool.Status != "completed" || entry.ContextIncomplete {
		t.Fatalf("tool identity/state lost: %+v", entry)
	}
	for key, want := range map[string]string{"title": "null", "rawOutput": "[]", "rawInput": `{"query":"value"}`, "vendor": `{"custom":1}`} {
		if string(entry.Tool.Fields[key]) != want {
			t.Fatalf("field %s: %s, want %s", key, entry.Tool.Fields[key], want)
		}
	}
}

func TestConversationPagingEvictionAndRecreatedTool(t *testing.T) {
	slot := conversationFixture(t)
	slot.store.maxEntries = 3
	conversationUpdate(t, slot, "", `{"sessionUpdate":"tool_call","toolCallId":"tool-1","title":"original"}`)
	for i := 0; i < 2; i++ {
		conversationUpdate(t, slot, "", `{"sessionUpdate":"vendor_event","value":1}`)
	}
	old := conversationPage(t, slot)
	one := 1
	latest, err := slot.read(api.ACPConversationRead{ConversationID: old.Conversation.ID, Limit: &one})
	if err != nil {
		t.Fatal(err)
	}
	conversationUpdate(t, slot, "", `{"sessionUpdate":"vendor_event","value":2}`)
	older, err := slot.read(api.ACPConversationRead{Cursor: latest.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if older.ThroughOrder != 3 || len(older.Entries) != 1 || older.Entries[0].Order != 2 || !older.RangeEvicted || older.HasMore {
		t.Fatalf("bad partial page: %+v", older)
	}
	conversationUpdate(t, slot, "", `{"sessionUpdate":"tool_call_update","toolCallId":"tool-1","status":"completed"}`)
	current := conversationPage(t, slot)
	tool := current.Entries[len(current.Entries)-1]
	if tool.ID == old.Entries[0].ID || !tool.ContextIncomplete || tool.Tool.Fields["title"] != nil {
		t.Fatal("evicted tool resurrected with false context")
	}
	get, err := slot.get(api.ACPConversationGet{ConversationID: old.Conversation.ID, EntryIDs: []string{old.Entries[0].ID, "e-999", tool.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(get.Missing) != 2 || get.Missing[0].Reason != "evicted" || get.Missing[1].Reason != "not_found" || len(get.Entries) != 1 {
		t.Fatalf("missing reasons: %+v", get)
	}
	for i := 0; i < 3; i++ {
		conversationUpdate(t, slot, "", `{"sessionUpdate":"vendor_event"}`)
	}
	empty, err := slot.read(api.ACPConversationRead{Cursor: latest.NextCursor})
	if err != nil || len(empty.Entries) != 0 || !empty.RangeEvicted || empty.HasMore {
		t.Fatalf("fully evicted page: %+v %v", empty, err)
	}
}

func TestConversationOversizedTextKeepsBoundedUpdatingTail(t *testing.T) {
	slot := conversationFixture(t)
	text := "PREFIX" + strings.Repeat("\x00汉😀<&", 50000) + "TAIL"
	update := map[string]json.RawMessage{"sessionUpdate": api.Payload("agent_message_chunk"), "messageId": api.Payload("message-1"), "content": api.Payload(map[string]string{"type": "text", "text": text})}
	slot.update(update, "")
	first := conversationPage(t, slot).Entries[0]
	if !first.ContentOmitted || len(first.Message.Tail) == 0 || !strings.HasPrefix(rawMessageText(first.Message.Content[0]), "PREFIX") || !strings.HasSuffix(rawMessageText(first.Message.Tail[len(first.Message.Tail)-1]), "TAIL") {
		t.Fatal("oversized text lost prefix, tail or omission")
	}
	for i := 0; i < 100; i++ {
		update["content"] = api.Payload(map[string]string{"type": "text", "text": strings.Repeat("汉", 8192)})
		slot.update(update, "")
	}
	if len(slot.model.entries) != 1 || len(slot.model.index) != 1 {
		t.Fatal("continued oversized streaming accumulated entries or indexes")
	}
	update["content"] = api.Payload(map[string]string{"type": "text", "text": "-FINAL"})
	slot.update(update, "")
	last := conversationPage(t, slot).Entries[0]
	encoded := api.Payload(last)
	if last.ID != first.ID || len(encoded) > api.MaxACPEntryBytes || !utf8.Valid(encoded) || !json.Valid(encoded) || !strings.HasSuffix(rawMessageText(last.Message.Tail[len(last.Message.Tail)-1]), "-FINAL") {
		t.Fatal("truncated message stopped updating or exceeded encoded budget")
	}
}

func TestConversationByteLimitedReadAndGetProgress(t *testing.T) {
	slot := conversationFixture(t)
	for i := 0; i < 3; i++ {
		slot.update(map[string]json.RawMessage{"sessionUpdate": api.Payload("vendor_event"), "body": api.Payload(strings.Repeat("x", 190*1024))}, "")
	}
	page := conversationPage(t, slot)
	if len(page.Entries) != 2 || !page.HasMore || len(api.Payload(page)) > api.MaxACPConversationResponseBytes {
		t.Fatalf("byte-limited page: count=%d more=%v size=%d", len(page.Entries), page.HasMore, len(api.Payload(page)))
	}
	older, err := slot.read(api.ACPConversationRead{Cursor: page.NextCursor})
	if err != nil || len(older.Entries) != 1 || older.HasMore {
		t.Fatalf("no pagination progress: %+v %v", older, err)
	}
	get, err := slot.get(api.ACPConversationGet{ConversationID: page.Conversation.ID, EntryIDs: []string{"e-1", "e-2", "e-3", "e-1"}})
	if err != nil || len(get.Entries) != 2 || len(get.UnprocessedEntryIDs) != 1 || len(api.Payload(get)) > api.MaxACPConversationResponseBytes {
		t.Fatalf("get budget/progress: %+v %v", get, err)
	}
}

func TestConversationGlobalEvictionAndForgetRelease(t *testing.T) {
	store := newConversationStore()
	first := store.register("one", "inc", nil)
	second := store.register("two", "inc", nil)
	for _, slot := range []*conversationSlot{first, second} {
		slot.begin(api.ACPAction{Action: "new", Cwd: "/work"})
	}
	first.update(map[string]json.RawMessage{"sessionUpdate": api.Payload("vendor_event"), "body": api.Payload(strings.Repeat("x", 2000))}, "")
	first.exited()
	store.maxBytes = store.bytes + 100
	second.update(map[string]json.RawMessage{"sessionUpdate": api.Payload("vendor_event"), "body": api.Payload(strings.Repeat("x", 1000))}, "")
	if first.describe().RetainedEntryCount != 0 || second.describe().RetainedEntryCount != 1 || store.bytes > store.maxBytes {
		t.Fatal("global budget did not evict exited model first")
	}
	first.remove()
	second.remove()
	if store.bytes != 0 || len(store.slots) != 0 {
		t.Fatal("forgotten models leaked global accounting")
	}
}

func TestConversationReadSnapshotSurvivesConcurrentUpdates(t *testing.T) {
	slot := conversationFixture(t)
	update := map[string]json.RawMessage{"sessionUpdate": api.Payload("agent_message_chunk"), "messageId": api.Payload("stable"), "content": api.Payload(map[string]string{"type": "text", "text": "x"})}
	slot.update(update, "")
	page := conversationPage(t, slot)
	original := string(api.Payload(page))
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for i := 0; i < 100; i++ {
			slot.update(update, "")
		}
	}()
	go func() {
		defer group.Done()
		for i := 0; i < 100; i++ {
			if string(api.Payload(page)) != original {
				t.Error("snapshot mutated during encoding")
				return
			}
		}
	}()
	group.Wait()
}

func TestConversationOversizedToolAndMetadata(t *testing.T) {
	slot := conversationFixture(t)
	fields := map[string]json.RawMessage{"sessionUpdate": api.Payload("tool_call"), "toolCallId": api.Payload("tool-large"), "status": api.Payload(strings.Repeat("s", 300000))}
	for i := 0; i < 300; i++ {
		fields[strings.Repeat("k", i+1)] = api.Payload(map[string]string{"binary": strings.Repeat("<汉", 40000)})
	}
	slot.update(fields, "")
	entry := conversationPage(t, slot).Entries[0]
	if len(api.Payload(entry)) > api.MaxACPEntryBytes || !entry.ContentOmitted || len(entry.Tool.Fields) > 128 {
		t.Fatal("tool escaped its final representation budget")
	}
	slot.update(map[string]json.RawMessage{"sessionUpdate": api.Payload("tool_call_update"), "tool_call_id": api.Payload(strings.Repeat("x", 300000))}, "")
	for _, entry := range conversationPage(t, slot).Entries {
		if len(api.Payload(entry)) > api.MaxACPEntryBytes {
			t.Fatal("oversized alternate native ID escaped entry budget")
		}
	}
}

func TestConversationRejectsInvalidCursorsAndArguments(t *testing.T) {
	slot := conversationFixture(t)
	conversationUpdate(t, slot, "", `{"sessionUpdate":"vendor_event"}`)
	for _, limit := range []int{0, -1, 201} {
		if _, err := slot.read(api.ACPConversationRead{ConversationID: slot.describe().ID, Limit: &limit}); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	for _, value := range []string{`{"conversation_id":"c","limit":null}`, `{"limit":1.5}`, `{"limit":"50"}`} {
		var request api.ACPConversationRead
		if json.Unmarshal([]byte(value), &request) == nil {
			t.Fatalf("accepted malformed limit %s", value)
		}
	}
	cursor := conversationCursor{Runtime: slot.runtimeID, Incarnation: slot.incarnation, Conversation: slot.describe().ID, Through: 1, Before: 2}
	for _, change := range []func(*conversationCursor){func(c *conversationCursor) { c.Runtime = "other" }, func(c *conversationCursor) { c.Incarnation = "other" }, func(c *conversationCursor) { c.Through = 2 }, func(c *conversationCursor) { c.Before = 0 }, func(c *conversationCursor) { c.Before = 3 }} {
		bad := cursor
		change(&bad)
		if _, err := slot.read(api.ACPConversationRead{Cursor: encodeConversationCursor(bad)}); err == nil {
			t.Fatalf("accepted invalid cursor %+v", bad)
		}
	}
	for _, id := range []string{"bad", "e-0", "e-01", "e--1", "e-18446744073709551616"} {
		if _, err := slot.get(api.ACPConversationGet{ConversationID: slot.describe().ID, EntryIDs: []string{id}}); err == nil {
			t.Fatalf("accepted invalid entry ID %s", id)
		}
	}
}

func TestConversationLateToolTerminalAndMissingTurn(t *testing.T) {
	slot := conversationFixture(t)
	slot.startTurn("one", promptContent(api.ACPAction{Text: "question"}))
	conversationUpdate(t, slot, "turn-one", `{"sessionUpdate":"tool_call","toolCallId":"known","status":"in_progress","title":"work"}`)
	tool := conversationPage(t, slot).Entries[2]
	slot.finishTurn(api.AgentOperation{Ref: "one", State: "completed", StopReason: "end_turn"})
	finished := conversationPage(t, slot).Entries[2]
	if finished.Tool.Status != "unknown" || string(finished.Tool.Fields["status"]) != `"in_progress"` {
		t.Fatal("turn completion fabricated a successful tool")
	}
	conversationUpdate(t, slot, "", `{"sessionUpdate":"tool_call_update","toolCallId":"known","status":"completed","rawOutput":"late result"}`)
	late := conversationPage(t, slot).Entries[2]
	if late.ID != tool.ID || late.Tool.Status != "completed" || late.TurnID != "turn-one" {
		t.Fatal("late native update lost retained tool identity")
	}
	conversationUpdate(t, slot, "", `{"sessionUpdate":"tool_call","toolCallId":"without-turn"}`)
	entries := conversationPage(t, slot).Entries
	if !entries[len(entries)-1].ContextIncomplete {
		t.Fatal("missing native turn was presented as complete context")
	}
	conversationUpdate(t, slot, "", `{"sessionUpdate":"tool_call_update","toolCallId":"known","status":null}`)
	if conversationPage(t, slot).Entries[2].Tool.Status != "unknown" {
		t.Fatal("explicit null did not clear native tool status")
	}
}

func TestConversationOldSnapshotIsImmutableAcrossSwitch(t *testing.T) {
	slot := conversationFixture(t)
	conversationUpdate(t, slot, "", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"first generation"}}`)
	page := conversationPage(t, slot)
	get, err := slot.get(api.ACPConversationGet{ConversationID: page.Conversation.ID, EntryIDs: []string{page.Entries[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	encodedPage, encodedGet := string(api.Payload(page)), string(api.Payload(get))
	snapshotTaken := make(chan struct{})
	switched := make(chan struct{})
	go func() {
		close(snapshotTaken)
		slot.begin(api.ACPAction{Action: "load", SessionID: "session-test", Cwd: "/work"})
		close(switched)
	}()
	<-snapshotTaken
	<-switched
	if string(api.Payload(page)) != encodedPage || string(api.Payload(get)) != encodedGet {
		t.Fatal("in-flight snapshot adopted a newer generation")
	}
	if _, err := slot.read(api.ACPConversationRead{ConversationID: page.Conversation.ID}); err == nil {
		t.Fatal("old target still accepted after switch")
	}
	if _, err := slot.get(api.ACPConversationGet{ConversationID: page.Conversation.ID, EntryIDs: []string{page.Entries[0].ID}}); err == nil {
		t.Fatal("old get still accepted after switch")
	}
}

func TestConversationDoesNotDeduplicateEchoOrRepeatedInput(t *testing.T) {
	slot := conversationFixture(t)
	for _, operation := range []string{"first", "second"} {
		slot.startTurn(operation, promptContent(api.ACPAction{Text: "same text"}))
		conversationUpdate(t, slot, conversationTurnID(operation), `{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"same text"}}`)
		slot.finishTurn(api.AgentOperation{Ref: operation, State: "completed"})
	}
	page := conversationPage(t, slot)
	var userMessages []api.ACPEntry
	for _, entry := range page.Entries {
		if entry.Message != nil && entry.Message.Role == "user" {
			userMessages = append(userMessages, entry)
		}
	}
	if len(userMessages) != 4 {
		t.Fatalf("text equality deduplicated a distinct input or uncorrelated echo: %d", len(userMessages))
	}
	for _, entry := range userMessages {
		if rawMessageText(entry.Message.Content[0]) != "same text" {
			t.Fatal("distinct input was concatenated across source/turn boundary")
		}
	}
	if userMessages[0].TurnID == userMessages[2].TurnID {
		t.Fatal("repeated input lost its turn boundary")
	}
}

func TestConversationReplayAndLiveProtocolSemanticsMatch(t *testing.T) {
	// Identical fixed protocol facts go through the actual controller receive
	// path, with no active prompt. Load must ingest them before its result.
	facts := []string{
		`{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"question"}}`,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"hel"}}`,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"lo"}}`,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"resource_link","uri":"file:///work/result.txt","name":"result"}}`,
		`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Read","status":"in_progress","rawInput":{"path":"result.txt"}}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"t1","title":null,"status":"completed","rawOutput":{"ok":true}}`,
		`{"sessionUpdate":"vendor_event","value":{"meaning":"kept"}}`,
	}
	live, _ := queueFixture(t)
	replay, requests := queueFixture(t)
	load := submitAction(t, replay, api.ACPAction{Action: "load", SessionID: "session-a"})
	rpc := takeRPC(t, requests)
	for _, fact := range facts {
		update := api.Payload(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-a", "update": json.RawMessage(fact)}})
		live.receive(update)
		replay.receive(update)
	}
	pending := conversationPage(t, replay.conversation)
	if pending.Conversation.Phase != "loading" || pending.Conversation.OpenOutcome != "pending" || len(pending.Entries) != 4 {
		t.Fatal("load replay was not readable before response")
	}
	replyRPC(replay, rpc, map[string]any{})
	waitOperation(t, replay, load)
	a, b := conversationPage(t, live.conversation), conversationPage(t, replay.conversation)
	for i := range a.Entries {
		a.Entries[i].Revision = 0
		b.Entries[i].Revision = 0
	}
	if string(api.Payload(a.Entries)) != string(api.Payload(b.Entries)) {
		t.Fatal("replay and live protocol merging diverged")
	}
	if rawMessageText(b.Entries[1].Message.Content[0]) != "hello" || len(b.Entries[1].Message.Content) != 2 || b.Entries[2].Tool.Status != "completed" || string(b.Entries[2].Tool.Fields["title"]) != "null" || !b.Entries[2].ContextIncomplete || b.Entries[3].Activity.UpdateType != "vendor_event" {
		t.Fatal("fixed protocol facts were not preserved")
	}
}
