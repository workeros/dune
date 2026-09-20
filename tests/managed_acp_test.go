package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func TestManagedACPHistoryReplayIgnoresSlowDiagnostics(t *testing.T) {
	h := start(t)
	mock := filepath.Join(h.dir, "mock-acp")
	output, err := exec.Command("go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput()
	if err != nil {
		t.Fatalf("mock build: %s %v", output, err)
	}
	const updateCount = 160
	largeValue := strings.Repeat("x", 80*1024)
	history := make([]map[string]any, 0, updateCount)
	for i := 0; i < updateCount; i++ {
		value := largeValue
		if i == 0 {
			value = strings.Repeat("y", 900*1024)
		}
		history = append(history, map[string]any{"sessionUpdate": "tool_call", "toolCallId": fmt.Sprintf("tool-%d", i), "title": fmt.Sprintf("tool title %d", i), "rawOutput": value})
	}
	b, err := json.Marshal(history)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(h.dir, ".dune-mock-acp-history.json"), b, 0600))

	p := profile(h.dir, "acp", mock)
	p.ManagedACP = true
	gate, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer gate.Close()
	p.Env = map[string]string{"DUNE_MOCK_HISTORY": "1", "DUNE_MOCK_LOAD_GATE": gate.Addr().String()}
	rt, initial, err := h.client.Start(h.ctx, p)
	must(t, err)
	initial.Close()
	defer h.client.Stop(h.ctx, rt)

	type state struct {
		Ready bool   `json:"ready"`
		Busy  string `json:"busy"`
	}
	for deadline := time.Now().Add(8 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var current state
		must(t, h.client.CallID(h.ctx, "acp.state", wire.ID(), map[string]any{}, &current, &rt))
		if current.Ready && current.Busy == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ACP did not become ready")
		}
	}

	observer, err := h.client.Attach(h.ctx, rt, true)
	must(t, err)
	defer observer.Close()
	// A second actual Agent remains usable while the first replays a large
	// history to a non-consuming diagnostic observer.
	normalProfile := profile(t.TempDir(), "acp", mock)
	normalProfile.ManagedACP = true
	normal, normalStream, err := h.client.Start(h.ctx, normalProfile)
	must(t, err)
	normalStream.Close()
	waitManagedACPReady(t, h, normal)
	opened, err := h.client.ACPSubmit(h.ctx, normal, api.ACPAction{Action: "new"})
	must(t, err)
	opened, err = h.client.WaitAgentOperation(h.ctx, normal, api.AgentOperationWait{Ref: opened.Ref, TimeoutMS: 5000})
	must(t, err)
	accepted, err := h.client.ACPSubmit(h.ctx, rt, api.ACPAction{Action: "load", SessionID: "mock-session"})
	must(t, err)
	must(t, gate.(*net.TCPListener).SetDeadline(time.Now().Add(5*time.Second)))
	paused, err := gate.Accept()
	must(t, err)
	defer paused.Close()
	must(t, paused.SetDeadline(time.Now().Add(10*time.Second)))
	var ready [1]byte
	_, err = io.ReadFull(paused, ready[:])
	must(t, err)
	loading, err := h.client.ACPState(h.ctx, rt)
	must(t, err)
	if loading.Conversation == nil || loading.Conversation.Phase != "loading" {
		t.Fatal("load barrier did not hold the native result")
	}
	prompt, err := h.client.ACPSubmit(h.ctx, normal, api.ACPAction{Action: "prompt", ExpectedConversationID: opened.ConversationID, Text: "unrelated progress during replay"})
	must(t, err)
	prompt, err = h.client.WaitAgentOperation(h.ctx, normal, api.AgentOperationWait{Ref: prompt.Ref, TimeoutMS: 5000})
	must(t, err)
	if prompt.State != "completed" {
		t.Fatal("replay starved another Runtime", prompt)
	}
	other, err := h.client.ReadACPConversation(h.ctx, normal, api.ACPConversationRead{ConversationID: opened.ConversationID})
	must(t, err)
	if !strings.Contains(string(api.Payload(other.Entries)), "unrelated progress during replay") {
		t.Fatal("concurrent normal conversation lost content")
	}
	_, err = paused.Write([]byte{1})
	must(t, err)
	completed, err := h.client.WaitAgentOperation(h.ctx, rt, api.AgentOperationWait{Ref: accepted.Ref, TimeoutMS: 30000})
	must(t, err)
	if completed.State != "completed" {
		t.Fatal("slow diagnostics blocked native load", completed)
	}
	current, err := h.client.ACPState(h.ctx, rt)
	must(t, err)
	page, err := h.client.ReadACPConversation(h.ctx, rt, api.ACPConversationRead{ConversationID: current.Conversation.ID})
	must(t, err)
	if page.Conversation.HeadOrder != updateCount || !page.Conversation.PrefixEvicted || !page.Conversation.ContentOmitted || len(page.Entries) == 0 || len(api.Payload(page)) > api.MaxACPConversationResponseBytes {
		t.Fatal("replay did not reach a bounded retained model", page.Conversation)
	}
	last := page.Entries[len(page.Entries)-1]
	if last.Tool.ID != "tool-159" || string(last.Tool.Fields["title"]) != `"tool title 159"` {
		t.Fatal("replay tail lost", last.Tool.ID)
	}

}

func TestManagedACPOfflinePermissions(t *testing.T) {
	h := start(t)
	mock := filepath.Join(h.dir, "mock-acp")
	output, err := exec.Command("go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput()
	if err != nil {
		t.Fatalf("mock build: %s %v", output, err)
	}
	p := profile(h.dir, "acp", mock)
	p.ManagedACP = true
	rt, stream, err := h.client.Start(h.ctx, p)
	must(t, err)
	stream.Close()
	type state struct {
		Conversation *api.ACPConversation `json:"conversation"`
		Ready        bool                 `json:"ready"`
		Busy         string               `json:"busy"`
		SessionID    string               `json:"session_id"`
		Permissions  []struct {
			ID string `json:"id"`
		} `json:"permissions"`
	}
	read := func() state {
		var s state
		must(t, h.client.CallID(h.ctx, "acp.state", wire.ID(), map[string]any{}, &s, &rt))
		return s
	}
	await := func(check func(state) bool) state {
		t.Helper()
		for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
			s := read()
			if check(s) {
				return s
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("ACP state did not settle")
		return state{}
	}
	action := func(a map[string]any) error {
		var out json.RawMessage
		return h.client.CallID(h.ctx, "acp.action", wire.ID(), a, &out, &rt)
	}
	await(func(s state) bool { return s.Ready })
	must(t, action(map[string]any{"action": "new"}))
	observed := await(func(s state) bool { return s.SessionID != "" && s.Busy == "" })
	for _, a := range []string{"list", "load"} {
		if err := action(map[string]any{"action": a, "session_id": "mock-session"}); err == nil || !strings.Contains(err.Error(), "UNSUPPORTED") {
			t.Fatalf("unadvertised %s: %v", a, err)
		}
	}
	must(t, action(map[string]any{"action": "prompt", "expected_conversation_id": observed.Conversation.ID, "text": "offline permission"}))
	pending := await(func(s state) bool { return len(s.Permissions) == 1 })
	var queued json.RawMessage
	must(t, h.client.CallID(h.ctx, "acp.action", wire.ID(), map[string]any{"action": "new"}, &queued, &rt))
	if !strings.Contains(string(queued), `"state":"pending"`) {
		t.Fatalf("concurrent new did not queue: %s", queued)
	}
	if _, err := h.client.Attach(h.ctx, rt, false); err == nil {
		t.Fatal("raw ACP input was not rejected")
	}
	h.client.Close()
	time.Sleep(100 * time.Millisecond)
	h.reconnect()
	still := read()
	if still.Busy != "prompt" || len(still.Permissions) != 1 || still.Permissions[0].ID != pending.Permissions[0].ID {
		t.Fatal("offline permission changed")
	}
	permission := map[string]any{"action": "permission", "permission_id": pending.Permissions[0].ID, "option_id": "allow"}
	must(t, action(permission))
	if err := action(permission); err == nil {
		t.Fatal("permission answered twice")
	}
	observed = await(func(s state) bool { return s.Busy == "" && len(s.Permissions) == 0 })
	must(t, action(map[string]any{"action": "prompt", "expected_conversation_id": observed.Conversation.ID, "text": "permission cancellation"}))
	stale := await(func(s state) bool { return len(s.Permissions) == 1 })
	must(t, action(map[string]any{"action": "cancel"}))
	await(func(s state) bool { return s.Busy == "" && len(s.Permissions) == 0 })
	if err := action(map[string]any{"action": "permission", "permission_id": stale.Permissions[0].ID, "option_id": "allow"}); err == nil {
		t.Fatal("cancelled permission accepted")
	}
	err = h.client.Stop(h.ctx, rt)
	must(t, err)
}
