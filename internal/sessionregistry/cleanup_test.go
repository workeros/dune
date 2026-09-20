package sessionregistry

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func testResources(target api.SubmissionTarget, instance string) CleanupResources {
	return CleanupResources{Instance: instance, TmuxSession: "acp-" + target.RuntimeID,
		Directory: FileIdentity{Path: filepath.Join("/installation/acp/runtimes", target.RuntimeID), Device: 1, Inode: 2},
		Socket:    FileIdentity{Path: "/private/ipc/" + instance, Device: 1, Inode: 3}}
}

func registerCleanupHost(t *testing.T, r *Registry, target api.SubmissionTarget) HostRecord {
	t.Helper()
	if err := r.ReserveRuntime(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{Target: target, Instance: "host", BootID: "boot", PID: 1234,
		Runtime:      api.Runtime{ID: target.RuntimeID, Incarnation: target.RuntimeIncarnation, Generation: target.RuntimeGeneration, State: "exited"},
		Registration: []byte(`{}`), Resources: testResources(target, "host")}
	if err := r.RegisterHost(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordHostRuntime(t.Context(), target, host.Instance, host.Runtime); err != nil {
		t.Fatal(err)
	}
	host.Phase = "exited"
	return host
}

func verifyExited(host HostRecord) error {
	if host.Phase != "exited" {
		return errors.New("host still active")
	}
	return nil
}

func TestForgetAtomicAdmissionSurvivesSaturationCancellationAndDuplicates(t *testing.T) {
	dir := privateDirectory(t)
	r := openRegistry(t, dir, 1)
	other := openRegistry(t, dir, 1)
	key := testKey()
	registerCleanupHost(t, r, key.Target)
	if _, _, err := r.ClaimKey(t.Context(), key, Digest("prompt", nil), "host"); err != nil {
		t.Fatal(err)
	}
	key.SubmissionID = "forget"
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if acquired, receipt, err := r.AcceptForget(ctx, key, "cancelled", verifyExited); err == nil || acquired || receipt.Admission != api.SubmissionUnknown {
		t.Fatal("cancelled request consumed cleanup reservation", receipt, err)
	}
	var verified, winners atomic.Int32
	var group sync.WaitGroup
	for i := range 20 {
		group.Go(func() {
			registry := r
			if i%2 != 0 {
				registry = other
			}
			acquired, receipt, err := registry.AcceptForget(t.Context(), key, "original-cleanup", func(host HostRecord) error {
				verified.Add(1)
				return verifyExited(host)
			})
			if err != nil || receipt.Admission != api.SubmissionAccepted || receipt.OperationRef != "original-cleanup" || receipt.Cleanup == nil || len(receipt.Cleanup.Remaining) != 3 {
				t.Error(receipt, err)
			}
			if acquired {
				winners.Add(1)
			}
		})
	}
	group.Wait()
	if winners.Load() != 1 || verified.Load() != 1 {
		t.Fatal("duplicates rechecked lifecycle or executed cleanup", winners.Load(), verified.Load())
	}
	if _, _, err := r.ClaimControl(t.Context(), key, Digest("runtime.forget", nil), ForgetReceiver, ControlForget, ""); err == nil {
		t.Fatal("cleanup allowed a non-atomic claim path")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = openRegistry(t, dir, 1)
	jobs, err := r.PendingCleanups(t.Context())
	if err != nil || len(jobs) != 1 || jobs[0].Key != key || jobs[0].Confirmed != 0 || jobs[0].Term != 0 {
		t.Fatal("read changed cleanup or lost its fixed plan", jobs, err)
	}
	if jobs[0].Host.Resources != testResources(key.Target, "host") {
		t.Fatal("cleanup inferred new resources after reopen", jobs[0])
	}
}

func TestForgetVerificationAndSealSerializeAgainstGroupRegistration(t *testing.T) {
	dir := privateDirectory(t)
	r := openRegistry(t, dir, 2)
	other := openRegistry(t, dir, 2)
	key := testKey()
	if err := r.ReserveRuntime(t.Context(), key.Target); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{Target: key.Target, Instance: "host", BootID: "boot", PID: 1234,
		Runtime:      api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration, State: "running"},
		Registration: []byte(`{}`), Resources: testResources(key.Target, "host")}
	if err := r.RegisterHost(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecordGroup(t.Context(), key.Target, host.Instance, 0, 5678); err != nil {
		t.Fatal(err)
	}
	// A failed proof consumes neither the key nor the sole cleanup reservation.
	if acquired, receipt, err := r.AcceptForget(t.Context(), key, "original", verifyExited); err == nil || acquired || receipt.Admission != api.SubmissionUnknown {
		t.Fatal(receipt, err)
	}
	proofStarted, releaseProof := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-releaseProof:
		default:
			close(releaseProof)
		}
	}()
	accepted := make(chan error, 1)
	go func() {
		_, _, err := r.AcceptForget(t.Context(), key, "original", func(snapshot HostRecord) error {
			if snapshot.GroupID != 5678 || snapshot.GroupGeneration != 1 {
				return errors.New("loss proof saw the wrong group")
			}
			close(proofStarted)
			<-releaseProof
			return nil // an observational kernel-loss proof succeeded
		})
		accepted <- err
	}()
	<-proofStarted
	groupStarted, groupResult := make(chan struct{}), make(chan error, 1)
	go func() {
		close(groupStarted)
		_, err := other.RecordGroup(t.Context(), key.Target, host.Instance, 1, 9999)
		groupResult <- err
	}()
	<-groupStarted
	close(releaseProof)
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	requireCode(t, <-groupResult, "STALE_RUNTIME")
	if err := other.RecordHostRuntime(t.Context(), key.Target, host.Instance, host.Runtime); err == nil {
		t.Fatal("late host publication crossed the cleanup seal")
	}
}

func TestForgetCrossReceiverConflictPrecedesLiveOrLostProof(t *testing.T) {
	for _, decision := range []string{"claimed", "accepted", "not_accepted"} {
		t.Run(decision, func(t *testing.T) {
			r := openRegistry(t, privateDirectory(t), 1)
			key := testKey()
			registerCleanupHost(t, r, key.Target)
			claim, _, err := r.ClaimKey(t.Context(), key, Digest("prompt", nil), "original-host")
			if err != nil {
				t.Fatal(err)
			}
			switch decision {
			case "accepted":
				_, err = r.Accept(t.Context(), claim, "prompt")
			case "not_accepted":
				_, err = r.Reject(t.Context(), claim, "INVALID_ARGUMENT")
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, proof := range []error{nil, errors.New("host is alive")} {
				_, _, err := r.AcceptForget(t.Context(), key, "forget", func(HostRecord) error {
					t.Error("conflicting key reached lifecycle verification")
					return proof
				})
				requireCode(t, err, "SUBMISSION_CONFLICT")
			}
			original, err := r.Get(t.Context(), key)
			if err != nil || original.OperationRef == "forget" {
				t.Fatal(original, err)
			}
			key.SubmissionID = "new-cleanup-key"
			if acquired, _, err := r.AcceptForget(t.Context(), key, "forget", verifyExited); err != nil || !acquired {
				t.Fatal("conflict consumed reserved cleanup", err)
			}
		})
	}
}

func TestCleanupCheckpointsFenceOldExecutorsAndPreserveReceiptAfterCompletion(t *testing.T) {
	dir := privateDirectory(t)
	r := openRegistry(t, dir, 1)
	key := testKey()
	registerCleanupHost(t, r, key.Target)
	if _, _, err := r.AcceptForget(t.Context(), key, "original", verifyExited); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Progress(t.Context(), key, "original", "completed", "", nil); err == nil {
		t.Fatal("generic progress fabricated cleanup completion")
	}
	job, err := r.BindCleanup(t.Context(), key, "original", 1)
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range cleanupSteps {
		old := job
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		r = openRegistry(t, dir, 1)
		before, err := r.Get(t.Context(), key)
		if err != nil || len(before.Cleanup.Confirmed) != i || before.Stage != "cleaning" {
			t.Fatal(before, err)
		}
		job, err = r.BindCleanup(t.Context(), key, "original", uint64(i+2))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.CheckpointCleanup(t.Context(), old, step, ""); err == nil {
			t.Fatal("stale executor published a result")
		}
		failed, err := r.CheckpointCleanup(t.Context(), job, step, "CLEANUP_UNCONFIRMED")
		if err != nil || !reflect.DeepEqual(failed.Cleanup, before.Cleanup) {
			t.Fatal("unconfirmed step advanced", failed, err)
		}
		confirmed, err := r.CheckpointCleanup(t.Context(), job, step, "")
		if err != nil || len(confirmed.Cleanup.Confirmed) != i+1 || confirmed.ErrorCode != "" {
			t.Fatal(confirmed, err)
		}
		job.Confirmed++
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = openRegistry(t, dir, 1)
	receipt, err := r.Get(t.Context(), key)
	if err != nil || receipt.Stage != "completed" || len(receipt.Cleanup.Remaining) != 0 {
		t.Fatal(receipt, err)
	}
	jobs, err := r.PendingCleanups(t.Context())
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	if acquired, duplicate, err := r.AcceptForget(t.Context(), key, "another-ref", func(HostRecord) error {
		t.Error("completed duplicate verified missing resources")
		return nil
	}); err != nil || acquired || !reflect.DeepEqual(receipt, duplicate) {
		t.Fatal(duplicate, err)
	}
	if err := r.ReserveRuntime(t.Context(), key.Target); err == nil {
		t.Fatal("completed identity could execute again")
	}
	var live, sealed int
	if err := r.db.QueryRow(`SELECT live,sealed FROM runtime_reservations WHERE target=?`, encodeTarget(key.Target)).Scan(&live, &sealed); err != nil || live != 0 || sealed != 1 {
		t.Fatal(live, sealed, err)
	}
}
