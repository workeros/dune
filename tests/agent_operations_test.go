package tests

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func TestAgentOperationsShareFabricdQueueAcrossConnections(t *testing.T) {
	h := start(t)
	mock := filepath.Join(h.dir, "mock-acp")
	output, err := exec.Command("go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput()
	if err != nil {
		t.Fatalf("mock build: %s %v", output, err)
	}
	profile := profile(h.dir, "acp", mock)
	profile.ManagedACP = true
	runtime, stream, err := h.client.Start(h.ctx, profile)
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
	created, err := h.client.ACPSubmit(h.ctx, runtime, api.ACPAction{Action: "new"})
	must(t, err)
	created, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: created.Ref, TimeoutMS: 3000})
	must(t, err)
	if created.State != "completed" || created.NativeSessionID != "mock-session" {
		t.Fatalf("new: %+v", created)
	}
	clientA := h.client
	h.reconnect()
	defer clientA.Close()
	first, err := clientA.ACPSubmit(h.ctx, runtime, api.ACPAction{Action: "prompt", Text: "permission first", SessionID: "mock-session"})
	must(t, err)
	second, err := h.client.ACPSubmit(h.ctx, runtime, api.ACPAction{Action: "prompt", Text: "second", SessionID: "mock-session"})
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
