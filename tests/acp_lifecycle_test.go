package tests

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func TestManagedACPOriginalProcessAcrossConnectorRestart(t *testing.T) {
	h := start(t)
	mock := filepath.Join(h.dir, "mock-acp")
	if output, err := exec.Command("go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput(); err != nil {
		t.Fatalf("mock build: %s %v", output, err)
	}
	gate, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer gate.Close()
	p := profile(h.dir, "acp", mock)
	p.ManagedACP = true
	rpcLog, processLog := filepath.Join(h.dir, "rpc.log"), filepath.Join(h.dir, "process.log")
	p.Env = map[string]string{"DUNE_MOCK_HISTORY": "1", "DUNE_MOCK_RPC_LOG": rpcLog, "DUNE_MOCK_PROCESS_LOG": processLog, "DUNE_MOCK_PROMPT_GATE": gate.Addr().String()}
	runtime, stream, err := testStartProfile(h.client, h.ctx, p)
	must(t, err)
	stream.Close()
	defer func() { _ = h.client.Stop(h.ctx, runtime) }()
	waitManagedACPReady(t, h, runtime)
	key := api.SubmissionKey{Target: api.SubmissionTarget{
		OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: h.c.Target, BindingRevision: 1,
		RuntimeID: runtime.ID, RuntimeIncarnation: runtime.Incarnation, RuntimeGeneration: runtime.Generation,
	}}
	submit := func(id string, action api.ACPAction) api.SubmissionReceipt {
		t.Helper()
		key.SubmissionID = id
		receipt, err := h.client.Submit(h.ctx, api.SubmissionRequest{SubmissionKey: key, Operation: "acp.action", Payload: api.Payload(action)})
		must(t, err)
		return receipt
	}
	wait := func(receipt api.SubmissionReceipt) {
		t.Helper()
		operation, err := h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: receipt.OperationRef, TimeoutMS: 3000})
		must(t, err)
		if operation.State != "completed" {
			t.Fatal("original operation did not complete", operation)
		}
		observed, err := h.client.QuerySubmission(h.ctx, receipt.SubmissionKey)
		must(t, err)
		if observed.OperationRef != receipt.OperationRef || observed.Admission != api.SubmissionAccepted {
			t.Fatal("restart changed admission identity", observed, receipt)
		}
	}
	wait(submit("open", api.ACPAction{Action: "new", Cwd: h.dir}))
	state, err := h.client.ACPState(h.ctx, runtime)
	must(t, err)
	conversation := state.Conversation.ID
	active := submit("active-before-kill", api.ACPAction{Action: "prompt", Text: "barrier task", ExpectedConversationID: conversation})
	_ = gate.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	blocked, err := gate.Accept()
	must(t, err)
	defer blocked.Close()
	var entered [1]byte
	_, err = blocked.Read(entered[:])
	must(t, err)
	queued := submit("queued-before-kill", api.ACPAction{Action: "prompt", Text: "offline queued task", ExpectedConversationID: conversation})
	h.client.Close()
	h.stopProcess("fabricd", syscall.SIGKILL)
	_, err = blocked.Write([]byte{1})
	must(t, err)
	// Both tasks must progress with no connector or observer running.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		history, _ := os.ReadFile(filepath.Join(h.dir, ".dune-mock-acp-history.json"))
		if strings.Contains(string(history), "Mock response") && strings.Count(string(history), "agent_message_chunk") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("offline host did not complete running and queued work")
		}
	}
	h.startProcess("fabricd")
	h.reconnect()
	wait(active)
	wait(queued)
	listed, err := h.client.List(h.ctx)
	must(t, err)
	if len(listed) != 1 || listed[0].ID != runtime.ID || listed[0].Incarnation != runtime.Incarnation || listed[0].Generation != runtime.Generation || listed[0].Availability != "" {
		t.Fatal("connector did not rediscover the original Runtime", listed)
	}
	permissionTask := submit("permission-before-term", api.ACPAction{Action: "prompt", Text: "permission task", ExpectedConversationID: conversation})
	var permissionID string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		state, err = h.client.ACPState(h.ctx, runtime)
		must(t, err)
		if len(state.Permissions) == 1 {
			permissionID = state.Permissions[0].ID
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("missing permission")
		}
	}
	before := state.Conversation.Revision
	h.client.Close()
	h.stopProcess("fabricd", syscall.SIGTERM)
	h.startProcess("fabricd")
	h.reconnect()
	state, err = h.client.ACPState(h.ctx, runtime)
	must(t, err)
	if len(state.Permissions) != 1 || state.Permissions[0].ID != permissionID || state.Conversation.ID != conversation || state.Conversation.Revision < before {
		t.Fatal("restart reset permission or model identity", state)
	}
	must(t, h.client.CallID(h.ctx, "acp.action", "permission-control", api.ACPAction{Action: "permission", PermissionID: permissionID, OptionID: "allow"}, nil, &runtime))
	wait(permissionTask)
	page, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: conversation})
	must(t, err)
	if len(page.Entries) < 9 {
		t.Fatal("offline model lost original task content", page)
	}
	processes, err := os.ReadFile(processLog)
	must(t, err)
	if strings.Count(string(processes), "\n") != 1 {
		t.Fatalf("connector restart replaced Agent process: %s", processes)
	}
	var process struct {
		PID        int  `json:"pid"`
		StdinPipe  bool `json:"stdin_pipe"`
		StdoutPipe bool `json:"stdout_pipe"`
	}
	must(t, json.Unmarshal(processes, &process))
	if process.PID <= 0 || !process.StdinPipe || !process.StdoutPipe {
		t.Fatal("Agent did not retain genuine anonymous stdio pipes", process)
	}
	rpcs, err := os.ReadFile(rpcLog)
	must(t, err)
	if strings.Count(string(rpcs), "initialize\n") != 1 || strings.Count(string(rpcs), "session/new\n") != 1 || strings.Contains(string(rpcs), "session/load") {
		t.Fatalf("connector restart replayed session lifecycle: %s", rpcs)
	}
}
