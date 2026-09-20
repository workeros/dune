package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
)

func TestAgentMessengerUsesFabricdOperationsAcrossConnections(t *testing.T) {
	f := openExecutorFixture(t)
	gate := filepath.Join(f.workspace, "release-first")
	launched, connection := directoryACPWithEnvironment(t, f, map[string]string{"DUNE_HOST_FAKE_ACP_GATE": gate})
	directoryAction(t, connection, *launched.Runtime, api.ACPAction{Action: "new"})
	page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 {
		t.Fatal("discovery", page, err)
	}
	agent := page.Items[0]
	firstService, secondService := f.app.AgentMessenger(), f.app.AgentMessenger()
	first, err := firstService.Prompt(t.Context(), f.agentScope(), agents.PromptRequest{ExpectedConversationID: agent.Runtime.ConversationID, AgentRef: agent.Ref, Text: "first", WaitMS: 1})
	if err != nil || first.Terminal() || !strings.HasPrefix(first.Ref, "operation_") {
		t.Fatal("first submission", first, err)
	}
	second, err := secondService.Prompt(t.Context(), f.agentScope(), agents.PromptRequest{ExpectedConversationID: agent.Runtime.ConversationID, AgentRef: agent.Ref, Text: "second"})
	if err != nil || second.State != "pending" || second.Ref == first.Ref {
		t.Fatal("second submission", second, err)
	}
	read, err := secondService.Read(t.Context(), f.agentScope(), agents.ReadRequest{OperationRef: second.Ref})
	if err != nil || read.Operation.State != "pending" || len(read.Operation.Output) != 0 {
		t.Fatal("second consumed first output", read, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := firstService.Wait(ctx, f.agentScope(), agents.WaitRequest{OperationRef: first.Ref, TimeoutMS: 3000}); err == nil {
		t.Fatal("cancelled caller kept waiting")
	}
	waited, err := secondService.Wait(t.Context(), f.agentScope(), agents.WaitRequest{OperationRef: second.Ref, TimeoutMS: 1})
	if err != nil || !waited.TimedOut || waited.Operation.State != "pending" {
		t.Fatal("unrelated wait completed", waited, err)
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		op   agents.Operation
		text string
	}{{first, "first"}, {second, "second"}} {
		waited, err := secondService.Wait(t.Context(), f.agentScope(), agents.WaitRequest{OperationRef: item.op.Ref, TimeoutMS: 3000})
		if err != nil || waited.TimedOut || waited.Operation.State != "completed" || waited.Operation.StopReason != "end_turn" || waited.Operation.Ref != item.op.Ref {
			t.Fatal("operation completion", waited, err)
		}
		read, err := firstService.Read(t.Context(), f.agentScope(), agents.ReadRequest{OperationRef: item.op.Ref, Limit: 1})
		if err != nil || read.Operation.Incomplete || read.Operation.Position != 0 || read.Operation.NextPosition != 1 || len(read.Operation.Output) != 1 {
			t.Fatal("operation output", read, err)
		}
		var envelope struct {
			Update struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(read.Operation.Output[0], &envelope) != nil || envelope.Update.Content.Text != item.text {
			t.Fatal("wrong operation output")
		}
		end, err := firstService.Read(t.Context(), f.agentScope(), agents.ReadRequest{OperationRef: item.op.Ref, Position: read.Operation.NextPosition})
		if err != nil || len(end.Operation.Output) != 0 {
			t.Fatal("position did not advance", end, err)
		}
	}
	idle, err := firstService.Wait(t.Context(), f.agentScope(), agents.WaitRequest{AgentRef: agent.Ref, Until: "idle", TimeoutMS: 1000})
	if err != nil || idle.Agent == nil || idle.Operation != nil || idle.TimedOut {
		t.Fatal("activity mode", idle, err)
	}
	directoryAction(t, connection, *launched.Runtime, api.ACPAction{Action: "load", SessionID: "different", Cwd: f.workspace})
	_, err = firstService.Prompt(t.Context(), f.agentScope(), agents.PromptRequest{ExpectedConversationID: agent.Runtime.ConversationID, AgentRef: agent.Ref, Text: "must not be redirected"})
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "STALE_SESSION" {
		t.Fatal("old native reference was accepted", err)
	}
	if _, err := secondService.Read(t.Context(), f.agentScope(), agents.ReadRequest{OperationRef: first.Ref}); err != nil {
		t.Fatal("native switch expired earlier operation output", err)
	}
	wrong := f.agentScope()
	wrong.OwnerID = "another-tenant"
	if _, err := secondService.Read(t.Context(), wrong, agents.ReadRequest{OperationRef: first.Ref}); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("cross-Tenant operation read", err)
	}
	if _, err := secondService.Wait(t.Context(), wrong, agents.WaitRequest{OperationRef: first.Ref}); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("cross-Tenant operation wait", err)
	}
	if _, err := secondService.Prompt(t.Context(), wrong, agents.PromptRequest{ExpectedConversationID: agent.Runtime.ConversationID, AgentRef: agent.Ref, Text: "cross tenant"}); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("cross-Tenant prompt", err)
	}
	if _, err := secondService.SendKeys(t.Context(), f.agentScope(), agents.KeysRequest{AgentRef: agent.Ref, Keys: []string{"Enter"}}); err == nil {
		t.Fatal("ACP accepted terminal keys")
	}
}

// This native process records bytes; it makes no model or vendor requests.
func TestMessengerPTYRecorder(t *testing.T) {
	record := os.Getenv("DUNE_MESSENGER_PTY_RECORD")
	if record == "" {
		return
	}
	command := exec.Command("/bin/stty", "raw", "-echo")
	command.Stdin = os.Stdin
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(record, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fmt.Print("\x1b[?2004hMESSENGER_READY")
	_, _ = io.Copy(file, os.Stdin)
}

func TestAgentMessengerPTYDeliveryAndSnapshotRemainDistinct(t *testing.T) {
	f := openExecutorFixture(t)
	program, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.workspace, "codex")
	if err := os.WriteFile(path, program, 0700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(f.workspace, "input")
	profile := api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: f.workspace, Start: api.Command{Argv: []string{path, "-test.run=^TestMessengerPTYRecorder$"}}, Env: map[string]string{"DUNE_MESSENGER_PTY_RECORD": record, "PATH": "/usr/bin:/bin"}}
	if _, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Custom: &profile}); err != nil {
		t.Fatal(err)
	}
	messenger := f.app.AgentMessenger()
	var agent agents.Agent
	for deadline := time.Now().Add(4 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
		if err != nil || len(page.Items) != 1 {
			t.Fatal(page, err)
		}
		agent = page.Items[0]
		read, err := messenger.Read(t.Context(), f.agentScope(), agents.ReadRequest{AgentRef: agent.Ref})
		if err == nil && strings.Contains(read.Snapshot.Terminal.Content, "MESSENGER_READY") && agent.Runtime.Activity.Agent == "codex" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PTY did not become ready", err)
		}
	}
	op, err := messenger.Prompt(t.Context(), f.agentScope(), agents.PromptRequest{ExpectedConversationID: agent.Runtime.ConversationID, AgentRef: agent.Ref, Text: "delegated task", WaitMS: 3000})
	if err != nil || op.State != "delivered" {
		t.Fatal("PTY delivery", op, err)
	}
	activity, err := messenger.Wait(t.Context(), f.agentScope(), agents.WaitRequest{AgentRef: agent.Ref, Until: "idle", TimeoutMS: 1})
	if err != nil || !activity.TimedOut || activity.Agent.Runtime.Activity.State != "unknown" {
		t.Fatal("delivery implied task completion", activity, err)
	}
	if _, err := messenger.Read(t.Context(), f.agentScope(), agents.ReadRequest{OperationRef: op.Ref}); err == nil {
		t.Fatal("PTY pretended to have per-prompt output")
	}
	keys, err := messenger.SendKeys(t.Context(), f.agentScope(), agents.KeysRequest{AgentRef: agent.Ref, Keys: []string{"Tab", "Enter"}})
	if err != nil {
		t.Fatal(err)
	}
	done, err := messenger.Wait(t.Context(), f.agentScope(), agents.WaitRequest{OperationRef: keys.Ref, TimeoutMS: 3000})
	if err != nil || done.Operation.State != "delivered" {
		t.Fatal(done, err)
	}
	want := []byte("\x1b[200~delegated task\x1b[201~\r\t\r")
	for deadline := time.Now().Add(time.Second); ; time.Sleep(5 * time.Millisecond) {
		data, err := os.ReadFile(record)
		if err == nil && bytes.Equal(data, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unexpected native input %q: %v", data, err)
		}
	}
}

func TestAgentMessengerPreservesObservedConversationAfterSameNativeReload(t *testing.T) {
	f := openExecutorFixture(t)
	started, connection := directoryACPWithEnvironment(t, f, nil)
	page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	observed := page.Items[0]
	if observed.Runtime.ConversationID == "" {
		t.Fatal("discovery omitted prompt precondition")
	}
	directoryAction(t, connection, *started.Runtime, api.ACPAction{Action: "load", SessionID: observed.Runtime.NativeSession.ID, Cwd: observed.Runtime.NativeSession.Cwd})
	_, err = f.app.AgentMessenger().Prompt(t.Context(), f.agentScope(), agents.PromptRequest{AgentRef: observed.Ref, ExpectedConversationID: observed.Runtime.ConversationID, Text: "must not be redirected"})
	if errorCode(err) != "CONVERSATION_CHANGED" {
		t.Fatal("host refreshed the caller's precondition", err)
	}
	_, err = f.app.AgentMessenger().Prompt(t.Context(), f.agentScope(), agents.PromptRequest{AgentRef: observed.Ref, Text: "missing condition"})
	if errorCode(err) != "INVALID_ARGUMENT" {
		t.Fatal("host accepted missing conversation ID", err)
	}
}
