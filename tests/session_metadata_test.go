package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func TestSessionMetadataDiscoveredWithoutAttachment(t *testing.T) {
	h := start(t)
	p := profile(h.dir, "acp", mockACPBinary(t))
	p.ManagedACP = true
	journal := filepath.Join(h.dir, "rpc.log")
	p.Env = map[string]string{"DUNE_MOCK_SESSION_TITLE": "  修复登录失败  ", "DUNE_MOCK_RPC_LOG": journal}
	runtime, stream, err := testStartProfile(h.client, h.ctx, p)
	must(t, err)
	stream.Close()
	defer testStopRuntime(h.client, h.ctx, runtime)
	waitManagedACPReady(t, h, runtime)
	operation, err := testACPSubmit(h.client, h.ctx, runtime, api.ACPAction{Action: "new", Cwd: h.dir})
	must(t, err)
	_, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 3000})
	must(t, err)
	var discovered api.Runtime
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		page, err := h.client.List(h.ctx)
		must(t, err)
		for _, item := range page.Items {
			if item.ID == runtime.ID {
				discovered = item
			}
		}
		if discovered.SessionMetadata != nil && discovered.SessionMetadata.Title != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("title absent from discovery", discovered)
		}
	}
	if *discovered.SessionMetadata.Title != "修复登录失败" || discovered.Title != runtime.Title {
		t.Fatal("title normalization changed launch name", discovered)
	}
	before, err := os.ReadFile(journal)
	must(t, err)
	h.client.Close()
	h.reconnect()
	current, err := h.client.Get(h.ctx, runtime)
	must(t, err)
	state, err := h.client.ACPState(h.ctx, runtime)
	must(t, err)
	page, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: discovered.ConversationID})
	must(t, err)
	expected := string(api.Payload(discovered.SessionMetadata))
	for _, metadata := range []any{current.SessionMetadata, state.SessionMetadata, state.Conversation.SessionMetadata, page.Conversation.SessionMetadata} {
		if string(api.Payload(metadata)) != expected {
			t.Fatalf("read contracts disagree: %s != %s", api.Payload(metadata), expected)
		}
	}
	after, err := os.ReadFile(journal)
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("metadata reads or reconnect dispatched native RPCs", string(after))
	}
}
