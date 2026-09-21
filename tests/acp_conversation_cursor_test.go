package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/sdk"
)

func TestACPConversationIndependentCursorsAcrossConnections(t *testing.T) {
	h := start(t)
	mock := mockACPBinary(t)
	p := profile(h.dir, "acp", mock)
	p.ManagedACP = true
	journal := filepath.Join(h.dir, "rpc.log")
	p.Env = map[string]string{"DUNE_MOCK_RPC_LOG": journal}
	runtime, initial, err := testStartProfile(h.client, h.ctx, p)
	must(t, err)
	initial.Close()
	waitManagedACPReady(t, h, runtime)
	submit := func(action api.ACPAction) api.AgentOperation {
		t.Helper()
		operation, err := testACPSubmit(h.client, h.ctx, runtime, action)
		must(t, err)
		result, err := h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 3000})
		must(t, err)
		if result.State != "completed" {
			t.Fatal("fixture operation did not complete", result)
		}
		return result
	}
	opened := submit(api.ACPAction{Action: "new"})
	for i := 0; i < 3; i++ {
		submit(api.ACPAction{Action: "prompt", ExpectedConversationID: opened.ConversationID, Text: fmt.Sprintf("turn-%d", i)})
	}
	two := 2
	latest, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: opened.ConversationID, Limit: &two})
	must(t, err)
	if latest.ThroughOrder != 9 || len(latest.Entries) != 2 || latest.Entries[0].Order != 8 || latest.Entries[1].Order != 9 || !latest.HasMore {
		t.Fatal("unexpected latest window", latest)
	}
	first := h.client
	defer first.Close()
	h.reconnect()
	second := h.client
	// Opening a new connection does not register or take ownership of a cursor.
	subscription, err := second.SubscribeACPConversation(h.ctx, runtime)
	must(t, err)
	defer subscription.Close()
	submit(api.ACPAction{Action: "prompt", ExpectedConversationID: opened.ConversationID, Text: "inserted after cursor"})
	stateBefore, err := first.ACPState(h.ctx, runtime)
	must(t, err)
	methodsBefore, err := os.ReadFile(journal)
	must(t, err)
	type response struct {
		page api.ACPConversationPage
		err  error
	}
	results := make(chan response, 6)
	var group sync.WaitGroup
	start := make(chan struct{})
	for _, client := range []*sdk.Client{first, second, first, second, first, second} {
		group.Go(func() {
			<-start
			page, err := client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{Cursor: latest.NextCursor, Limit: &two})
			results <- response{page, err}
		})
	}
	close(start)
	group.Wait()
	close(results)
	var encoded string
	for response := range results {
		must(t, response.err)
		page := response.page
		if page.ThroughOrder != 9 || page.Conversation.HeadOrder != 12 || len(page.Entries) != 2 || page.Entries[0].Order != 6 || page.Entries[1].Order != 7 {
			t.Fatal("appending changed an existing traversal", page)
		}
		value := string(api.Payload(page))
		if encoded != "" && value != encoded {
			t.Fatal("shared cursor was consumed or changed by another reader")
		}
		encoded = value
	}
	// One reader goes to the oldest page; the other can still reuse the first
	// cursor, with no server-side rewind or acknowledgement.
	orders := map[uint64]bool{8: true, 9: true}
	cursor := latest.NextCursor
	for cursor != "" {
		page, err := first.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{Cursor: cursor, Limit: &two})
		must(t, err)
		for _, entry := range page.Entries {
			if orders[entry.Order] || entry.Order > 9 {
				t.Fatal("duplicate or new member in old traversal", entry.Order)
			}
			orders[entry.Order] = true
		}
		if page.HasMore && (page.NextCursor == "" || page.NextCursor == cursor) {
			t.Fatal("cursor made no progress")
		}
		cursor = page.NextCursor
	}
	if len(orders) != 9 {
		t.Fatal("old traversal lost members", orders)
	}
	back, err := second.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{Cursor: latest.NextCursor, Limit: &two})
	must(t, err)
	if string(api.Payload(back)) != encoded {
		t.Fatal("another reader reaching the end invalidated the original cursor")
	}
	stateAfter, err := first.ACPState(h.ctx, runtime)
	must(t, err)
	if string(api.Payload(stateBefore.Conversation)) != string(api.Payload(stateAfter.Conversation)) {
		t.Fatal("reading changed the model revision or retention")
	}
	methodsAfter, err := os.ReadFile(journal)
	must(t, err)
	if string(methodsBefore) != string(methodsAfter) || strings.Count(string(methodsAfter), "session/prompt\n") != 4 {
		t.Fatal("cursor requests changed Agent control traffic")
	}
	// Returned entries retain real messages, not only expected order fields.
	var content map[string]string
	must(t, json.Unmarshal(latest.Entries[0].Message.Content[0], &content))
	if content["text"] != "turn-2" {
		t.Fatal("latest page lost the actual user content", content)
	}
}
