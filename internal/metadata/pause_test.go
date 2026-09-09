package metadata

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/fabric"
)

func TestManagedPauseFansOutAccessClosureToLiveInstances(t *testing.T) {
	ctx := context.Background()
	store, _, user, sessionHash := managedFixture(t, "postgres")
	created, machine := readyManagedResource(t, store, user, sessionHash)
	if _, err := store.db.ExecContext(ctx, `UPDATE dune_managed_resources SET state='ready',capabilities=$2 WHERE runner_id=$1`, created.Runner.ID, `{"pause_resume":true,"disk_snapshot":false}`); err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("a", 64)
	first, second := wire.ID(), wire.ID()
	if _, err := store.RegisterInstance(ctx, InstanceConfig{BootID: first, Fingerprint: fingerprint}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterInstance(ctx, InstanceConfig{BootID: second, Fingerprint: fingerprint}); err != nil {
		t.Fatal(err)
	}
	selected, err := store.RunnerResource(ctx, created.Runner.ID)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := store.ManagedResource(ctx, created.Runner.ID)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := store.CreateManagedPauseResume(ctx, user, sessionHash, wire.ID(), selected, resource, first, "pause")
	if err != nil {
		t.Fatal(err)
	}
	for _, instanceID := range []string{first, second} {
		closures, err := store.PendingManagedAccessClosures(ctx, instanceID, 32)
		if err != nil || len(closures) != 1 || closures[0].OperationID != paused.ID || closures[0].MachineID != machine.ID {
			t.Fatal("live instance did not receive pause closure", instanceID, closures, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE dune_managed_resources SET access_suspended=FALSE WHERE runner_id=$1`, created.Runner.ID); err != nil {
		t.Fatal(err)
	}
	if closures, err := store.PendingManagedAccessClosures(ctx, second, 32); err != nil || len(closures) != 0 {
		t.Fatal("stale pause closure survived resumed access", closures, err)
	}
}

func TestManagedPauseResumeSuspendsAccessUntilReconnect(t *testing.T) {
	ctx := context.Background()
	store, _, user, sessionHash := managedFixture(t, "sqlite")
	created, machine := readyManagedResource(t, store, user, sessionHash)
	if _, err := store.db.ExecContext(ctx, `UPDATE dune_managed_resources SET state='ready',capabilities=$2 WHERE runner_id=$1`, created.Runner.ID, `{"pause_resume":true,"disk_snapshot":false}`); err != nil {
		t.Fatal(err)
	}
	selected, err := store.RunnerResource(ctx, created.Runner.ID)
	if err != nil || selected.Runner.Binding == nil {
		t.Fatal(err)
	}
	resource, err := store.ManagedResource(ctx, created.Runner.ID)
	if err != nil {
		t.Fatal(err)
	}
	access, err := store.CreateRunnerAccess(ctx, wire.ID(), sessionHash, user.ID, user.Namespace, *selected.Runner.Binding, time.Now().Add(time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}

	instanceID := wire.ID()
	paused, err := store.CreateManagedPauseResume(ctx, user, sessionHash, wire.ID(), selected, resource, instanceID, "pause")
	if err != nil || paused.MachineID != machine.ID || !paused.Exclusive {
		t.Fatal("pause was not accepted", paused, err)
	}
	if valid, err := store.CheckAccess(ctx, access, time.Now().Unix()); err != nil || valid {
		t.Fatal("existing access survived pause", valid, err)
	}
	closures, err := store.PendingManagedAccessClosures(ctx, instanceID, 32)
	if err != nil || len(closures) != 1 || closures[0].OperationID != paused.ID || closures[0].MachineID != machine.ID {
		t.Fatal("pause did not fan out existing stream closure", closures, err)
	}
	if err := store.ConfirmManagedAccessClosed(ctx, closures[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRunnerAccess(ctx, wire.ID(), sessionHash, user.ID, user.Namespace, *selected.Runner.Binding, time.Now().Add(time.Minute).Unix()); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("new access was issued while paused", err)
	}
	claimed, err := store.ClaimOperation(ctx, paused.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pauseAction, dispatch, err := store.BeginProviderAction(ctx, claimed, lifecycle.ActionRequest{Kind: "pause", Digest: claimed.Digest})
	if err != nil || !dispatch {
		t.Fatal("pause action", dispatch, err)
	}
	capabilities := &fabric.ResourceCapabilities{PauseResume: true}
	if err := store.RecordProviderAction(ctx, claimed, pauseAction.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: resource.Ref, ExpiresAt: resource.ExpiresAt, State: "paused", Capabilities: capabilities}); err != nil {
		t.Fatal(err)
	}

	resource, err = store.ManagedResource(ctx, created.Runner.ID)
	if err != nil || resource.State != "paused" || !resource.AccessSuspended {
		t.Fatal("paused resource facts", resource, err)
	}
	inspectionClaim, err := store.ClaimManagedInspection(ctx, created.Runner.ID, "personal-v1", wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pausedExpiry := time.Now().Add(5 * time.Minute)
	schedule, err := store.RecordManagedInspection(ctx, inspectionClaim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{
		Status: lifecycle.InspectionConfirmed, ResourceRef: resource.Ref, ExpiresAt: pausedExpiry, State: "paused", Capabilities: capabilities,
	})
	if err != nil || schedule.RenewUntil.IsZero() {
		t.Fatal("paused resource did not retain renewal scheduling", schedule, err)
	}
	renewal, consumed, err := store.ClaimScheduledManagedRenewal(ctx, schedule, "personal-v1", wire.ID(), time.Minute)
	if err != nil || !consumed || renewal.ID == "" {
		t.Fatal("paused renewal was not accepted", renewal, consumed, err)
	}
	renewAction, dispatch, expired, err := store.BeginManagedRenewal(ctx, renewal)
	if err != nil || !dispatch || expired {
		t.Fatal("paused renewal action", dispatch, expired, err)
	}
	if err := store.RecordProviderAction(ctx, renewal, renewAction.ID, lifecycle.ActionObservation{Outcome: "unknown", ResourceRef: resource.Ref, ExpiresAt: pausedExpiry, State: "paused", Capabilities: capabilities}); err != nil {
		t.Fatal(err)
	}
	deferredRenewal, err := store.Operation(ctx, renewal.ID)
	if err != nil || deferredRenewal.Exclusive || deferredRenewal.Finished {
		t.Fatal("paused renewal blocked resume instead of awaiting post-resume facts", deferredRenewal, err)
	}
	resumed, err := store.CreateManagedPauseResume(ctx, user, sessionHash, wire.ID(), selected, resource, instanceID, "resume")
	if err != nil {
		t.Fatal("resume was not accepted", err)
	}
	claimed, err = store.ClaimOperation(ctx, resumed.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resumeAction, dispatch, err := store.BeginProviderAction(ctx, claimed, lifecycle.ActionRequest{Kind: "resume", Digest: claimed.Digest})
	if err != nil || !dispatch {
		t.Fatal("resume action", dispatch, err)
	}
	if err := store.RecordProviderAction(ctx, claimed, resumeAction.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: resource.Ref, ExpiresAt: resource.ExpiresAt, State: "ready", Capabilities: capabilities}); err != nil {
		t.Fatal(err)
	}
	waiting, err := store.Operation(ctx, resumed.ID)
	if err != nil || waiting.Finished || waiting.Exclusive {
		t.Fatal("resume did not wait for reconnect", waiting, err)
	}
	if err := store.ConfirmMachineOnline(ctx, onlineBinding(machine)); err != nil {
		t.Fatal("resume reconnect:", err)
	}
	finished, err := store.Operation(ctx, resumed.ID)
	resource, resourceErr := store.ManagedResource(ctx, created.Runner.ID)
	if err != nil || resourceErr != nil || !finished.Finished || finished.Outcome != "succeeded" || resource.AccessSuspended || resource.State != "ready" {
		t.Fatal("resume did not restore access", finished, resource, err, resourceErr)
	}
	requeuedRenewal, err := store.Operation(ctx, renewal.ID)
	if err != nil || !requeuedRenewal.Exclusive || requeuedRenewal.Finished || requeuedRenewal.Worker != "" || !requeuedRenewal.Until.IsZero() {
		t.Fatal("resume did not requeue the original renewal for observation", requeuedRenewal, err)
	}
}
