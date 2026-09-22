package fabricd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/pkg/api"
)

func elicit(a *acpController, id string, params map[string]any) {
	a.receive(api.Payload(map[string]any{"jsonrpc": "2.0", "id": id, "method": "elicitation/create", "params": params}))
}

func TestElicitationValidationAndOnceOnlyReservedResponse(t *testing.T) {
	a, requests, registry, key := reservedController(t)
	for _, id := range []string{"fill-one", "fill-two"} {
		key.SubmissionID = id
		_, _, err := registry.ClaimKey(t.Context(), key, [32]byte{}, "host")
		if err != nil && id == "fill-one" {
			t.Fatal(err)
		}
	}
	elicit(a, "question", map[string]any{"mode": "form", "sessionId": "session-a", "toolCallId": "tool-a", "message": "Choose settings", "requestedSchema": map[string]any{"properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": 1}, "enabled": map[string]any{"type": "boolean"}}, "required": []string{"count", "enabled"}}})
	live := a.snapshot().Elicitations
	if len(live) != 1 || live[0].FormError != "" {
		t.Fatal(live)
	}
	key.SubmissionID = "invalid"
	answer := api.ACPAction{Action: "elicitation", ElicitationID: live[0].ID, ElicitationResponse: &api.ACPElicitationResponse{Action: "accept", Content: json.RawMessage(`{"count":0,"enabled":false}`)}}
	if _, err := a.submit(t.Context(), registry, key, answer, "host"); err == nil {
		t.Fatal("invalid answer was accepted")
	}
	if len(a.snapshot().Elicitations) != 1 {
		t.Fatal("validation consumed request")
	}
	answer.ElicitationResponse.Content = json.RawMessage(`{"count":2,"enabled":false}`)
	key.SubmissionID = "answer"
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			receipt, err := a.submit(t.Context(), registry, key, answer, "host")
			if err != nil || receipt.Stage != "written" {
				t.Error(receipt, err)
			}
		})
	}
	group.Wait()
	response := takeRPC(t, requests)
	if response.ID != "question" || !strings.Contains(string(response.Result), `"count":2`) {
		t.Fatal(response)
	}
	select {
	case extra := <-requests:
		t.Fatal("duplicate write", extra)
	default:
	}
	if records := permissionHistory(t, a); len(records) != 1 || records[0].State != "responded" || records[0].Response != "" || records[0].ToolCallID != "tool-a" {
		t.Fatal(records)
	}
	if strings.Contains(string(redactElicitationResponse(api.Payload(map[string]any{"result": answer.ElicitationResponse}))), `"count"`) {
		t.Fatal("form answers entered diagnostics")
	}
}

func TestElicitationURLConsentAndCompletionAreSeparate(t *testing.T) {
	a, requests := queueFixture(t)
	params := map[string]any{"mode": "url", "sessionId": "session-a", "elicitationId": "opaque-id", "url": "https://example.com/connect", "message": "Connect account"}
	elicit(a, "url-request", params)
	id := a.snapshot().Elicitations[0].ID
	a.completeElicitation(json.RawMessage(`{"elicitationId":"opaque-id"}`))
	if len(a.snapshot().Elicitations) != 1 {
		t.Fatal("unsolicited completion consumed pending consent")
	}
	if _, err := a.action(api.ACPAction{Action: "elicitation", ElicitationID: id, ElicitationResponse: &api.ACPElicitationResponse{Action: "accept", Content: json.RawMessage(`{"ignored":"private"}`)}}); err != nil {
		t.Fatal(err)
	}
	response := takeRPC(t, requests)
	if string(response.Result) != `{"action":"accept"}` {
		t.Fatal(response)
	}
	if records := permissionHistory(t, a); len(records) != 1 || records[0].State != "awaiting_completion" {
		t.Fatal(records)
	}
	elicit(a, "duplicate-url", params)
	if response := takeRPC(t, requests); len(response.Error) == 0 {
		t.Fatal("outstanding URL ID reused", response)
	}
	a.completeElicitation(json.RawMessage(`{"elicitationId":"unknown"}`))
	a.completeElicitation(json.RawMessage(`{"elicitationId":"opaque-id"}`))
	a.completeElicitation(json.RawMessage(`{"elicitationId":"opaque-id"}`))
	if records := permissionHistory(t, a); len(records) != 1 || records[0].State != "completed" {
		t.Fatal(records)
	}
}

func TestElicitationURLCompletionRetentionDoesNotSpendPendingCapacity(t *testing.T) {
	a, requests := queueFixture(t)
	var firstID, lastID string
	for i := range maxACPElicitationCompletions + 1 {
		urlID := fmt.Sprintf("url-%d", i)
		elicit(a, urlID, map[string]any{"mode": "url", "sessionId": "session-a", "elicitationId": urlID, "url": "https://example.com/connect?token=" + strings.Repeat("x", 8*1024), "message": "Connect account", "toolCallId": urlID})
		live := a.snapshot().Elicitations
		if len(live) != 1 {
			t.Fatalf("URL request %d was not admitted after earlier answers: %+v", i, live)
		}
		if i == 0 {
			firstID = live[0].ID
		}
		lastID = live[0].ID
		if _, err := a.action(api.ACPAction{Action: "elicitation", ElicitationID: lastID, ElicitationResponse: &api.ACPElicitationResponse{Action: "accept"}}); err != nil {
			t.Fatal(err)
		}
		if response := takeRPC(t, requests); response.ID != urlID || string(response.Result) != `{"action":"accept"}` {
			t.Fatal(response)
		}
		if state := a.snapshot(); len(state.Elicitations) != 0 || state.Resources.Elicitations.Used != 0 {
			t.Fatal("answered URL consumed pending capacity", state)
		}
	}
	a.mu.Lock()
	retained := append([]acpElicitationCompletion(nil), a.elicitationCompletions...)
	a.mu.Unlock()
	if len(retained) != maxACPElicitationCompletions || retained[0].urlID != "url-1" {
		t.Fatal("completion retention did not evict the oldest correlation", retained)
	}
	elicit(a, "next-form", map[string]any{"mode": "form", "sessionId": "session-a", "message": "Choose settings", "requestedSchema": map[string]any{}})
	elicit(a, "next-url", map[string]any{"mode": "url", "sessionId": "session-a", "elicitationId": "next-url", "url": "https://example.com/connect", "message": "Connect account"})
	if live := a.snapshot().Elicitations; len(live) != 2 {
		t.Fatal("completion retention rejected a new form or URL", live)
	}
	a.completeElicitation(api.Payload(map[string]string{"elicitationId": "url-0"}))
	a.completeElicitation(api.Payload(map[string]string{"elicitationId": fmt.Sprintf("url-%d", maxACPElicitationCompletions)}))
	limit := api.MaxACPConversationLimit
	page, err := a.conversation.read(api.ACPConversationRead{ConversationID: a.snapshot().Conversation.ID, Limit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, entry := range page.Entries {
		if entry.Activity == nil || entry.Activity.UpdateType != "interaction" {
			continue
		}
		var record api.ACPInteractionRecord
		if err := json.Unmarshal(entry.Activity.Data, &record); err != nil {
			t.Fatal(err)
		}
		states[record.ID] = record.State
		if record.ID == lastID && (record.Title != "Connect account" || record.ToolCallID != fmt.Sprintf("url-%d", maxACPElicitationCompletions)) {
			t.Fatal("completion lost its original interaction metadata", record)
		}
	}
	if states[firstID] != "expired" || states[lastID] != "completed" {
		t.Fatal("completion was not correlated with its retained request", states)
	}
	if _, err := a.action(api.ACPAction{Action: "elicitation", ElicitationID: lastID, ElicitationResponse: &api.ACPElicitationResponse{Action: "accept"}}); err == nil {
		t.Fatal("completed URL accepted a second answer")
	}
}

func TestElicitationURLCompletionRetentionHasIndependentByteBudget(t *testing.T) {
	a, requests := queueFixture(t)
	for i := range 3 {
		urlID := fmt.Sprintf("large-url-%d", i)
		elicit(a, urlID, map[string]any{"mode": "url", "sessionId": "session-a", "elicitationId": urlID, "url": "https://example.com/connect", "message": strings.Repeat("x", 70*1024)})
		live := a.snapshot().Elicitations
		if len(live) != 1 {
			t.Fatalf("large URL request %d was rejected: %+v", i, live)
		}
		if _, err := a.action(api.ACPAction{Action: "elicitation", ElicitationID: live[0].ID, ElicitationResponse: &api.ACPElicitationResponse{Action: "accept"}}); err != nil {
			t.Fatal(err)
		}
		_ = takeRPC(t, requests)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.elicitationCompletions) != 1 || a.elicitationCompletions[0].urlID != "large-url-2" || a.elicitationCompletions[0].bytes() > acpElicitationCompletionBytes {
		t.Fatal("completion metadata exceeded its independent byte budget")
	}
}

func TestElicitationAcceptedURLCompletionDoesNotSuspendLifecycleTimeout(t *testing.T) {
	a, requests := queueFixture(t)
	a.mu.Lock()
	a.pending["parent"] = make(chan acpReply, 1)
	a.methods["parent"] = "initialize"
	a.mu.Unlock()
	elicit(a, "url-request", map[string]any{"mode": "url", "requestId": "parent", "elicitationId": "opaque-id", "url": "https://example.com/connect", "message": "Connect account"})
	id := a.snapshot().Elicitations[0].ID
	if _, err := a.action(api.ACPAction{Action: "elicitation", ElicitationID: id, ElicitationResponse: &api.ACPElicitationResponse{Action: "accept"}}); err != nil {
		t.Fatal(err)
	}
	_ = takeRPC(t, requests)
	a.mu.Lock()
	a.questionActivity["parent"] = time.Now().Add(-time.Minute)
	a.mu.Unlock()
	if remaining := a.elicitationWaitRemaining("parent", time.Second); remaining > 0 {
		t.Fatal("optional URL completion suspended the parent's timeout", remaining)
	}
	a.completeElicitation(json.RawMessage(`{"elicitationId":"opaque-id"}`))
	if remaining := a.elicitationWaitRemaining("parent", time.Second); remaining <= 0 || remaining > time.Second {
		t.Fatal("completion did not give the parent a fresh bounded interval", remaining)
	}
}

type elicitationWriteHook struct {
	write func([]byte) (int, error)
}

func (w elicitationWriteHook) Write(data []byte) (int, error) { return w.write(data) }
func (w elicitationWriteHook) Close() error                   { return nil }

func TestElicitationFastURLCompletionAndUnknownWrite(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("write_failure_%t", failWrite), func(t *testing.T) {
			a, _ := queueFixture(t)
			elicit(a, "url-request", map[string]any{"mode": "url", "sessionId": "session-a", "elicitationId": "opaque-id", "url": "https://example.com/connect", "message": "Connect account"})
			id := a.snapshot().Elicitations[0].ID
			writes := 0
			a.r.mu.Lock()
			a.r.p = &process.Process{Input: elicitationWriteHook{write: func(data []byte) (int, error) {
				writes++
				a.completeElicitation(json.RawMessage(`{"elicitationId":"opaque-id"}`))
				if failWrite {
					return 0, io.ErrClosedPipe
				}
				return len(data), nil
			}}}
			a.r.mu.Unlock()
			action := api.ACPAction{Action: "elicitation", ElicitationID: id, ElicitationResponse: &api.ACPElicitationResponse{Action: "accept"}}
			_, err := a.action(action)
			if (err != nil) != failWrite {
				t.Fatal(err)
			}
			a.completeElicitation(json.RawMessage(`{"elicitationId":"opaque-id"}`))
			want := "completed"
			if failWrite {
				want = "unknown"
			}
			if records := permissionHistory(t, a); len(records) != 1 || records[0].State != want {
				t.Fatal("fast completion overrode the write outcome", records)
			}
			a.mu.Lock()
			pending, retained := len(a.elicitations), len(a.elicitationCompletions)
			a.mu.Unlock()
			if pending != 0 || retained != 0 {
				t.Fatal("terminal response retained executable or completion state", pending, retained)
			}
			if _, err := a.action(action); err == nil || writes != 1 {
				t.Fatal("terminal response was replayed", err, writes)
			}
		})
	}
}

func TestElicitationURLCompletionExpiresWithControllerScope(t *testing.T) {
	for _, boundary := range []string{"cancel", "close", "load"} {
		t.Run(boundary, func(t *testing.T) {
			a, requests := queueFixture(t)
			var prompt queuedRPC
			if boundary == "cancel" {
				submitAction(t, a, api.ACPAction{Action: "prompt", Text: "work", ExpectedConversationID: a.snapshot().Conversation.ID})
				prompt = takeRPC(t, requests)
			}
			elicit(a, "url-request", map[string]any{"mode": "url", "sessionId": "session-a", "elicitationId": "opaque-id", "url": "https://example.com/connect", "message": "Connect account"})
			id := a.snapshot().Elicitations[0].ID
			if _, err := a.action(api.ACPAction{Action: "elicitation", ElicitationID: id, ElicitationResponse: &api.ACPElicitationResponse{Action: "accept"}}); err != nil {
				t.Fatal(err)
			}
			_ = takeRPC(t, requests)
			switch boundary {
			case "cancel":
				if _, err := a.action(api.ACPAction{Action: "cancel"}); err != nil {
					t.Fatal(err)
				}
				if request := takeRPC(t, requests); request.Method != "session/cancel" {
					t.Fatal("accepted URL was answered again during cancellation", request)
				}
				replyRPC(a, prompt, map[string]string{"stopReason": "cancelled"})
			case "close":
				a.closed()
			case "load":
				op := submitAction(t, a, api.ACPAction{Action: "load", SessionID: "session-b"})
				replyRPC(a, takeRPC(t, requests), map[string]any{})
				waitOperation(t, a, op)
			}
			a.mu.Lock()
			retained := len(a.elicitationCompletions)
			a.mu.Unlock()
			if retained != 0 {
				t.Fatal("controller boundary retained old URL completion correlation")
			}
			a.completeElicitation(json.RawMessage(`{"elicitationId":"opaque-id"}`))
			for _, record := range permissionHistory(t, a) {
				if record.State == "completed" {
					t.Fatal("late completion revived a cleared URL", record)
				}
			}
		})
	}
}

func TestElicitationScopeExpiryAndCancellation(t *testing.T) {
	a, requests := queueFixture(t)
	params := map[string]any{"mode": "form", "requestId": "parent", "message": "Connection question", "requestedSchema": map[string]any{}}
	elicit(a, "stale", params)
	if response := takeRPC(t, requests); len(response.Error) == 0 {
		t.Fatal("accepted unbound request scope")
	}
	a.mu.Lock()
	a.pending["parent"] = make(chan acpReply, 1)
	a.methods["parent"] = "initialize"
	a.mu.Unlock()
	elicit(a, "active", params)
	if len(a.snapshot().Elicitations) != 1 {
		t.Fatal("request scope was not exposed")
	}
	a.receive([]byte(`{"jsonrpc":"2.0","id":"parent","result":{}}`))
	if len(a.snapshot().Elicitations) != 0 {
		t.Fatal("ended parent remained executable")
	}
	if records := permissionHistory(t, a); records[0].State != "expired" {
		t.Fatal(records)
	}
	submitAction(t, a, api.ACPAction{Action: "prompt", Text: "work", ExpectedConversationID: a.snapshot().Conversation.ID})
	prompt := takeRPC(t, requests)
	delete(params, "requestId")
	params["sessionId"] = "session-a"
	elicit(a, "session-question", params)
	id := a.snapshot().Elicitations[0].ID
	if _, err := a.action(api.ACPAction{Action: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if response := takeRPC(t, requests); string(response.Result) != `{"action":"cancel"}` {
		t.Fatal(response)
	}
	_ = takeRPC(t, requests)
	replyRPC(a, prompt, map[string]string{"stopReason": "cancelled"})
	if _, err := a.action(api.ACPAction{Action: "elicitation", ElicitationID: id, ElicitationResponse: &api.ACPElicitationResponse{Action: "decline"}}); err == nil {
		t.Fatal("cancelled question revived")
	}
}

func TestElicitationFlatSchemaAndPrivacyBoundary(t *testing.T) {
	for _, test := range []struct {
		name, schema, content string
		valid                 bool
	}{
		{"null constraints", `{"properties":{"name":{"type":"string","minLength":null}},"required":null}`, `{"name":"Ada"}`, true},
		{"titled enum", `{"properties":{"color":{"type":"string","oneOf":[{"const":"blue","title":"Blue"}]}}}`, `{"color":"blue"}`, true},
		{"multiselect", `{"properties":{"colors":{"type":"array","items":{"anyOf":[{"const":"blue","title":"Blue"}]},"minItems":1,"maxItems":2}}}`, `{"colors":["blue"]}`, true},
		{"wrong enum", `{"properties":{"color":{"type":"string","enum":["blue"]}}}`, `{"color":"red"}`, false},
		{"required", `{"properties":{"n":{"type":"number"}},"required":["n"]}`, `{}`, false},
		{"format", `{"properties":{"date":{"type":"string","format":"date"}}}`, `{"date":"2026-02-30"}`, false},
		{"unknown field", `{"properties":{"value":{"type":"_custom"}}}`, `{"value":"x"}`, false},
		{"nested", `{"properties":{"value":{"type":"object"}}}`, `{"value":{}}`, false},
		{"credential", `{"properties":{"api_key":{"type":"string"}}}`, `{"api_key":"secret"}`, false},
		{"extra data", `{"properties":{}}`, `{"unexpected":"x"}`, false},
		{"remote ref", `{"properties":{"value":{"type":"string","$ref":"https://example.com"}}}`, `{"value":"x"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			schema, err := prepareElicitationSchema(json.RawMessage(test.schema))
			if err != nil {
				if test.valid {
					t.Fatal(err)
				}
				return
			}
			e := &acpElicitation{schema: schema, params: elicitationParams{Mode: "form", Schema: json.RawMessage(test.schema)}}
			err = validateElicitationResponse(e, &api.ACPElicitationResponse{Action: "accept", Content: json.RawMessage(test.content)})
			if (err == nil) != test.valid {
				t.Fatal(err)
			}
		})
	}
}

func TestElicitationDoesNotSpendLifecycleTimeoutWhileWaitingForUser(t *testing.T) {
	a, requests := queueFixture(t)
	result := make(chan error, 1)
	go func() { _, err := a.rpc("initialize", map[string]any{}, 100*time.Millisecond, nil); result <- err }()
	parent := takeRPC(t, requests)
	elicit(a, "connection-question", map[string]any{"mode": "form", "requestId": parent.ID, "message": "Choose a workspace", "requestedSchema": map[string]any{}})
	time.Sleep(250 * time.Millisecond)
	select {
	case err := <-result:
		t.Fatal("user wait spent RPC timeout", err)
	default:
	}
	id := a.snapshot().Elicitations[0].ID
	if _, err := a.action(api.ACPAction{Action: "elicitation", ElicitationID: id, ElicitationResponse: &api.ACPElicitationResponse{Action: "decline"}}); err != nil {
		t.Fatal(err)
	}
	_ = takeRPC(t, requests)
	replyRPC(a, parent, map[string]any{"protocolVersion": 1})
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.questionActivity) != 0 {
		t.Fatal("finished RPC retained its timeout state")
	}
}
