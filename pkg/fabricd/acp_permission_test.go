package fabricd

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func permissionHistory(t *testing.T, a *acpController) []api.ACPInteractionRecord {
	t.Helper()
	var records []api.ACPInteractionRecord
	for _, entry := range conversationPage(t, a.conversation).Entries {
		if entry.Activity == nil || entry.Activity.UpdateType != "interaction" {
			continue
		}
		var record api.ACPInteractionRecord
		if err := json.Unmarshal(entry.Activity.Data, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestPermissionHistoryRetainsOneReadOnlyDecision(t *testing.T) {
	a, requests := queueFixture(t)
	operation := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "work", ExpectedConversationID: a.snapshot().Conversation.ID})
	prompt := takeRPC(t, requests)
	a.receive([]byte(`{"jsonrpc":"2.0","id":"permission","method":"session/request_permission","params":{"sessionId":"session-a","toolCall":{"toolCallId":"tool-1","title":"Run tests"},"options":[{"optionId":"allow","name":"Allow once","kind":"allow_once"}]}}`))
	permission := a.snapshot().Permissions[0]
	if permission.ConversationID != a.snapshot().Conversation.ID || permission.TurnID != conversationTurnID(operation.Ref) {
		t.Fatal("permission is not bound to its original conversation and turn", permission)
	}
	if record := permissionHistory(t, a); len(record) != 1 || record[0].State != "pending" || record[0].ToolCallID != "tool-1" {
		t.Fatal(record)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := a.action(api.ACPAction{Action: "permission", PermissionID: permission.ID, OptionID: "allow"})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 || takeRPC(t, requests).ID != "permission" {
		t.Fatal("one pending request must produce one wire response")
	}
	replyRPC(a, prompt, map[string]string{"stopReason": "end_turn"})
	if len(a.snapshot().Permissions) != 0 {
		t.Fatal("responded permission is still executable")
	}
	if record := permissionHistory(t, a); len(record) != 1 || record[0].State != "responded" || record[0].Response != "Allow once" {
		t.Fatal("completed prompt erased or duplicated the decision", record)
	}
}

func TestPendingPermissionHistoryIsCancelledWithThePrompt(t *testing.T) {
	a, requests := queueFixture(t)
	submitAction(t, a, api.ACPAction{Action: "prompt", Text: "work", ExpectedConversationID: a.snapshot().Conversation.ID})
	prompt := takeRPC(t, requests)
	a.receive([]byte(`{"jsonrpc":"2.0","id":"permission","method":"session/request_permission","params":{"sessionId":"session-a","toolCall":{"title":"Run tests"},"options":[{"optionId":"allow"}]}}`))
	permission := a.snapshot().Permissions[0]
	if _, err := a.action(api.ACPAction{Action: "cancel"}); err != nil {
		t.Fatal(err)
	}
	_ = takeRPC(t, requests)
	_ = takeRPC(t, requests)
	replyRPC(a, prompt, map[string]string{"stopReason": "cancelled"})
	if record := permissionHistory(t, a); len(record) != 1 || record[0].State != "cancelled" {
		t.Fatal(record)
	}
	if _, err := a.action(api.ACPAction{Action: "permission", PermissionID: permission.ID, OptionID: "allow"}); err == nil {
		t.Fatal("cancelled request accepted another answer")
	}
}
