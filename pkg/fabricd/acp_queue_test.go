package fabricd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/pkg/api"
)

type queuedRPC struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func queueFixture(t *testing.T) (*acpController, <-chan queuedRPC) {
	t.Helper()
	reader, writer := io.Pipe()
	r := &runtime{cwd: "/tmp", subs: map[*subscription]bool{}, p: &process.Process{Input: writer}}
	a := newACPController(r)
	r.acp = a
	a.state = api.ACPState{Ready: true, SessionID: "session-a", Cwd: "/tmp", CanLoad: true, CanList: true}
	requests := make(chan queuedRPC, 64)
	go func() {
		defer close(requests)
		decoder := json.NewDecoder(reader)
		for {
			var request queuedRPC
			if decoder.Decode(&request) != nil {
				return
			}
			requests <- request
		}
	}()
	t.Cleanup(func() { a.closed(); reader.Close(); writer.Close() })
	return a, requests
}

func takeRPC(t *testing.T, requests <-chan queuedRPC) queuedRPC {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("missing ACP RPC")
		return queuedRPC{}
	}
}

func submitAction(t *testing.T, a *acpController, action api.ACPAction) api.AgentOperation {
	t.Helper()
	value, err := a.action(action)
	if err != nil {
		t.Fatal(err)
	}
	return value.(api.AgentOperation)
}

func replyRPC(a *acpController, request queuedRPC, result any) {
	a.receive(api.Payload(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}))
}

func emitText(a *acpController, session, text string) {
	a.receive(api.Payload(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}}}}))
}

func waitOperation(t *testing.T, a *acpController, operation api.AgentOperation) api.AgentOperation {
	t.Helper()
	result, err := a.operations.wait(context.Background(), api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Terminal() {
		t.Fatalf("operation did not finish: %+v", result)
	}
	return result
}

func readOperation(t *testing.T, a *acpController, operation api.AgentOperation) api.AgentOperationOutput {
	t.Helper()
	result, err := a.operations.read(api.AgentOperationRead{Ref: operation.Ref})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestACPQueueSeparatesPromptCompletionAndOutput(t *testing.T) {
	a, requests := queueFixture(t)
	first := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "first"})
	firstRPC := takeRPC(t, requests)
	second := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "second"})
	if second.State != "pending" || a.snapshot().Pending != 1 {
		t.Fatal("second request was not queued")
	}
	emitText(a, "session-a", "only first output")
	before, err := a.operations.wait(context.Background(), api.AgentOperationWait{Ref: second.Ref, TimeoutMS: 1})
	if err != nil || before.State != "pending" {
		t.Fatalf("pending wait: %+v %v", before, err)
	}
	if got := readOperation(t, a, second); got.NextPosition != 0 || len(got.Output) != 0 {
		t.Fatal("first output leaked to second")
	}
	replyRPC(a, firstRPC, map[string]string{"stopReason": "end_turn"})
	if got := waitOperation(t, a, first); got.State != "completed" || got.StopReason != "end_turn" {
		t.Fatalf("first: %+v", got)
	}
	secondRPC := takeRPC(t, requests)
	if secondRPC.Method != "session/prompt" || !strings.Contains(string(secondRPC.Params), "second") {
		t.Fatal("wrong queued RPC")
	}
	if got := readOperation(t, a, second); got.State != "running" || len(got.Output) != 0 {
		t.Fatal("A completion or output satisfied B")
	}
	emitText(a, "session-a", "only second output")
	replyRPC(a, secondRPC, map[string]string{"stopReason": "max_tokens"})
	if got := waitOperation(t, a, second); got.StopReason != "max_tokens" {
		t.Fatalf("second: %+v", got)
	}
	for _, test := range []struct {
		op   api.AgentOperation
		text string
	}{{first, "only first output"}, {second, "only second output"}} {
		got := readOperation(t, a, test.op)
		if got.Incomplete || got.Position != 0 || got.NextPosition != 1 || len(got.Output) != 1 || !strings.Contains(string(got.Output[0]), test.text) {
			t.Fatalf("wrong operation output: %+v", got)
		}
	}
}

func TestACPQueueSerializesLoadAndRejectsRedirectedPrompt(t *testing.T) {
	a, requests := queueFixture(t)
	first := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "first"})
	firstRPC := takeRPC(t, requests)
	load := submitAction(t, a, api.ACPAction{Action: "load", SessionID: "session-b"})
	stale := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "must not go to b"})
	replyRPC(a, firstRPC, map[string]string{"stopReason": "end_turn"})
	waitOperation(t, a, first)
	loadRPC := takeRPC(t, requests)
	if loadRPC.Method != "session/load" {
		t.Fatal("load did not retain queue order")
	}
	emitText(a, "session-b", "native history")
	replyRPC(a, loadRPC, map[string]any{})
	waitOperation(t, a, load)
	if got := waitOperation(t, a, stale); got.State != "failed" {
		t.Fatalf("redirected prompt: %+v", got)
	}
	if got := readOperation(t, a, stale); len(got.Output) != 0 {
		t.Fatal("load replay leaked to pending prompt")
	}
	select {
	case extra := <-requests:
		t.Fatalf("stale prompt was sent: %+v", extra)
	default:
	}
	latest := submitAction(t, a, api.ACPAction{Action: "prompt", SessionID: "session-b", Text: "new task"})
	latestRPC := takeRPC(t, requests)
	emitText(a, "session-b", "new answer")
	replyRPC(a, latestRPC, map[string]string{"stopReason": "end_turn"})
	waitOperation(t, a, latest)
	if got := readOperation(t, a, latest); len(got.Output) != 1 || strings.Contains(string(got.Output[0]), "history") {
		t.Fatal("load replay leaked to next prompt")
	}
}

func TestACPQueueCloseAndBoundedAdmission(t *testing.T) {
	a, requests := queueFixture(t)
	active := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "active"})
	takeRPC(t, requests)
	var pending api.AgentOperation
	for i := 0; i < maxACPPending; i++ {
		pending = submitAction(t, a, api.ACPAction{Action: "prompt", Text: "queued"})
	}
	_, err := a.action(api.ACPAction{Action: "prompt", Text: "overflow"})
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "RESOURCE_EXHAUSTED" {
		t.Fatalf("queue limit: %v", err)
	}
	a.closed()
	if got := waitOperation(t, a, active); got.State != "unknown" {
		t.Fatalf("unconfirmed operation: %+v", got)
	}
	if got := waitOperation(t, a, pending); got.State != "cancelled" {
		t.Fatalf("unsent operation: %+v", got)
	}
	if !readOperation(t, a, active).Incomplete {
		t.Fatal("exit did not mark output incomplete")
	}
}

func TestACPQueuePinsDirectoryEvenWhenNativeIDDoesNotChange(t *testing.T) {
	a, requests := queueFixture(t)
	_, err := a.action(api.ACPAction{Action: "prompt", SessionID: "session-a", Cwd: "/other", Text: "wrong directory"})
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "STALE_SESSION" {
		t.Fatal("admitted a prompt for another native directory", err)
	}
	first := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "first"})
	firstRPC := takeRPC(t, requests)
	load := submitAction(t, a, api.ACPAction{Action: "load", SessionID: "session-a", Cwd: "/other"})
	queued := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "must remain in original directory"})
	replyRPC(a, firstRPC, map[string]string{"stopReason": "end_turn"})
	waitOperation(t, a, first)
	replyRPC(a, takeRPC(t, requests), map[string]any{})
	waitOperation(t, a, load)
	if result := waitOperation(t, a, queued); result.State != "failed" {
		t.Fatal("queued prompt followed a changed directory", result)
	}
	select {
	case request := <-requests:
		t.Fatal("stale prompt reached Agent", request)
	default:
	}
	current := submitAction(t, a, api.ACPAction{Action: "prompt", SessionID: "session-a", Cwd: "/other", Text: "current directory"})
	replyRPC(a, takeRPC(t, requests), map[string]string{"stopReason": "end_turn"})
	waitOperation(t, a, current)
}

func TestACPQueuePermissionAndCancelBypassPending(t *testing.T) {
	a, requests := queueFixture(t)
	active := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "active"})
	rpc := takeRPC(t, requests)
	queued := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "queued"})
	a.receive([]byte(`{"jsonrpc":"2.0","id":"permission","method":"session/request_permission","params":{"sessionId":"session-a","options":[{"optionId":"allow"}]}}`))
	permission := a.snapshot().Permissions[0]
	if _, err := a.action(api.ACPAction{Action: "permission", PermissionID: permission.ID, OptionID: "allow"}); err != nil {
		t.Fatal(err)
	}
	if got := takeRPC(t, requests); got.ID != "permission" {
		t.Fatal("permission queued behind prompt")
	}
	if _, err := a.action(api.ACPAction{Action: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if got := takeRPC(t, requests); got.Method != "session/cancel" {
		t.Fatal("cancel queued behind prompt")
	}
	replyRPC(a, rpc, map[string]string{"stopReason": "cancelled"})
	if got := waitOperation(t, a, active); got.State != "cancelled" {
		t.Fatalf("cancel: %+v", got)
	}
	rpc = takeRPC(t, requests)
	replyRPC(a, rpc, map[string]string{"stopReason": "end_turn"})
	if got := waitOperation(t, a, queued); got.State != "completed" {
		t.Fatal("cancel affected a queued prompt")
	}
}

func TestOperationOutputTruncationAndExpiry(t *testing.T) {
	a, _ := queueFixture(t)
	op, err := a.operations.create()
	if err != nil {
		t.Fatal(err)
	}
	update := api.Payload(strings.Repeat("x", maxOperationBytes/2))
	for i := 0; i < 4; i++ {
		a.operations.append(op.Ref, update)
	}
	got := readOperation(t, a, op)
	if !got.Incomplete || got.Position != 3 || got.NextPosition != 4 || len(got.Output) != 1 {
		t.Fatalf("truncation positions: %+v", got.AgentOperation)
	}
	a.operations.append(op.Ref, api.Payload(strings.Repeat("x", maxOperationBytes)))
	got = readOperation(t, a, op)
	if !got.Incomplete || got.Position != 5 || got.NextPosition != 5 || len(got.Output) != 0 {
		t.Fatal("oversized event gap lost")
	}
	a.operations.append(op.Ref, api.Payload("tail"))
	got = readOperation(t, a, op)
	if got.Position != 5 || got.NextPosition != 6 || len(got.Output) != 1 {
		t.Fatal("positions renumbered after omission")
	}
	a.operations.set(op.Ref, "completed", "end_turn", "")
	a.operations.mu.Lock()
	a.operations.records[op.Ref].finished = time.Now().Add(-operationRetention)
	a.operations.mu.Unlock()
	_, err = a.operations.read(api.AgentOperationRead{Ref: op.Ref})
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "OPERATION_EXPIRED" {
		t.Fatalf("expiry: %v", err)
	}
	other := &operationLog{records: map[string]*operationRecord{}}
	_, err = other.wait(context.Background(), api.AgentOperationWait{Ref: op.Ref})
	if !errors.As(err, &failure) || failure.Code != "OPERATION_EXPIRED" {
		t.Fatal("another Runtime accepted old reference")
	}
}

func TestACPQueueNeverAdvancesOnMalformedCompletion(t *testing.T) {
	a, requests := queueFixture(t)
	active := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "first"})
	rpc := takeRPC(t, requests)
	queued := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "must not execute"})
	replyRPC(a, rpc, map[string]string{})
	if got := waitOperation(t, a, active); got.State != "unknown" {
		t.Fatalf("malformed response: %+v", got)
	}
	if got := waitOperation(t, a, queued); got.State != "cancelled" {
		t.Fatal("queue advanced after unknown boundary")
	}
}

func TestACPQueueKeepsMatchedResultWhenAgentExitsImmediately(t *testing.T) {
	a, requests := queueFixture(t)
	operation := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "quick answer"})
	rpc := takeRPC(t, requests)
	emitText(a, "session-a", "final output")
	replyRPC(a, rpc, map[string]string{"stopReason": "end_turn"})
	a.closed()
	if result := waitOperation(t, a, operation); result.State != "completed" || result.Error != "" {
		t.Fatalf("matched response was lost on exit: %+v", result)
	}
}

func TestACPQueueDoesNotAdvanceBeforeControlWriteFinishes(t *testing.T) {
	a, requests := queueFixture(t)
	first := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "first"})
	rpc := takeRPC(t, requests)
	second := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "second"})
	// Hold the same boundary used while sending a cancel/permission response.
	a.mu.Lock()
	a.controlling = true
	a.mu.Unlock()
	replyRPC(a, rpc, map[string]string{"stopReason": "end_turn"})
	waitOperation(t, a, first)
	if result := readOperation(t, a, second); result.State != "pending" {
		t.Fatal("the next prompt overtook a control targeting its predecessor")
	}
	a.finishControl(nil)
	rpc = takeRPC(t, requests)
	replyRPC(a, rpc, map[string]string{"stopReason": "end_turn"})
	waitOperation(t, a, second)
}
