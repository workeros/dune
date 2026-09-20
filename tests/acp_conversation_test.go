package tests

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func TestACPConversationReadWithoutSubscription(t *testing.T) {
	h := start(t)
	mock := filepath.Join(h.dir, "mock-acp")
	output, err := exec.Command("go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput()
	if err != nil {
		t.Fatalf("mock build: %s %v", output, err)
	}
	p := profile(h.dir, "acp", mock)
	p.ManagedACP = true
	rpcLog := filepath.Join(h.dir, "rpc.log")
	p.Env = map[string]string{"DUNE_MOCK_HISTORY": "1", "DUNE_MOCK_RPC_LOG": rpcLog}
	runtime, initial, err := h.client.Start(h.ctx, p)
	must(t, err)
	initial.Close()
	defer h.client.Stop(h.ctx, runtime)
	for deadline := time.Now().Add(8 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		state, err := h.client.ACPState(h.ctx, runtime)
		must(t, err)
		if state.Ready {
			if state.Conversation != nil {
				t.Fatal("initialize fabricated a conversation")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ACP initialization timed out")
		}
	}
	submit := func(action api.ACPAction) api.AgentOperation {
		t.Helper()
		operation, err := h.client.ACPSubmit(h.ctx, runtime, action)
		must(t, err)
		operation, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 3000})
		must(t, err)
		if operation.State != "completed" {
			t.Fatalf("operation: %+v", operation)
		}
		return operation
	}
	submit(api.ACPAction{Action: "new", Cwd: h.dir})
	state, err := h.client.ACPState(h.ctx, runtime)
	must(t, err)
	if state.Conversation == nil || state.Conversation.OpenOutcome != "succeeded" {
		t.Fatalf("conversation missing: %+v", state)
	}
	id := state.Conversation.ID
	submit(api.ACPAction{Action: "prompt", Text: "offline conversation marker"})
	// The mock persists only actual prompt updates. Independent reads must not
	// change this transcript or dispatch another load/prompt/new request.
	transcript := filepath.Join(h.dir, ".dune-mock-acp-history.json")
	before, err := os.ReadFile(transcript)
	must(t, err)
	rpcsBefore, err := os.ReadFile(rpcLog)
	must(t, err)
	h.client.Close()
	h.reconnect()
	page, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: id})
	must(t, err)
	if page.Conversation.ID != id || len(page.Entries) != 3 {
		t.Fatalf("unexpected snapshot: %+v", page)
	}
	if page.Entries[0].Turn == nil || page.Entries[0].Turn.State != "completed" {
		t.Fatalf("turn did not settle: %+v", page.Entries[0])
	}
	if page.Entries[1].Message == nil || page.Entries[1].Message.Role != "user" || !strings.Contains(string(page.Entries[1].Message.Content[0]), "offline conversation marker") {
		t.Fatal("user input was not retained")
	}
	if page.Entries[2].Message == nil || page.Entries[2].Message.Role != "agent" || !strings.Contains(string(page.Entries[2].Message.Content[0]), "Mock response") {
		t.Fatal("Agent response was not retained")
	}
	entries, err := h.client.GetACPConversationEntries(h.ctx, runtime, api.ACPConversationGet{ConversationID: id, EntryIDs: []string{page.Entries[2].ID}})
	must(t, err)
	if len(entries.Entries) != 1 || string(api.Payload(entries.Entries[0])) != string(api.Payload(page.Entries[2])) {
		t.Fatal("get and read disagree on entry identity or value")
	}
	one := 1
	last, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: id, Limit: &one})
	must(t, err)
	if len(last.Entries) != 1 || last.Entries[0].ID != page.Entries[2].ID || !last.HasMore {
		t.Fatal("latest page did not preserve insertion order")
	}
	earlier, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{Cursor: last.NextCursor, Limit: &one})
	must(t, err)
	if len(earlier.Entries) != 1 || earlier.Entries[0].ID != page.Entries[1].ID || earlier.ThroughOrder != last.ThroughOrder {
		t.Fatal("cursor moved the pagination boundary")
	}
	after, err := os.ReadFile(transcript)
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("read issued an Agent control request")
	}
	rpcsAfter, err := os.ReadFile(rpcLog)
	must(t, err)
	if string(rpcsBefore) != string(rpcsAfter) {
		t.Fatalf("read dispatched ACP RPCs: before=%s after=%s", rpcsBefore, rpcsAfter)
	}
	// Native replay is ingested even though there is no active prompt.
	submit(api.ACPAction{Action: "load", SessionID: "mock-session", Cwd: h.dir})
	loaded, err := h.client.ACPState(h.ctx, runtime)
	must(t, err)
	if loaded.Conversation.ID == id {
		t.Fatal("load reused the previous generation")
	}
	replay, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: loaded.Conversation.ID})
	must(t, err)
	if len(replay.Entries) != 2 || replay.Conversation.NativeHistoryCoverage != "unknown" {
		t.Fatalf("replay missing or falsely complete: %+v", replay)
	}
	if _, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: id}); err == nil {
		t.Fatal("old generation remained readable")
	}
	var encoded map[string]json.RawMessage
	must(t, json.Unmarshal(api.Payload(replay.Conversation), &encoded))
	if len(encoded["revision"]) == 0 || encoded["revision"][0] != '"' {
		t.Fatal("revision must be a decimal JSON string")
	}
}
