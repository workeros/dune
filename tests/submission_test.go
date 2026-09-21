package tests

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

func TestManagedSubmissionAdmissionThroughGateway(t *testing.T) {
	h := start(t)
	mock := mockACPBinary(t)
	p := profile(h.dir, "acp", mock)
	p.ManagedACP = true
	rpcLog := filepath.Join(h.dir, "rpc.log")
	p.Env = map[string]string{"DUNE_MOCK_HISTORY": "1", "DUNE_MOCK_RPC_LOG": rpcLog}
	runtime, initial, err := testStartProfile(h.client, h.ctx, p)
	must(t, err)
	initial.Close()
	defer testStopRuntime(h.client, h.ctx, runtime)
	waitManagedACPReady(t, h, runtime)
	key := api.SubmissionKey{SubmissionID: "open-before-send", Target: api.SubmissionTarget{
		OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: h.c.Target, BindingRevision: 1,
		RuntimeID: runtime.ID, RuntimeIncarnation: runtime.Incarnation, RuntimeGeneration: runtime.Generation,
	}}
	request := api.SubmissionRequest{SubmissionKey: key, Operation: "acp.action", Payload: api.Payload(api.ACPAction{Action: "new", Cwd: h.dir})}
	opened, err := h.client.Submit(h.ctx, request)
	must(t, err)
	operation, err := h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: opened.OperationRef, TimeoutMS: 3000})
	must(t, err)
	if operation.State != "completed" {
		t.Fatal("open failed", operation)
	}
	state, err := h.client.ACPState(h.ctx, runtime)
	must(t, err)
	key.SubmissionID = "prompt-before-send"
	request.SubmissionKey = key
	request.Payload = api.Payload(api.ACPAction{Action: "prompt", Text: "exactly one prompt", ExpectedConversationID: state.Conversation.ID})
	accepted, err := h.client.Submit(h.ctx, request)
	must(t, err)
	operation, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: accepted.OperationRef, TimeoutMS: 3000})
	must(t, err)
	if operation.State != "completed" {
		t.Fatal("prompt failed", operation)
	}
	// Reconnect a caller, read its pre-send key, and try an explicit duplicate.
	h.client.Close()
	h.reconnect()
	observed, err := h.client.QuerySubmission(h.ctx, key)
	must(t, err)
	duplicate, err := h.client.Submit(h.ctx, request)
	must(t, err)
	if observed.OperationRef != accepted.OperationRef || duplicate.OperationRef != accepted.OperationRef {
		t.Fatal("original submission was rebound", observed, duplicate)
	}
	request.Payload = api.Payload(api.ACPAction{Action: "prompt", Text: "different prompt", ExpectedConversationID: state.Conversation.ID})
	_, err = h.client.Submit(h.ctx, request)
	var failure *api.Error
	var submissionError *api.SubmissionError
	if !errors.As(err, &failure) || failure.Code != "SUBMISSION_CONFLICT" || !errors.As(err, &submissionError) || submissionError.Key != key {
		t.Fatal("conflict lost original key or executed changed payload", err)
	}
	key.SubmissionID = "stale-before-send"
	request.SubmissionKey = key
	request.Payload = api.Payload(api.ACPAction{Action: "prompt", Text: "never execute", ExpectedConversationID: "obsolete"})
	for range 2 {
		rejected, err := h.client.Submit(h.ctx, request)
		if !errors.As(err, &failure) || failure.Code != "CONVERSATION_CHANGED" || rejected.Admission != api.SubmissionNotAccepted || rejected.SubmissionKey != key {
			t.Fatal("durable rejection did not survive network round trip", rejected, err)
		}
	}
	observed, err = h.client.QuerySubmission(h.ctx, key)
	must(t, err)
	if observed.Admission != api.SubmissionNotAccepted || observed.ErrorCode != "CONVERSATION_CHANGED" {
		t.Fatal("query reversed rejection", observed)
	}
	log, err := os.ReadFile(rpcLog)
	must(t, err)
	if strings.Count(string(log), "session/new\n") != 1 || strings.Count(string(log), "session/prompt\n") != 1 {
		t.Fatalf("duplicates reached Agent: %s", log)
	}
}

// This tracer bullet exercises public receipt reads against durable fixture
// evidence. It does not yet claim that Agent submission or process survival is
// implemented: no Agent is started, and the admission fixture uses the registry.
func TestSubmissionQueryAcrossFabricdRestart(t *testing.T) {
	h := start(t)
	key := api.SubmissionKey{SubmissionID: "saved-before-send", Target: api.SubmissionTarget{
		OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric",
		MachineID: h.c.Target, BindingRevision: 1,
		RuntimeID: "original-runtime", RuntimeIncarnation: "original-incarnation", RuntimeGeneration: 1,
	}}
	receipt, err := h.client.QuerySubmission(h.ctx, key)
	must(t, err)
	if receipt.Admission != api.SubmissionUnknown || receipt.SubmissionKey != key {
		t.Fatal("missing receipt was treated as a negative admission", receipt)
	}
	runtime := api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration}
	const readID = "same-transport-read"
	must(t, h.client.CallID(h.ctx, "submission.get", readID, key, &receipt, &runtime))
	registry, err := sessionregistry.Open(h.ctx, filepath.Join(h.c.SessionDir, "registry"), sessionregistry.Options{})
	must(t, err)
	defer registry.Close()
	claim, _, err := registry.ClaimKey(h.ctx, key, sessionregistry.Digest("prompt", []byte("fixture")), "original-host")
	must(t, err)
	if !claim.Acquired() {
		t.Fatal("read-only queries consumed the admission key")
	}
	_, err = registry.Accept(h.ctx, claim, "original-operation")
	must(t, err)
	must(t, h.client.CallID(h.ctx, "submission.get", readID, key, &receipt, &runtime))
	if receipt.Admission != api.SubmissionAccepted || receipt.OperationRef != "original-operation" {
		t.Fatal("transport cache hid newer admission evidence", receipt)
	}
	previous := h.client.Binding.Incarnation
	h.client.Close()
	h.stopProcess("fabricd", syscall.SIGKILL)
	h.startProcess("fabricd")
	h.reconnect()
	if h.client.Binding.Incarnation == previous {
		t.Fatal("test did not replace the connector process")
	}
	receipt, err = h.client.QuerySubmission(h.ctx, key)
	must(t, err)
	if receipt.Admission != api.SubmissionAccepted || receipt.OperationRef != "original-operation" || receipt.SubmissionKey != key {
		t.Fatal("restart lost or rebound the original receipt", receipt)
	}
	runtimes, err := h.client.List(h.ctx)
	must(t, err)
	if len(runtimes.Items) != 0 {
		t.Fatal("receipt lookup created a replacement Runtime", runtimes)
	}
}
