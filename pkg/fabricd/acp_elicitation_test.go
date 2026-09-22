package fabricd

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

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
