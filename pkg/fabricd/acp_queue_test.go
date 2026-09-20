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
	// Scheduling tests use a controllable synthetic wire; connection replacement
	// itself is covered by the subprocess and old-connection callback tests.
	a.renewConnection = func() error { a.mu.Lock(); a.reconnecting = false; a.mu.Unlock(); return nil }
	a.state = api.ACPState{Ready: true, SessionID: "session-a", Cwd: "/tmp", CanLoad: true, CanList: true}
	a.conversation.begin(api.ACPAction{Action: "new", Cwd: "/tmp"})
	a.conversation.opened("session-a", "/tmp", "succeeded", nil)
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
	first := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "first"})
	firstRPC := takeRPC(t, requests)
	second := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "second"})
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
	first := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "first"})
	firstRPC := takeRPC(t, requests)
	load := submitAction(t, a, api.ACPAction{Action: "load", SessionID: "session-b"})
	stale := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "must not go to b"})
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
	latest := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", SessionID: "session-b", Text: "new task"})
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
	active := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "active"})
	takeRPC(t, requests)
	var pending api.AgentOperation
	for i := 0; i < maxACPPending; i++ {
		pending = submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "queued"})
	}
	_, err := a.action(api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "overflow"})
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "SUBMISSION_CAPACITY_EXHAUSTED" {
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
	_, err := a.action(api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", SessionID: "session-a", Cwd: "/other", Text: "wrong directory"})
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "STALE_SESSION" {
		t.Fatal("admitted a prompt for another native directory", err)
	}
	first := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "first"})
	firstRPC := takeRPC(t, requests)
	load := submitAction(t, a, api.ACPAction{Action: "load", SessionID: "session-a", Cwd: "/other"})
	queued := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "must remain in original directory"})
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
	current := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", SessionID: "session-a", Cwd: "/other", Text: "current directory"})
	replyRPC(a, takeRPC(t, requests), map[string]string{"stopReason": "end_turn"})
	waitOperation(t, a, current)
}

func TestACPQueuePermissionAndCancelBypassPending(t *testing.T) {
	a, requests := queueFixture(t)
	active := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "active"})
	rpc := takeRPC(t, requests)
	queued := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "queued"})
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
	active := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "first"})
	rpc := takeRPC(t, requests)
	queued := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "must not execute"})
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
	operation := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "quick answer"})
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
	first := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "first"})
	rpc := takeRPC(t, requests)
	second := submitAction(t, a, api.ACPAction{ExpectedConversationID: a.snapshot().Conversation.ID, Action: "prompt", Text: "second"})
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

func TestACPConversationPreconditionAtAdmissionAndDispatch(t *testing.T) {
	a, requests := queueFixture(t)
	c1 := a.snapshot().Conversation.ID
	for _, expected := range []string{"", "not-current"} {
		_, err := a.action(api.ACPAction{Action: "prompt", ExpectedConversationID: expected, Text: "rejected"})
		var failure *api.Error
		want := "CONVERSATION_CHANGED"
		if expected == "" {
			want = "INVALID_ARGUMENT"
		}
		if !errors.As(err, &failure) || failure.Code != want {
			t.Fatalf("precondition: %v", err)
		}
	}
	first := submitAction(t, a, api.ACPAction{Action: "prompt", ExpectedConversationID: c1, Text: "first"})
	firstRPC := takeRPC(t, requests)
	load := submitAction(t, a, api.ACPAction{Action: "load", SessionID: "session-a", Cwd: "/tmp"})
	stale := submitAction(t, a, api.ACPAction{Action: "prompt", ExpectedConversationID: c1, Text: "queued stale"})
	replyRPC(a, firstRPC, map[string]string{"stopReason": "end_turn"})
	waitOperation(t, a, first)
	loadRPC := takeRPC(t, requests)
	replyRPC(a, loadRPC, map[string]any{})
	waitOperation(t, a, load)
	if result := waitOperation(t, a, stale); result.State != "failed" || result.ErrorCode != "CONVERSATION_CHANGED" {
		t.Fatalf("queued precondition: %+v", result)
	}
	_, err := a.action(api.ACPAction{Action: "prompt", ExpectedConversationID: c1, Text: "delayed stale"})
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "CONVERSATION_CHANGED" {
		t.Fatal("late admission", err)
	}
	if a.snapshot().Conversation.ID == c1 || len(conversationPage(t, a.conversation).Entries) != 0 {
		t.Fatal("same-native reload retained generation or appended rejected prompt")
	}
	select {
	case extra := <-requests:
		t.Fatalf("stale request dispatched: %+v", extra)
	default:
	}
}

func TestACPLoadCoalescesOnlyAdjacentInflightOperations(t *testing.T) {
	a, requests := queueFixture(t)
	load := api.ACPAction{Action: "load", SessionID: "session-a", Cwd: "/tmp"}
	first := submitAction(t, a, load)
	firstRPC := takeRPC(t, requests)
	shared := submitAction(t, a, load)
	if first.Ref != shared.Ref {
		t.Fatal("equivalent in-flight load did not share task")
	}
	_, err := a.operations.wait(context.Background(), api.AgentOperationWait{Ref: shared.Ref, TimeoutMS: 1})
	if err != nil {
		t.Fatal(err)
	}
	intervening := submitAction(t, a, api.ACPAction{Action: "list"})
	later := submitAction(t, a, load)
	if later.Ref == first.Ref {
		t.Fatal("coalesced across intervening operation")
	}
	replyRPC(a, firstRPC, map[string]any{})
	waitOperation(t, a, first)
	list := takeRPC(t, requests)
	if list.Method != "session/list" {
		t.Fatal("changed queue order")
	}
	replyRPC(a, list, map[string]any{"sessions": []any{}})
	waitOperation(t, a, intervening)
	last := takeRPC(t, requests)
	if last.Method != "session/load" {
		t.Fatal("lost later explicit load")
	}
	replyRPC(a, last, map[string]any{})
	waitOperation(t, a, later)
	select {
	case extra := <-requests:
		t.Fatalf("shared load sent extra RPC: %+v", extra)
	default:
	}
}

func TestACPOpenOutcomeIsSettledBeforeExitAndSurvivesRetention(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			a, requests := queueFixture(t)
			op := submitAction(t, a, api.ACPAction{Action: "load", SessionID: "session-a"})
			rpc := takeRPC(t, requests)
			emitText(a, "session-a", "replayed before result")
			if outcome == "succeeded" {
				replyRPC(a, rpc, map[string]any{})
			}
			if outcome == "failed" {
				a.receive(api.Payload(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32000, "message": "load denied"}}))
			}
			a.closed()
			waitOperation(t, a, op)
			page := conversationPage(t, a.conversation)
			if page.Conversation.Phase != "exited" || page.Conversation.OpenOutcome != outcome || len(page.Entries) != 1 {
				t.Fatalf("ordered result/exit: %+v", page.Conversation)
			}
			if (page.Conversation.OpenError == nil) != (outcome == "succeeded") {
				t.Fatal("open error disagrees with outcome")
			}
			a.operations.mu.Lock()
			a.operations.expireLocked(time.Now().Add(operationRetention))
			a.operations.mu.Unlock()
			retained := conversationPage(t, a.conversation)
			if len(retained.Entries) != 1 || rawMessageText(retained.Entries[0].Message.Content[0]) != "replayed before result" {
				t.Fatal("operation expiration erased retained conversation content")
			}
			a.conversation.store.maxEntries = 0
			a.conversation.mutate(func(*conversationModel) {})
			empty := conversationPage(t, a.conversation)
			if empty.Conversation.OpenOutcome != outcome || len(empty.Entries) != 0 || !empty.Conversation.PrefixEvicted {
				t.Fatal("open result expired with output")
			}
		})
	}
}

func TestACPRejectsCallbacksFromOldConnectionWithSameNativeID(t *testing.T) {
	a, _ := queueFixture(t)
	old := a.connection
	fresh := &process.Process{}
	a.mu.Lock()
	a.connection = fresh
	a.conversation.begin(api.ACPAction{Action: "load", SessionID: "session-a", Cwd: "/tmp"})
	a.mu.Unlock()
	update := api.Payload(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-a", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "from fresh connection"}}}})
	if !a.r.acceptACPLineFrom(update, old) || !a.r.acceptACPLineFrom([]byte("invalid old bytes"), old) {
		t.Fatal("old connection affected live parsing")
	}
	if len(conversationPage(t, a.conversation).Entries) != 0 {
		t.Fatal("old callback crossed conversation boundary")
	}
	if !a.r.acceptACPLineFrom(update, fresh) {
		t.Fatal("fresh connection rejected")
	}
	if page := conversationPage(t, a.conversation); len(page.Entries) != 1 || rawMessageText(page.Entries[0].Message.Content[0]) != "from fresh connection" {
		t.Fatal("fresh replay was not retained")
	}
}

func TestACPInFlightCallbacksCannotCrossLoadBoundary(t *testing.T) {
	for _, kind := range []string{"permission", "response"} {
		t.Run(kind, func(t *testing.T) {
			a, _ := queueFixture(t)
			a.mu.Lock()
			a.conversation.begin(api.ACPAction{Action: "load", SessionID: "session-a", Cwd: "/tmp"})
			a.reconnecting = true
			a.state.Busy = "load"
			reply := make(chan acpReply, 1)
			a.pending["old-prompt"], a.methods["old-prompt"] = reply, "session/prompt"
			a.mu.Unlock()
			before := a.snapshot()
			// receive runs after the connection check in acceptACPLineFrom. An
			// action can begin a new model while that admitted line is parsed.
			// inputMu prevents a fresh connection from starting until it returns.
			message := map[string]any{"jsonrpc": "2.0", "id": "old-permission", "method": "session/request_permission", "params": map[string]any{
				"sessionId": "session-a", "options": []map[string]string{{"optionId": "allow", "kind": "allow_once", "name": "Allow"}},
			}}
			if kind == "response" {
				message = map[string]any{"jsonrpc": "2.0", "id": "old-prompt", "result": map[string]string{"stopReason": "end_turn"}}
			}
			a.receive(api.Payload(message))
			after := a.snapshot()
			if len(after.Permissions) != 0 || after.Revision != before.Revision || after.Conversation.Revision != before.Conversation.Revision {
				t.Fatalf("in-flight old callback changed new state: before=%+v after=%+v", before, after)
			}
			select {
			case <-reply:
				t.Fatal("draining connection delivered a reply after the open boundary")
			default:
			}
		})
	}
}
