package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func TestPTYOperationRecorderChild(t *testing.T) {
	path := os.Getenv("DUNE_TEST_PTY_INPUT_CAPTURE")
	if path == "" {
		return
	}
	command := exec.Command("/bin/stty", "raw", "-echo")
	command.Stdin = os.Stdin
	must(t, command.Run())
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	must(t, err)
	defer file.Close()
	fmt.Print("\x1b[?2004hPTY_RECORDER_READY")
	_, _ = io.Copy(file, os.Stdin)
}

func TestPTYOperationsShareBrowserInputAndExpireOnFabricdRestart(t *testing.T) {
	h := start(t)
	executable, err := os.Executable()
	must(t, err)
	program, err := os.ReadFile(executable)
	must(t, err)
	agent := filepath.Join(h.dir, "codex")
	must(t, os.WriteFile(agent, program, 0700))
	record := filepath.Join(h.dir, "terminal-input")
	p := profile(h.dir, "pty", agent, "-test.run=^TestPTYOperationRecorderChild$")
	p.Env = map[string]string{"DUNE_TEST_PTY_INPUT_CAPTURE": record}
	runtime, browser, err := testStartProfile(h.client, h.ctx, p)
	must(t, err)
	defer browser.Close()
	receive(t, browser, "data", "PTY_RECORDER_READY")
	_, err = browser.Input([]byte("draft:"))
	must(t, err)
	receive(t, browser, "written", "")
	operation, err := h.client.PTYPrompt(h.ctx, runtime, api.PTYPrompt{Agent: "codex", Text: "agent text"})
	must(t, err)
	_, err = browser.Input([]byte("after:"))
	must(t, err)
	result, err := h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 5000})
	must(t, err)
	if result.State != "delivered" {
		t.Fatalf("PTY submission: %+v", result)
	}
	receive(t, browser, "written", "")
	want := []byte("draft:\x1b[200~agent text\x1b[201~\rafter:")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		data, _ := os.ReadFile(record)
		if bytes.Equal(data, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("interleaved terminal input: got %q want %q", data, want)
		}
	}
	// The submitting browser is disposable; the Runtime and ordered input live
	// in fabricd. A fabricd restart keeps tmux but expires its operation records.
	browser.Close()
	h.client.Close()
	h.restartProcess("fabricd")
	h.reconnect()
	current, err := h.client.Get(h.ctx, runtime)
	must(t, err)
	if current.State != "running" || current.Incarnation != runtime.Incarnation {
		t.Fatal("tmux Runtime did not survive fabricd restart")
	}
	if _, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: operation.Ref}); err == nil || !strings.Contains(err.Error(), "OPERATION_EXPIRED") {
		t.Fatalf("old PTY operation after fabricd restart: %v", err)
	}
	keys, err := h.client.PTYSendKeys(h.ctx, runtime, api.PTYKeys{Agent: "codex", Keys: []string{"Enter"}})
	must(t, err)
	result, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: keys.Ref, TimeoutMS: 5000})
	must(t, err)
	if result.State != "delivered" {
		t.Fatalf("input after restore: %+v", result)
	}
	must(t, h.client.Stop(h.ctx, runtime))
}

func TestAgentOperationsShareFabricdQueueAcrossConnections(t *testing.T) {
	h := start(t)
	mock := filepath.Join(h.dir, "mock-acp")
	output, err := exec.Command("go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput()
	if err != nil {
		t.Fatalf("mock build: %s %v", output, err)
	}
	profile := profile(h.dir, "acp", mock)
	profile.ManagedACP = true
	runtime, stream, err := testStartProfile(h.client, h.ctx, profile)
	must(t, err)
	stream.Close()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var state struct {
			Ready bool `json:"ready"`
		}
		must(t, h.client.CallID(h.ctx, "acp.state", wire.ID(), struct{}{}, &state, &runtime))
		if state.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ACP initialization did not finish")
		}
	}
	created, err := testACPSubmit(h.client, h.ctx, runtime, api.ACPAction{Action: "new"})
	must(t, err)
	created, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: created.Ref, TimeoutMS: 3000})
	must(t, err)
	if created.State != "completed" || created.NativeSession == nil || created.NativeSession.ID != "mock-session" || created.NativeSession.Cwd != runtime.WorkingDirectory || created.NativeSession.Sequence != 1 {
		t.Fatalf("new: %+v", created)
	}
	clientA := h.client
	h.reconnect()
	defer clientA.Close()
	observed, err := h.client.ACPState(h.ctx, runtime)
	must(t, err)
	first, err := testACPSubmit(clientA, h.ctx, runtime, api.ACPAction{ExpectedConversationID: observed.Conversation.ID, Action: "prompt", Text: "permission first", SessionID: "mock-session"})
	must(t, err)
	second, err := testACPSubmit(h.client, h.ctx, runtime, api.ACPAction{ExpectedConversationID: observed.Conversation.ID, Action: "prompt", Text: "second", SessionID: "mock-session"})
	must(t, err)
	if second.State != "pending" {
		t.Fatalf("second admission: %+v", second)
	}
	before, err := clientA.ReadAgentOperation(h.ctx, runtime, api.AgentOperationRead{Ref: second.Ref})
	must(t, err)
	if before.State != "pending" || before.NextPosition != 0 || len(before.Output) != 0 {
		t.Fatalf("B exposed A output: %+v", before)
	}
	// Closing the submitting client does not own or cancel admitted work.
	clientA.Close()
	var permissions struct {
		Permissions []struct {
			ID string `json:"id"`
		} `json:"permissions"`
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		must(t, h.client.CallID(h.ctx, "acp.state", wire.ID(), struct{}{}, &permissions, &runtime))
		if len(permissions.Permissions) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("permission did not arrive")
		}
	}
	var accepted json.RawMessage
	must(t, h.client.CallID(h.ctx, "acp.action", wire.ID(), api.ACPAction{Action: "permission", PermissionID: permissions.Permissions[0].ID, OptionID: "allow"}, &accepted, &runtime))
	for _, operation := range []api.AgentOperation{first, second} {
		result, err := h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 3000})
		must(t, err)
		if result.State != "completed" || result.StopReason != "end_turn" {
			t.Fatalf("operation: %+v", result)
		}
	}
	a, err := h.client.ReadAgentOperation(h.ctx, runtime, api.AgentOperationRead{Ref: first.Ref})
	must(t, err)
	b, err := h.client.ReadAgentOperation(h.ctx, runtime, api.AgentOperationRead{Ref: second.Ref})
	must(t, err)
	if a.Incomplete || b.Incomplete || len(a.Output) != 1 || len(b.Output) != 1 || strings.Contains(string(a.Output[0]), "second") || !strings.Contains(string(b.Output[0]), "second") {
		t.Fatalf("output association: A=%s B=%s", api.Payload(a), api.Payload(b))
	}
	// Reconnecting both clients still reads the same fabricd records.
	h.client.Close()
	h.reconnect()
	bAgain, err := h.client.ReadAgentOperation(h.ctx, runtime, api.AgentOperationRead{Ref: second.Ref})
	must(t, err)
	if string(api.Payload(b)) != string(api.Payload(bAgain)) {
		t.Fatal("reconnect changed operation result")
	}
	wrong := runtime
	wrong.Incarnation = "another-runtime"
	if _, err := h.client.ReadAgentOperation(h.ctx, wrong, api.AgentOperationRead{Ref: second.Ref}); err == nil {
		t.Fatal("wrong Runtime read operation")
	}
	must(t, h.client.Stop(h.ctx, runtime))
}
