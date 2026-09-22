package fabricd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestACPConfigurationChangesUseConfirmedCompleteState(t *testing.T) {
	a, requests := queueFixture(t)
	initial := json.RawMessage(`{"configOptions":[{"id":"model","name":"Model","type":"select","currentValue":"a","options":[{"group":"recommended","name":"Recommended","options":[{"value":"a","name":"A"},{"value":"b","name":"B"}]}]},{"id":"verify","name":"Verify","type":"boolean","currentValue":true}],"modes":{"currentModeId":"ask","availableModes":[{"id":"ask","name":"Ask"}]}}`)
	a.mu.Lock()
	a.retainSessionConfigurationLocked(initial)
	a.mu.Unlock()
	change := api.ACPAction{Action: "set_config_option", ExpectedConversationID: a.snapshot().Conversation.ID, SessionID: "session-a", ConfigID: "model", ConfigValue: json.RawMessage(`"b"`)}
	operation := submitAction(t, a, change)
	rpc := takeRPC(t, requests)
	if rpc.Method != "session/set_config_option" || !strings.Contains(string(rpc.Params), `"value":"b"`) {
		t.Fatal(rpc)
	}
	if state := a.snapshot().Conversation.State["config_option_update"]; !strings.Contains(string(state), `"currentValue":"a"`) {
		t.Fatal("configuration changed before acknowledgement", string(state))
	}
	if _, err := a.action(change); err == nil {
		t.Fatal("concurrent configuration was accepted")
	}
	replyRPC(a, rpc, map[string]any{"configOptions": []any{map[string]any{"id": "verify", "name": "Verify", "type": "boolean", "currentValue": true}}})
	waitOperation(t, a, operation)
	if state := a.snapshot().Conversation.State["config_option_update"]; strings.Contains(string(state), `"model"`) {
		t.Fatal("complete response was merged instead of replaced")
	}
	change.ConfigID = "verify"
	change.ConfigValue = json.RawMessage(`false`)
	operation = submitAction(t, a, change)
	rpc = takeRPC(t, requests)
	if !strings.Contains(string(rpc.Params), `"type":"boolean"`) || !strings.Contains(string(rpc.Params), `"value":false`) {
		t.Fatal(rpc)
	}
	replyRPC(a, rpc, map[string]any{"configOptions": []any{}})
	waitOperation(t, a, operation)
	change.Action = "set_mode"
	change.ModeID = "ask"
	if _, err := a.action(change); err == nil {
		t.Fatal("empty config list incorrectly revived legacy modes")
	}
	change.ExpectedConversationID = "old"
	if _, err := a.action(change); err == nil {
		t.Fatal("configuration escaped conversation identity")
	}
}

func TestACPModeWithoutConfigOptionsAndStructuredPrompt(t *testing.T) {
	a, requests := queueFixture(t)
	a.mu.Lock()
	a.retainSessionConfigurationLocked(json.RawMessage(`{"modes":{"currentModeId":"ask","availableModes":[{"id":"ask","name":"Ask"},{"id":"code","name":"Code"}]}}`))
	a.state.PromptCapabilities = api.ACPPromptCapabilities{Image: true, EmbeddedContext: true}
	a.mu.Unlock()
	operation := submitAction(t, a, api.ACPAction{Action: "set_mode", ExpectedConversationID: a.snapshot().Conversation.ID, SessionID: "session-a", ModeID: "code"})
	rpc := takeRPC(t, requests)
	replyRPC(a, rpc, map[string]any{})
	waitOperation(t, a, operation)
	if state := a.snapshot().Conversation.State["current_mode_update"]; !strings.Contains(string(state), `"code"`) {
		t.Fatal(string(state))
	}
	attachments := []json.RawMessage{json.RawMessage(`{"type":"image","mimeType":"image/png","data":"eA=="}`), json.RawMessage(`{"type":"resource","resource":{"uri":"file:///tmp/a.go","text":"package main"}}`)}
	operation = submitAction(t, a, api.ACPAction{Action: "prompt", ExpectedConversationID: a.snapshot().Conversation.ID, Text: "/review", Attachments: attachments})
	rpc = takeRPC(t, requests)
	var params struct {
		Prompt []json.RawMessage `json:"prompt"`
	}
	if json.Unmarshal(rpc.Params, &params) != nil || len(params.Prompt) != 3 || string(params.Prompt[1]) != string(attachments[0]) {
		t.Fatal(rpc)
	}
	page := conversationPage(t, a.conversation)
	found := false
	for _, entry := range page.Entries {
		if entry.Message != nil && entry.Message.Role == "user" {
			found = true
			if len(entry.Message.Content) != 3 {
				t.Fatal(entry)
			}
		}
	}
	if !found {
		t.Fatal("structured prompt absent from retained user message")
	}
	replyRPC(a, rpc, map[string]string{"stopReason": "end_turn"})
	waitOperation(t, a, operation)
}

func TestACPPromptCapabilitiesAreCheckedBeforeAdmission(t *testing.T) {
	a, _ := queueFixture(t)
	for _, test := range []struct {
		name, block string
		valid       bool
	}{
		{"link is baseline", `{"type":"resource_link","uri":"file:///tmp/a.go","name":"a.go"}`, true},
		{"image absent", `{"type":"image","mimeType":"image/png","data":"eA=="}`, false},
		{"audio absent", `{"type":"audio","mimeType":"audio/wav","data":"eA=="}`, false},
		{"embedded absent", `{"type":"resource","resource":{"uri":"file:///tmp/a.go","text":"hello"}}`, false},
		{"unknown", `{"type":"future"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a.mu.Lock()
			defer a.mu.Unlock()
			err := a.validatePromptLocked(api.ACPAction{SessionID: "session-a", Attachments: []json.RawMessage{json.RawMessage(test.block)}})
			if (err == nil) != test.valid {
				t.Fatal(err)
			}
		})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.PromptCapabilities.Image = true
	if err := a.validatePromptLocked(api.ACPAction{SessionID: "session-a", Attachments: []json.RawMessage{json.RawMessage(`{"type":"image","mimeType":"image/png","data":"invalid"}`)}}); err == nil {
		t.Fatal("invalid base64 admitted")
	}
}
