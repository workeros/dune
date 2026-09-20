package fabricd

import (
	"errors"
	"os"
	"testing"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

func submissionFixture(t *testing.T) (*sessionregistry.Registry, api.SubmissionKey) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r, err := sessionregistry.Open(t.Context(), dir, sessionregistry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	key := api.SubmissionKey{SubmissionID: "saved-before-send", Target: api.SubmissionTarget{
		OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1,
		RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1,
	}}
	return r, key
}

func TestSubmissionUsesOriginalSharedOperationAndRejection(t *testing.T) {
	a, requests := queueFixture(t)
	r, key := submissionFixture(t)
	action := api.ACPAction{Action: "load", SessionID: "session-a"}
	first, err := a.submit(t.Context(), r, key, action, "host")
	if err != nil || first.Admission != api.SubmissionAccepted {
		t.Fatal(first, err)
	}
	rpc := takeRPC(t, requests)
	duplicate, err := a.submit(t.Context(), r, key, action, "host")
	if err != nil || duplicate.OperationRef != first.OperationRef {
		t.Fatal("duplicate changed operation", duplicate, err)
	}
	secondKey := key
	secondKey.SubmissionID = "independent-caller"
	shared, err := a.submit(t.Context(), r, secondKey, action, "host")
	if err != nil || shared.OperationRef != first.OperationRef {
		t.Fatal("equivalent in-flight load did not share original operation", shared, err)
	}
	replyRPC(a, rpc, map[string]any{})
	waitOperation(t, a, api.AgentOperation{Ref: first.OperationRef})
	action.SessionID = "different-session"
	_, err = a.submit(t.Context(), r, key, action, "host")
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "SUBMISSION_CONFLICT" {
		t.Fatal("changed request reused original key", err)
	}
	key.SubmissionID = "invalid-request"
	action = api.ACPAction{Action: "prompt", ExpectedConversationID: "obsolete", Text: "never send"}
	for range 2 {
		rejected, err := a.submit(t.Context(), r, key, action, "host")
		if !errors.As(err, &failure) || failure.Code != "CONVERSATION_CHANGED" || rejected.Admission != api.SubmissionNotAccepted || rejected.ErrorCode != failure.Code {
			t.Fatal("negative admission changed on retry", rejected, err)
		}
	}
	// Plain validation errors have the same durable rejection contract.
	key.SubmissionID = "missing-session"
	rejected, err := a.submit(t.Context(), r, key, api.ACPAction{Action: "load"}, "host")
	if !errors.As(err, &failure) || failure.Code != "INVALID_ARGUMENT" || rejected.Admission != api.SubmissionNotAccepted {
		t.Fatal("validation rejection became unknown", rejected, err)
	}
	select {
	case extra := <-requests:
		t.Fatal("duplicate or rejected submission dispatched another RPC", extra)
	default:
	}
}

func TestSubmissionAdmissionFailureDoesNotDispatch(t *testing.T) {
	a, requests := queueFixture(t)
	var ref string
	a.mu.Lock()
	_, err := a.enqueueWithAdmissionLocked(api.ACPAction{Action: "list"}, func(operation api.AgentOperation) error {
		ref = operation.Ref
		return errors.New("commit acknowledgement lost")
	})
	empty := a.active == nil && len(a.queue) == 0
	a.mu.Unlock()
	if err == nil || ref == "" || !empty {
		t.Fatal("admission failure published queue work", err)
	}
	if result := waitOperation(t, a, api.AgentOperation{Ref: ref}); result.State != "failed" {
		t.Fatal("unconfirmed admission lost the unsent operation fact", result)
	}
	select {
	case extra := <-requests:
		t.Fatal("failed admission reached Agent stdin", extra)
	default:
	}
}
