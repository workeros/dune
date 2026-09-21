package fabricd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

func reservedController(t *testing.T) (*acpController, <-chan queuedRPC, *sessionregistry.Registry, api.SubmissionKey) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	registry, err := sessionregistry.Open(t.Context(), dir, sessionregistry.Options{MaxKeys: 2, MaxControls: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.Close() })
	key := api.SubmissionKey{SubmissionID: "prompt", Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1, RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1}}
	admitTestRuntime(t, registry, key.Target)
	a, requests := queueFixture(t)
	a.reserveControl = func(kind, id string) error {
		return registry.ReserveControl(context.Background(), key.Target, kind, id)
	}
	a.releaseControl = func(kind, id string) {
		if err := registry.ReleaseControl(context.Background(), key.Target, kind, id); err != nil {
			t.Error(err)
		}
	}
	return a, requests, registry, key
}

func TestReservedACPControlsSurviveOrdinaryExhaustionAndDuplicateStorm(t *testing.T) {
	a, requests, registry, key := reservedController(t)
	prompt, err := a.submit(t.Context(), registry, key, api.ACPAction{Action: "prompt", Text: "waiting for permission", ExpectedConversationID: a.snapshot().Conversation.ID}, "host")
	if err != nil {
		t.Fatal(err)
	}
	rpc := takeRPC(t, requests)
	// Fill the ordinary operation log as well as the persistent ordinary key pool.
	for range maxRuntimeOperations - 1 {
		if _, err := a.operations.create(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.operations.create(); err == nil {
		t.Fatal("operation pool was not full")
	}
	a.receive(api.Payload(map[string]any{"jsonrpc": "2.0", "id": "agent-permission", "method": "session/request_permission", "params": map[string]any{"sessionId": "session-a", "options": []any{map[string]string{"optionId": "allow"}}}}))
	state := a.snapshot()
	if len(state.Permissions) != 1 {
		t.Fatal("permission not published", state)
	}
	answer := api.ACPAction{Action: "permission", PermissionID: state.Permissions[0].ID, OptionID: "allow"}
	key.SubmissionID = "capacity-check"
	_, err = a.submit(t.Context(), registry, key, api.ACPAction{Action: "list"}, "host")
	requireSubmissionCode(t, err, "SUBMISSION_CAPACITY_EXHAUSTED")
	for i := range 40 {
		key.SubmissionID = fmt.Sprintf("invalid-%d", i)
		invalid := answer
		invalid.OptionID = "unknown"
		receipt, err := a.submit(t.Context(), registry, key, invalid, "host")
		if err == nil || receipt.Admission != api.SubmissionUnknown {
			t.Fatal("invalid answer spent admission", receipt, err)
		}
	}
	key.SubmissionID = "answer"
	var wg sync.WaitGroup
	receipts := make(chan api.SubmissionReceipt, 20)
	for range 20 {
		wg.Go(func() {
			receipt, err := a.submit(t.Context(), registry, key, answer, "host")
			if err != nil {
				t.Error(err)
			}
			receipts <- receipt
		})
	}
	wg.Wait()
	close(receipts)
	original := api.SubmissionReceipt{}
	for receipt := range receipts {
		if receipt.Admission != api.SubmissionAccepted || receipt.Stage != "written" {
			t.Fatal(receipt)
		}
		if original.OperationRef != "" && original.OperationRef != receipt.OperationRef {
			t.Fatal("duplicate allocated another control")
		}
		original = receipt
	}
	response := takeRPC(t, requests)
	if response.ID != "agent-permission" {
		t.Fatal("wrong permission response", response)
	}
	key.SubmissionID = "other-answer"
	if _, err := a.submit(t.Context(), registry, key, answer, "host"); err == nil {
		t.Fatal("second key answered consumed permission")
	}
	key.SubmissionID = "answer"
	answer.OptionID = "changed"
	_, err = a.submit(t.Context(), registry, key, answer, "host")
	requireSubmissionCode(t, err, "SUBMISSION_CONFLICT")
	// Exact cancel uses its own reservation despite all other pools being full.
	key.SubmissionID = "cancel"
	cancel := api.ACPAction{Action: "cancel", OperationRef: prompt.OperationRef}
	cancelled, err := a.submit(t.Context(), registry, key, cancel, "host")
	if err != nil || cancelled.Stage != "written" {
		t.Fatal(cancelled, err)
	}
	if control := takeRPC(t, requests); control.Method != "session/cancel" {
		t.Fatal(control)
	}
	replyRPC(a, rpc, map[string]string{"stopReason": "cancelled"})
	duplicate, err := a.submit(t.Context(), registry, key, cancel, "host")
	if err != nil || duplicate.OperationRef != cancelled.OperationRef {
		t.Fatal("cancel duplicate lost original receipt", duplicate, err)
	}
	observed, err := registry.Get(t.Context(), key)
	if err != nil || observed != cancelled {
		t.Fatal(observed, err)
	}
	if err := registry.ReserveControl(t.Context(), key.Target, "permission", "new-permission"); err == nil {
		t.Fatal("consumed permission evidence was silently recycled")
	}
	if err := registry.ReserveControl(t.Context(), key.Target, "cancel", "new-prompt"); err == nil {
		t.Fatal("consumed cancel evidence was silently recycled")
	}
	select {
	case extra := <-requests:
		t.Fatal("duplicate/invalid control wrote extra RPC", extra)
	default:
	}
}

func TestClosingQueueReleasesOnlyUnusedControlReservations(t *testing.T) {
	a, requests, registry, key := reservedController(t)
	operation := submitAction(t, a, api.ACPAction{Action: "prompt", Text: "unfinished", ExpectedConversationID: a.snapshot().Conversation.ID})
	_ = takeRPC(t, requests)
	if err := registry.ReserveControl(t.Context(), key.Target, "cancel", "next"); err == nil {
		t.Fatal("missing cancel reservation")
	}
	a.closed()
	if result := waitOperation(t, a, operation); result.State != "unknown" {
		t.Fatal(result)
	}
	if err := registry.ReserveControl(t.Context(), key.Target, "cancel", "next"); err != nil {
		t.Fatal("ended unsent control leaked capacity", err)
	}
}

func requireSubmissionCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("want %s; got %v", code, err)
	}
}
