package fabricd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/gateway"
)

func TestProtectedStreamsAfterConnectorCrashWithOrdinarySubscriptionsFull(t *testing.T) {
	mock := filepath.Join(t.TempDir(), "mock-acp")
	if out, err := exec.Command("go", "build", "-o", mock, "../../samples/mock-acp").CombinedOutput(); err != nil {
		t.Fatal("mock build", string(out), err)
	}
	h := newCleanupProcessHarness(t)
	h.start("")
	work := t.TempDir()
	rpcLog, processLog := filepath.Join(work, "rpc.log"), filepath.Join(work, "process.log")
	runtime, launch, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: work, Start: api.Command{Argv: []string{mock}}, Env: map[string]string{"DUNE_MOCK_HISTORY": "1", "DUNE_MOCK_RPC_LOG": rpcLog, "DUNE_MOCK_PROCESS_LOG": processLog}})
	if err != nil {
		t.Fatal(err)
	}
	launch.Close()
	state := func() api.ACPState {
		t.Helper()
		s, err := h.client.ACPState(h.ctx, runtime)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	waitTimeoutTest(t, func() bool { return state().Ready })
	opened, err := h.client.ACPSubmit(h.ctx, cleanupTestKey(runtime, "open"), api.ACPAction{Action: "new", Cwd: work})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: opened.Ref, TimeoutMS: 3000}); err != nil || result.State != "completed" {
		t.Fatal(result, err)
	}
	conversation := state().Conversation.ID
	first, err := h.client.ACPSubmit(h.ctx, cleanupTestKey(runtime, "first"), api.ACPAction{Action: "prompt", Text: "permission first", ExpectedConversationID: conversation})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.client.ACPSubmit(h.ctx, cleanupTestKey(runtime, "second"), api.ACPAction{Action: "prompt", Text: "permission second", ExpectedConversationID: conversation})
	if err != nil {
		t.Fatal(err)
	}
	waitTimeoutTest(t, func() bool { return len(state().Permissions) == 1 })
	permission := state().Permissions[0].ID
	// Eight real hosts, each with its existing eight-subscription limit, fill
	// all 64 ordinary SDK/Gateway/fabricd slots without increasing any budget.
	runtimes := []api.Runtime{runtime}
	for len(runtimes) < 8 {
		r, _ := h.launch(mock)
		runtimes = append(runtimes, r)
	}
	ended, _ := h.launch(mock)
	if err := testStopRuntime(h.client, h.ctx, ended); err != nil {
		t.Fatal(err)
	}
	fill := func() []*client.Stream {
		t.Helper()
		var streams []*client.Stream
		for _, r := range runtimes {
			for range 8 {
				s, err := h.client.Attach(h.ctx, r, true)
				if err != nil {
					t.Fatal("ordinary subscription failed before its original hard limit", err)
				}
				streams = append(streams, s)
			}
		}
		if usage := h.g.Status().StreamCapacity["ordinary"]; usage.Used != wire.MaxStreams {
			t.Fatal("ordinary traffic was not held", usage)
		}
		return streams
	}
	for _, s := range fill() {
		defer s.Close()
	}
	h.kill()
	h.start("")
	for _, s := range fill() {
		defer s.Close()
	}
	if current := state(); current.Conversation.ID != conversation || len(current.Permissions) != 1 || current.Permissions[0].ID != permission {
		t.Fatal("reconnect lost original model or control target", current)
	}
	_, err = h.client.Attach(h.ctx, runtime, true)
	requireSubmissionCode(t, err, "RESOURCE_EXHAUSTED")
	if !strings.Contains(err.Error(), "SDK ordinary") {
		t.Fatal("SDK did not reserve its own control slots", err)
	}
	// A different client bypasses the full SDK and per-connection Gateway
	// budget, so this refusal must come from the machine-wide fabricd budget.
	left, right := net.Pipe()
	binding, handler, err := (access.Grant{Target: "cleanup-test", Role: gateway.RoleSDK}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	go h.g.ServeConn(h.ctx, left, binding, handler)
	other, err := client.Connect(h.ctx, right, "cleanup-test")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_, err = other.Attach(h.ctx, runtime, true)
	requireSubmissionCode(t, err, "RESOURCE_EXHAUSTED")
	if !strings.Contains(err.Error(), "fabricd ordinary") {
		t.Fatal("fabricd ordinary budget was not exercised", err)
	}
	var machine api.MachineInfo
	if err := h.client.Call(h.ctx, "machine.info", struct{}{}, &machine); err != nil || machine.StreamCapacity["ordinary"].Used != wire.MaxStreams || machine.StreamCapacity["ordinary"].Limit != wire.MaxStreams {
		t.Fatal("public machine diagnostics did not report actual ordinary pressure", machine, err)
	}
	// Waiting on a task must not exhaust the snapshot readers needed to answer
	// the very permission that will let that task finish.
	waitCtx, cancelWaits := context.WithCancel(h.ctx)
	defer cancelWaits()
	waiters := make(chan error, wire.MaxReservedStreams)
	for i := range wire.MaxReservedStreams {
		go func() {
			_, err := h.client.WaitAgentOperation(waitCtx, runtime, api.AgentOperationWait{Ref: first.Ref, TimeoutMS: 30000})
			waiters <- err
		}()
		waitTimeoutTest(t, func() bool { return h.g.Status().StreamCapacity["wait"].Used == i+1 })
	}
	waitTimeoutTest(t, func() bool { return h.g.Status().StreamCapacity["wait"].Used == wire.MaxReservedStreams })
	if observed, err := h.client.QuerySubmission(h.ctx, cleanupTestKey(runtime, "first")); err != nil || observed.OperationRef != first.Ref {
		t.Fatal("full ordinary/wait pools blocked original receipt", observed, err)
	}
	if current := state(); current.Permissions[0].ID != permission {
		t.Fatal("waiters changed permission", current)
	}
	forgetKey := cleanupTestKey(ended, "forget-while-full")
	if receipt, err := h.client.Forget(h.ctx, forgetKey); err != nil || receipt.Admission != api.SubmissionAccepted {
		t.Fatal("ordinary saturation blocked eligible cleanup", receipt, err)
	}
	waitTimeoutTest(t, func() bool {
		r, err := h.client.QuerySubmission(h.ctx, forgetKey)
		return err == nil && r.Stage == "completed"
	})
	answer := api.ACPAction{Action: "permission", PermissionID: permission, OptionID: "allow"}
	// Bad targets and duplicate keys do not retain execution slots or consume
	// another permission's durable reservation.
	for i := range 20 {
		invalid := answer
		invalid.OptionID = "invalid"
		if _, err := h.client.ACPControl(h.ctx, cleanupTestKey(runtime, fmt.Sprintf("invalid-%d", i)), invalid); err == nil {
			t.Fatal("invalid answer succeeded")
		}
	}
	answerKey := cleanupTestKey(runtime, "answer")
	answered, err := h.client.ACPControl(h.ctx, answerKey, answer)
	if err != nil || answered.Stage != "written" {
		t.Fatal("effective permission was blocked", answered, err)
	}
	for range wire.MaxReservedStreams {
		if err := <-waiters; err != nil {
			t.Fatal("original wait did not complete", err)
		}
	}
	for range 10 {
		r, err := h.client.ACPControl(h.ctx, answerKey, answer)
		if err != nil || r.OperationRef != answered.OperationRef {
			t.Fatal("duplicate permission was re-admitted", r, err)
		}
	}
	waitTimeoutTest(t, func() bool {
		s := state()
		return len(s.Permissions) == 1 && s.Permissions[0].ID != permission
	})
	cancelled, err := h.client.ACPControl(h.ctx, cleanupTestKey(runtime, "cancel"), api.ACPAction{Action: "cancel", OperationRef: second.Ref})
	if err != nil || cancelled.Stage != "written" {
		t.Fatal("effective cancel was blocked", cancelled, err)
	}
	waitTimeoutTest(t, func() bool {
		body, _ := os.ReadFile(rpcLog)
		return strings.Count(string(body), "session/cancel\n") == 1
	})
	if stopped, err := h.client.Stop(h.ctx, cleanupTestKey(runtime, "stop")); err != nil || stopped.Stage != "stopped" {
		t.Fatal("stop did not prove original process exit", stopped, err)
	}
	processes, err := os.ReadFile(processLog)
	if err != nil || strings.Count(string(processes), "\n") != 1 {
		t.Fatal("reconnect replaced the original Agent", string(processes), err)
	}
	rpcs, err := os.ReadFile(rpcLog)
	if err != nil || strings.Count(string(rpcs), "initialize\n") != 1 || strings.Count(string(rpcs), "session/new\n") != 1 || strings.Count(string(rpcs), "session/prompt\n") != 2 || strings.Count(string(rpcs), "session/cancel\n") != 1 {
		t.Fatal("control/ordinary traffic replayed ACP requests", string(rpcs), err)
	}
	history, err := os.ReadFile(filepath.Join(work, ".dune-mock-acp-history.json"))
	if err != nil || strings.Count(string(history), "Mock permission response received") != 2 {
		t.Fatal("permission answer/cancel did not each reach the Agent once", string(history), err)
	}
	// Exercise the original query after control completion; reads spend no keys.
	for _, id := range []string{"answer", "cancel", "stop"} {
		r, err := h.client.QuerySubmission(h.ctx, cleanupTestKey(runtime, id))
		if err != nil || r.Admission != api.SubmissionAccepted {
			t.Fatal(id, r, err)
		}
	}
}
