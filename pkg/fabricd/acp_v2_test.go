package fabricd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func v2QueueFixture(t *testing.T) (*acpController, <-chan queuedRPC) {
	a, requests := queueFixture(t)
	a.v2Draft, a.state.ProtocolVersion, a.state.CanLoad, a.state.CanResume = true, 2, false, true
	a.conversation.mutate(func(m *conversationModel) { m.description.ProtocolVersion = 2 })
	return a, requests
}

func emitV2(a *acpController, update string) {
	a.receive(api.Payload(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": a.snapshot().SessionID, "update": json.RawMessage(update)}}))
}

func TestACPV2NegotiationAndCapabilities(t *testing.T) {
	const result = `{"protocolVersion":2,"info":{"name":"test","version":"1"},"capabilities":{"session":{"prompt":{"image":{},"audio":null},"mcp":{"stdio":{}}}}}`
	if _, err := negotiateACP([]byte(result), false); err == nil {
		t.Fatal("draft was enabled implicitly")
	}
	value, err := negotiateACP([]byte(result), true)
	if err != nil || value.load || !value.resume || !value.list || !value.stdio || value.http || !value.prompt.Image || value.prompt.Audio {
		t.Fatalf("capabilities: %+v %v", value, err)
	}
	for _, invalid := range []string{
		`{"protocolVersion":2,"info":{},"capabilities":{}}`,
		`{"protocolVersion":2,"info":{},"capabilities":{"session":null}}`,
		`{"protocolVersion":3,"info":{},"capabilities":{"session":{}}}`,
	} {
		if _, err := negotiateACP([]byte(invalid), true); err == nil {
			t.Fatal("accepted invalid session surface", invalid)
		}
	}
	a, _ := v2QueueFixture(t)
	params := a.initializationParams()
	if params["protocolVersion"] != 2 || params["info"] == nil || params["capabilities"] == nil || params["clientInfo"] != nil {
		t.Fatal(params)
	}
}

func TestACPV2InsertionAndIdleAreIndependent(t *testing.T) {
	for _, first := range []string{"response", "message", "idle"} {
		t.Run(first, func(t *testing.T) {
			a, requests := v2QueueFixture(t)
			request := api.ACPAction{Action: "prompt", Text: "identical", ExpectedConversationID: a.snapshot().Conversation.ID}
			one := submitAction(t, a, request)
			rpc := takeRPC(t, requests)
			two := submitAction(t, a, request)
			message := func() {
				emitV2(a, `{"sessionUpdate":"user_message_chunk","messageId":"u1","content":{"type":"text","text":"identical"}}`)
			}
			ack := func() { replyRPC(a, rpc, map[string]string{"messageId": "u1"}) }
			idle := func() { emitV2(a, `{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`) }
			if first == "message" {
				message()
			}
			if first == "idle" {
				idle()
			} else {
				ack()
			}
			if a.snapshot().OperationRef != one.Ref || readOperation(t, a, two).State != "pending" {
				t.Fatal("queue advanced without both insertion and idle")
			}
			if first != "message" {
				message()
			}
			if first == "idle" {
				ack()
			} else {
				idle()
			}
			if got := waitOperation(t, a, one); got.State != "completed" {
				t.Fatal(got)
			}
			rpc2 := takeRPC(t, requests)
			replyRPC(a, rpc2, map[string]string{"messageId": "u2"})
			emitV2(a, `{"sessionUpdate":"user_message","messageId":"u2","content":[{"type":"text","text":"identical"}]}`)
			emitV2(a, `{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`)
			messages := 0
			for _, entry := range conversationPage(t, a.conversation).Entries {
				if entry.Message == nil {
					continue
				}
				messages++
				if len(entry.Message.Content) != 1 || rawMessageText(entry.Message.Content[0]) != "identical" || entry.Message.OperationRef == "" {
					t.Fatalf("unreconciled message: %+v", entry.Message)
				}
			}
			if messages != 2 {
				t.Fatalf("expected two distinct insertions, got %d", messages)
			}
		})
	}
}

func TestACPV2UpsertsAndLateToolUpdates(t *testing.T) {
	a, requests := v2QueueFixture(t)
	one := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "inspect", ExpectedConversationID: a.snapshot().Conversation.ID})
	rpc := takeRPC(t, requests)
	replyRPC(a, rpc, map[string]string{"messageId": "u"})
	emitV2(a, `{"sessionUpdate":"agent_message","messageId":"a","content":[{"type":"text","text":"A"}]}`)
	emitV2(a, `{"sessionUpdate":"agent_message_chunk","messageId":"a","content":{"type":"text","text":"B"}}`)
	emitV2(a, `{"sessionUpdate":"agent_message","messageId":"a"}`)
	page := conversationPage(t, a.conversation)
	if !strings.Contains(string(api.Payload(page)), `"text":"AB"`) {
		t.Fatal("omitted content changed the message")
	}
	emitV2(a, `{"sessionUpdate":"agent_message","messageId":"a","content":null}`)
	emitV2(a, `{"sessionUpdate":"agent_message_chunk","messageId":"a","content":{"type":"text","text":"C"}}`)
	emitV2(a, `{"sessionUpdate":"tool_call_update","toolCallId":"tool","title":"work","status":"in_progress","content":[]}`)
	emitV2(a, `{"sessionUpdate":"tool_call_content_chunk","toolCallId":"tool","content":{"type":"content","content":{"type":"text","text":"output"}}}`)
	emitV2(a, `{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`)
	if got := waitOperation(t, a, one); got.State != "completed" {
		t.Fatal(got)
	}
	before := conversationPage(t, a.conversation)
	emitV2(a, `{"sessionUpdate":"tool_call_update","toolCallId":"tool","status":"completed","title":null,"content":null}`)
	after := conversationPage(t, a.conversation)
	if len(before.Entries) != len(after.Entries) || a.snapshot().Busy != "" {
		t.Fatal("background output started a foreground turn")
	}
	for i, entry := range after.Entries {
		if entry.Message != nil && entry.Message.MessageID == "a" && rawMessageText(entry.Message.Content[0]) != "C" {
			t.Fatal("clear did not replace content")
		}
		if entry.Tool != nil && (entry.ID != before.Entries[i].ID || entry.Tool.Status != "completed" || string(entry.Tool.Fields["content"]) != "null" || before.Entries[i].Tool.Status != "in_progress") {
			t.Fatal("late tool or idle changed tool identity/state", entry)
		}
	}
}

func TestACPV2PermissionSurvivesInsertionResponseAndCancellation(t *testing.T) {
	a, requests := v2QueueFixture(t)
	one := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "work", ExpectedConversationID: a.snapshot().Conversation.ID})
	rpc := takeRPC(t, requests)
	a.receive([]byte(`{"jsonrpc":"2.0","id":"approval","method":"session/request_permission","params":{"sessionId":"session-a","title":"Run tests?","subject":{"type":"command","command":"go test ./...","cwd":"/tmp"},"options":[{"optionId":"yes","name":"Run","kind":"allow_once"}]}}`))
	replyRPC(a, rpc, map[string]string{"messageId": "user"})
	if len(a.snapshot().Permissions) != 1 {
		t.Fatal("insertion expired the approval")
	}
	if _, err := a.action(api.ACPAction{Action: "cancel", OperationRef: one.Ref}); err != nil {
		t.Fatal(err)
	}
	permission := takeRPC(t, requests)
	if !strings.Contains(string(permission.Result), "cancelled") {
		t.Fatal(permission)
	}
	if cancel := takeRPC(t, requests); cancel.Method != "session/cancel" {
		t.Fatal(cancel)
	}
	if a.snapshot().OperationRef != one.Ref {
		t.Fatal("cancel write fabricated completion")
	}
	emitV2(a, `{"sessionUpdate":"state_update","state":"idle","stopReason":"cancelled"}`)
	if got := waitOperation(t, a, one); got.State != "cancelled" {
		t.Fatal(got)
	}
}

func TestACPV2ResumeExplicitReplayAndExistingForeground(t *testing.T) {
	for _, replay := range []bool{false, true} {
		a, requests := v2QueueFixture(t)
		open := submitAction(t, a, api.ACPAction{Action: "resume", SessionID: "existing", Replay: replay})
		rpc := takeRPC(t, requests)
		if rpc.Method != "session/resume" || strings.Contains(string(rpc.Params), "replayFrom") != replay {
			t.Fatal(rpc)
		}
		if replay {
			emitV2(a, `{"sessionUpdate":"user_message","messageId":"retained","content":[{"type":"text","text":"history"}]}`)
		}
		emitV2(a, `{"sessionUpdate":"state_update","state":"running"}`)
		replyRPC(a, rpc, map[string]any{})
		if got := waitOperation(t, a, open); got.State != "completed" {
			t.Fatal(got)
		}
		foreground := a.snapshot().OperationRef
		if foreground == "" || foreground == open.Ref || a.snapshot().Busy != "prompt" {
			t.Fatal("resumed work lacks a separate cancellation identity")
		}
		if !replay && a.snapshot().Conversation.NativeHistoryCoverage != "not_requested" {
			t.Fatal("resume pretended to load history")
		}
		if _, err := a.action(api.ACPAction{Action: "cancel", OperationRef: foreground}); err != nil {
			t.Fatal(err)
		}
		if cancel := takeRPC(t, requests); cancel.Method != "session/cancel" {
			t.Fatal(cancel)
		}
		emitV2(a, `{"sessionUpdate":"state_update","state":"idle","stopReason":"cancelled"}`)
	}
}

func TestACPV2UnknownInsertionDoesNotReleaseQueue(t *testing.T) {
	a, requests := v2QueueFixture(t)
	request := api.ACPAction{Action: "prompt", Text: "once", ExpectedConversationID: a.snapshot().Conversation.ID}
	one := submitAction(t, a, request)
	rpc := takeRPC(t, requests)
	two := submitAction(t, a, request)
	replyRPC(a, rpc, map[string]string{"stopReason": "end_turn"})
	if got := waitOperation(t, a, one); got.State != "unknown" {
		t.Fatal(got)
	}
	if got := readOperation(t, a, two); got.State != "cancelled" {
		t.Fatal("unknown insertion released queued prompt", got)
	}
}

func TestACPV2ConfigurationAndPlanReplacement(t *testing.T) {
	a, requests := v2QueueFixture(t)
	a.mu.Lock()
	a.retainSessionConfigurationLocked([]byte(`{"configOptions":[{"configId":"model","type":"select","options":[{"groupId":"models","options":[{"value":"b"}]}]}]}`))
	a.mu.Unlock()
	op := submitAction(t, a, api.ACPAction{Action: "set_config_option", ConfigID: "model", ConfigValue: json.RawMessage(`"b"`), ExpectedConversationID: a.snapshot().Conversation.ID, SessionID: "session-a", Cwd: "/tmp"})
	rpc := takeRPC(t, requests)
	var fields map[string]json.RawMessage
	if json.Unmarshal(rpc.Params, &fields) != nil || rawString(fields, "type") != "id" || rawString(fields, "configId") != "model" {
		t.Fatal(string(rpc.Params))
	}
	replyRPC(a, rpc, map[string]any{"configOptions": []any{}})
	if got := waitOperation(t, a, op); got.State != "completed" {
		t.Fatal(got)
	}
	if _, err := a.action(api.ACPAction{Action: "set_mode", ModeID: "ask", ExpectedConversationID: a.snapshot().Conversation.ID, SessionID: "session-a", Cwd: "/tmp"}); err == nil {
		t.Fatal("v2 called removed modes method")
	}
	emitV2(a, `{"sessionUpdate":"plan_update","plan":{"planId":"one","type":"items","entries":[{"content":"old","status":"pending"}]}}`)
	emitV2(a, `{"sessionUpdate":"plan_update","plan":{"planId":"two","type":"_custom","details":{"keep":true}}}`)
	emitV2(a, `{"sessionUpdate":"plan_update","plan":{"planId":"one","type":"items","entries":[]}}`)
	state := a.snapshot().Conversation.State
	if strings.Contains(string(state["plan/one"]), "old") || !strings.Contains(string(state["plan/two"]), `"keep":true`) {
		t.Fatal(state)
	}
}
