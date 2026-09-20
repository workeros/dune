package sessionregistry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestStopAdmissionIsAtomicAndUsesOnlyItsOwnReservation(t *testing.T) {
	dir := privateDirectory(t)
	r := openRegistry(t, dir, 1)
	key := testKey()
	if err := r.ReserveRuntime(t.Context(), key.Target); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ClaimKey(t.Context(), key, Digest("prompt", nil), "host"); err != nil {
		t.Fatal(err)
	}
	key.SubmissionID = "stop"
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if acquired, _, err := r.AcceptStop(ctx, key, Digest("runtime.stop", nil), "host", "cancelled-ref"); err == nil || acquired {
		t.Fatal("cancelled admission acquired the stop slot", err)
	}
	var winners atomic.Int32
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			acquired, receipt, err := r.AcceptStop(t.Context(), key, Digest("runtime.stop", nil), "host", "original-stop")
			if err != nil || receipt.Admission != api.SubmissionAccepted || receipt.Stage != "stopping" || receipt.OperationRef != "original-stop" {
				t.Error("stop admission lost atomic evidence", receipt, err)
			}
			if acquired {
				winners.Add(1)
			}
		})
	}
	group.Wait()
	if winners.Load() != 1 {
		t.Fatal("stop executed more than once", winners.Load())
	}
	if _, err := r.Progress(t.Context(), key, "original-stop", "stopped", "", nil); err != nil {
		t.Fatal(err)
	}
	if acquired, receipt, err := r.AcceptStop(t.Context(), key, Digest("runtime.stop", nil), "host", "duplicate-ref"); err != nil || acquired || receipt.Stage != "stopped" || receipt.OperationRef != "original-stop" {
		t.Fatal("duplicate regressed the original result", receipt, err)
	}
	key.SubmissionID = "another-stop"
	_, _, err := r.AcceptStop(t.Context(), key, Digest("runtime.stop", nil), "host", "another-ref")
	requireCode(t, err, "CONTROL_UNAVAILABLE")
	key.SubmissionID = "forget"
	if claim, _, err := r.ClaimControl(t.Context(), key, Digest("forget", nil), "registry", ControlForget, ""); err != nil || !claim.Acquired() {
		t.Fatal("stop consumed the independent cleanup reservation", err)
	}
}

func TestProtectedReservationsSurviveOrdinaryAndOtherControlExhaustion(t *testing.T) {
	r, err := Open(t.Context(), privateDirectory(t), Options{MaxKeys: 1, MaxControls: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	key := testKey()
	if err := r.ReserveRuntime(t.Context(), key.Target); err != nil {
		t.Fatal(err)
	}
	if err := r.ReserveControl(t.Context(), key.Target, ControlCancel, "operation"); err != nil {
		t.Fatal(err)
	}
	if err := r.ReserveControl(t.Context(), key.Target, ControlPermission, "permission"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ClaimKey(t.Context(), key, Digest("prompt", nil), "host"); err != nil {
		t.Fatal(err)
	}
	key.SubmissionID = "over-limit"
	_, _, err = r.ClaimKey(t.Context(), key, Digest("prompt", nil), "host")
	requireCode(t, err, "SUBMISSION_CAPACITY_EXHAUSTED")
	if receipt, err := r.Get(t.Context(), key); err != nil || receipt.Admission != api.SubmissionUnknown {
		t.Fatal("ordinary pressure blocked reads or manufactured rejection", receipt, err)
	}
	for _, item := range []struct{ kind, id string }{{ControlPermission, "permission"}, {ControlCancel, "operation"}, {ControlStop, ""}, {ControlForget, ""}} {
		key.SubmissionID = item.kind
		digest := Digest(item.kind, []byte(item.id))
		claim, _, err := r.ClaimControl(t.Context(), key, digest, "host", item.kind, item.id)
		if err != nil || !claim.Acquired() {
			t.Fatal("ordinary exhaustion consumed protected slot", item, err)
		}
		if _, err := r.Accept(t.Context(), claim, item.kind+"-original"); err != nil {
			t.Fatal(err)
		}
		duplicate, receipt, err := r.ClaimControl(t.Context(), key, digest, "host", item.kind, item.id)
		if err != nil || duplicate.Acquired() || receipt.OperationRef != item.kind+"-original" {
			t.Fatal("control duplicate changed receipt", receipt, err)
		}
		other := key
		other.SubmissionID += "-another"
		_, _, err = r.ClaimControl(t.Context(), other, digest, "host", item.kind, item.id)
		requireCode(t, err, "CONTROL_UNAVAILABLE")
		_, _, err = r.ClaimKey(t.Context(), key, Digest("prompt", nil), "host")
		requireCode(t, err, "SUBMISSION_CONFLICT")
	}
	if err := r.ReleaseControl(t.Context(), key.Target, ControlPermission, "permission"); err != nil {
		t.Fatal(err)
	}
	err = r.ReserveControl(t.Context(), key.Target, ControlPermission, "new-permission")
	requireCode(t, err, "CONTROL_CAPACITY_EXHAUSTED")
}

func TestInvalidControlsCannotConsumeAnotherTargetsReservedAnswer(t *testing.T) {
	r := openRegistry(t, privateDirectory(t), 1)
	key := testKey()
	if err := r.ReserveRuntime(t.Context(), key.Target); err != nil {
		t.Fatal(err)
	}
	if err := r.ReserveControl(t.Context(), key.Target, ControlPermission, "valid"); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		key.SubmissionID = string(rune('A' + i%26))
		_, _, err := r.ClaimControl(t.Context(), key, Digest("permission", nil), "host", ControlPermission, "expired")
		requireCode(t, err, "CONTROL_UNAVAILABLE")
	}
	key.SubmissionID = "valid-answer"
	claim, _, err := r.ClaimControl(t.Context(), key, Digest("permission", nil), "host", ControlPermission, "valid")
	if err != nil || !claim.Acquired() {
		t.Fatal("invalid answers exhausted another target", err)
	}
	key.SubmissionID = "ordinary-still-available"
	claim, _, err = r.ClaimKey(t.Context(), key, Digest("prompt", nil), "host")
	if err != nil || !claim.Acquired() {
		t.Fatal("invalid controls grew ordinary key evidence", err)
	}
}

func TestLaunchAndForgetEvidenceOutliveRuntimeResources(t *testing.T) {
	dir := privateDirectory(t)
	r := openRegistry(t, dir, 4)
	key := testKey()
	runtime := api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration, Adapter: "acp", State: "starting"}
	launch := key
	launch.Target.RuntimeID, launch.Target.RuntimeIncarnation, launch.Target.RuntimeGeneration = "", "", 0
	claim, _, err := r.ClaimKey(t.Context(), launch, Digest("launch", nil), "registry")
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := r.AcceptLaunch(t.Context(), claim, "launch-operation", runtime)
	if err != nil || accepted.Runtime.ID != runtime.ID || accepted.Stage != "accepted" {
		t.Fatal(accepted, err)
	}
	// A key held before forget cannot commit admission after the seal.
	late := key
	late.SubmissionID = "late-prompt"
	lateClaim, _, err := r.ClaimKey(t.Context(), late, Digest("prompt", nil), "host")
	if err != nil {
		t.Fatal(err)
	}
	key.SubmissionID = "forget-before-send"
	claim, _, err = r.ClaimControl(t.Context(), key, Digest("forget", nil), "registry", ControlForget, "")
	if err != nil {
		t.Fatal(err)
	}
	accepted, err = r.AcceptForget(t.Context(), claim, "cleanup-original")
	if err != nil || accepted.Stage != "cleaning" {
		t.Fatal(accepted, err)
	}
	if _, err := r.Accept(t.Context(), lateClaim, "must-not-dispatch"); err == nil {
		t.Fatal("late host claim crossed cleanup admission")
	}
	_, _, err = r.ClaimKey(t.Context(), key, Digest("prompt", nil), "host")
	requireCode(t, err, "SUBMISSION_CONFLICT")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = openRegistry(t, dir, 4)
	observed, err := r.Get(t.Context(), key)
	if err != nil || observed.Admission != api.SubmissionAccepted || observed.Stage != "cleaning" || observed.OperationRef != "cleanup-original" {
		t.Fatal("cleanup crash lost admission or fabricated completion", observed, err)
	}
	finished, err := r.Progress(t.Context(), key, "cleanup-original", "completed", "", nil)
	if err != nil || finished.Stage != "completed" {
		t.Fatal(finished, err)
	}
	if err := r.ReserveRuntime(t.Context(), key.Target); err == nil {
		t.Fatal("forgotten Runtime identity was reused")
	}
	if _, err := r.Progress(t.Context(), key, "cleanup-original", "cleaning", "", nil); err == nil {
		t.Fatal("completed cleanup regressed")
	}
	duplicate, receipt, err := r.ClaimControl(t.Context(), key, Digest("forget", nil), "registry", ControlForget, "")
	if err != nil || duplicate.Acquired() || receipt.Stage != "completed" {
		t.Fatal("forgotten Runtime lost its independent duplicate receipt", receipt, err)
	}
	var failure *api.Error
	_, _, err = r.ClaimKey(t.Context(), late, Digest("changed", nil), "host")
	if !errors.As(err, &failure) || failure.Code != "SUBMISSION_CONFLICT" {
		t.Fatal(err)
	}
}
