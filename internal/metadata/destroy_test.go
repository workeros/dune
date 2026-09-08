package metadata

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
)

func destroyFixture(t *testing.T, s *Store, user identity.User, sessionHash string) (authorization.Resource, lifecycle.Resource, authorization.ConnectionAccess) {
	t.Helper()
	created, machine := readyManagedResource(t, s, user, sessionHash)
	selected, err := s.RunnerResource(context.Background(), created.Runner.ID)
	if err != nil || selected.Runner.Binding == nil || selected.FabricID != "sandbox" || selected.BindingRevision != 1 {
		t.Fatal("managed Runner did not expose its authoritative lifecycle scope", selected, err)
	}
	resource, err := s.ManagedResource(context.Background(), created.Runner.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.CreateRunnerAccess(context.Background(), wire.ID(), sessionHash, user.ID, user.Namespace, *selected.Runner.Binding, time.Now().Add(time.Minute).Unix())
	if err != nil || record.Target != machine.ID {
		t.Fatal("access setup", record, err)
	}
	claim, err := s.ClaimManagedInspection(context.Background(), created.Runner.ID, "personal-v1", wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := s.RecordManagedInspection(context.Background(), claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{
		Status: lifecycle.InspectionConfirmed, ResourceRef: resource.Ref, ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	if err != nil || schedule.RenewUntil.IsZero() {
		t.Fatal("renewal schedule setup", schedule, err)
	}
	return selected, resource, record
}

func TestManagedDestroyAcceptanceClosesAccessAtomically(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, sessionHash := managedFixture(t, backend)
			selected, resource, accessRecord := destroyFixture(t, s, user, sessionHash)
			other := s
			if backend == "postgres" {
				var err error
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}

			requestKey := wire.ID()
			var mu sync.Mutex
			var results []lifecycle.ManagedDestruction
			var wg sync.WaitGroup
			for i := range 6 {
				wg.Go(func() {
					store := s
					if i%2 == 1 {
						store = other
					}
					result, err := store.CreateManagedDestroy(ctx, user, sessionHash, requestKey, selected, resource, time.Minute)
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					results = append(results, result)
					mu.Unlock()
				})
			}
			wg.Wait()
			if len(results) != 6 {
				t.Fatal("concurrent retries did not all recover acceptance", len(results))
			}
			for _, result := range results[1:] {
				if !reflect.DeepEqual(result, results[0]) {
					t.Fatal("concurrent retry changed destroy intent", results[0], result)
				}
			}
			destroyed := results[0]
			if destroyed.ID == "" || destroyed.Action != "destroy" || destroyed.RunnerID != selected.Runner.ID || destroyed.FabricID != "sandbox" || destroyed.BindingRevision != 1 || !destroyed.Exclusive || destroyed.Finished || destroyed.Outcome != "" || destroyed.ResourceRef != resource.Ref || destroyed.MachineID != selected.Runner.Binding.MachineID || destroyed.AccessCloseOutcome != lifecycle.AccessCloseWaiting || destroyed.AccessClosedAt.IsZero() || destroyed.CloseDeadline.Sub(destroyed.AccessClosedAt) != time.Minute {
				t.Fatal("invalid destroy acceptance", destroyed)
			}
			currentResource, err := s.ManagedResource(ctx, selected.Runner.ID)
			if err != nil || !currentResource.AccessClosed || currentResource.Gone || currentResource.Ref != resource.Ref {
				t.Fatal("destroy acceptance lost or deleted the resource", currentResource, err)
			}
			currentRunner, err := s.RunnerResource(ctx, selected.Runner.ID)
			if err != nil || currentRunner.Runner.Binding != nil || currentRunner.FabricID != "sandbox" || currentRunner.BindingRevision != 1 {
				t.Fatal("destroy acceptance removed the Runner identity or retained its machine", currentRunner, err)
			}
			if valid, err := s.CheckAccess(ctx, accessRecord, time.Now().Unix()); err != nil || valid {
				t.Fatal("accepted destroy retained old access", valid, err)
			}
			var machines, tickets int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_machines WHERE runner_id=$1`, selected.Runner.ID).Scan(&machines); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_access_tickets WHERE runner_id=$1`, selected.Runner.ID).Scan(&tickets); err != nil || machines != 0 || tickets != 0 {
				t.Fatal("access records survived destroy acceptance", machines, tickets, err)
			}
			schedule, err := s.ManagedRenewalSchedule(ctx, selected.Runner.ID)
			if err != nil || schedule.Reason != "DESTROYING" || !schedule.RenewUntil.IsZero() || !schedule.NextCheckAt.IsZero() || schedule.Worker != "" || !schedule.Until.IsZero() {
				t.Fatal("destroy acceptance left renewal active", schedule, err)
			}

			// A retry resolves the original record after the transaction has removed
			// the machine; the machine ID is cleanup state, not part of its digest.
			refreshed, err := s.RunnerResource(ctx, selected.Runner.ID)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := other.CreateManagedDestroy(ctx, user, sessionHash, requestKey, refreshed, currentResource, time.Minute)
			if err != nil || !reflect.DeepEqual(replay, destroyed) {
				t.Fatal("post-commit retry did not recover the destroy", replay, err)
			}
			if _, err := s.CreateManagedDestroy(ctx, user, sessionHash, requestKey, refreshed, currentResource, 2*time.Minute); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("same request key changed the close deadline", err)
			}
			if _, err := s.CreateManagedDestroy(ctx, user, sessionHash, wire.ID(), refreshed, currentResource, time.Minute); !errors.Is(err, lifecycle.ErrBusy) {
				t.Fatal("a second destroy bypassed the Runner mutex", err)
			}
		})
	}
}

func TestManagedDestroyCommitLossIsRecoveredWithoutReopeningAccess(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, sessionHash := managedFixture(t, backend)
			selected, resource, _ := destroyFixture(t, s, user, sessionHash)
			interrupted, commits := lostCommitStore(t, config)
			requestKey := wire.ID()
			result, err := interrupted.CreateManagedDestroy(ctx, user, sessionHash, requestKey, selected, resource, time.Minute)
			if !errors.Is(err, ErrCommitUnknown) || result.ID != "" || commits.Load() != 1 {
				t.Fatal("uncertain destroy returned acceptance", result, commits.Load(), err)
			}
			recovered, err := s.ManagedDestroy(ctx, user.ID, requestKey)
			if err != nil || recovered.ID == "" || recovered.ResourceRef != resource.Ref || recovered.MachineID == "" {
				t.Fatal("uncertain destroy did not preserve recovery facts", recovered, err)
			}
			current, err := s.ManagedResource(ctx, selected.Runner.ID)
			if err != nil || !current.AccessClosed {
				t.Fatal("uncertain acknowledgement reopened access", current, err)
			}
			refreshed, err := s.RunnerResource(ctx, selected.Runner.ID)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := s.CreateManagedDestroy(ctx, user, sessionHash, requestKey, refreshed, current, time.Minute)
			if err != nil || !reflect.DeepEqual(replay, recovered) {
				t.Fatal("uncertain destroy was not reconciled", replay, recovered, err)
			}
		})
	}
}

func TestManagedDestroyWithoutMachineNeedsNoAccessWait(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, sessionHash := managedFixture(t, backend)
			created, claimed := confirmedManagedCreate(t, s, user, sessionHash)
			grant, dispatch, err := s.BeginManagedBootstrap(ctx, claimed, tokenHash("destroy without machine"), time.Minute)
			if err != nil || !dispatch {
				t.Fatal("bootstrap setup", dispatch, err)
			}
			if err := s.RecordProviderAction(ctx, claimed, grant.Action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: grant.Action.ResourceRef}); err != nil {
				t.Fatal(err)
			}
			selected, err := s.RunnerResource(ctx, created.Runner.ID)
			if err != nil || selected.Runner.Binding != nil {
				t.Fatal("unbound Managed Runner setup", selected, err)
			}
			resource, err := s.ManagedResource(ctx, created.Runner.ID)
			if err != nil {
				t.Fatal(err)
			}
			destroyed, err := s.CreateManagedDestroy(ctx, user, sessionHash, wire.ID(), selected, resource, time.Minute)
			if err != nil || destroyed.AccessCloseOutcome != lifecycle.AccessCloseConfirmed || destroyed.MachineID != "" || destroyed.Finished || !destroyed.Exclusive {
				t.Fatal("unbound resource waited for nonexistent access", destroyed, err)
			}
			creation, err := s.Operation(ctx, created.Operation.ID)
			if err != nil || !creation.Finished || creation.Outcome != "failed" || creation.Exclusive {
				t.Fatal("destroy left waiting_connection create unfinished", creation, err)
			}
		})
	}
}

func TestManagedDestroyDoesNotBypassPendingRenewal(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, sessionHash := managedFixture(t, backend)
			selected, resource, accessRecord := destroyFixture(t, s, user, sessionHash)
			schedule, err := s.ManagedRenewalSchedule(ctx, selected.Runner.ID)
			if err != nil {
				t.Fatal(err)
			}
			renewal, consumed, err := s.ClaimScheduledManagedRenewal(ctx, schedule, "personal-v1", wire.ID(), time.Minute)
			if err != nil || !consumed || renewal.ID == "" {
				t.Fatal("renewal setup", renewal, consumed, err)
			}
			if destroyed, err := s.CreateManagedDestroy(ctx, user, sessionHash, wire.ID(), selected, resource, time.Minute); !errors.Is(err, lifecycle.ErrBusy) || destroyed.ID != "" {
				t.Fatal("destroy bypassed an unresolved renewal", destroyed, err)
			}
			current, err := s.ManagedResource(ctx, selected.Runner.ID)
			if err != nil || current.AccessClosed {
				t.Fatal("rejected destroy closed resource access", current, err)
			}
			if valid, err := s.CheckAccess(ctx, accessRecord, time.Now().Unix()); err != nil || !valid {
				t.Fatal("rejected destroy revoked access", valid, err)
			}
		})
	}
}

func TestManagedDestroyOfConfirmedGoneResourceIsAlreadyComplete(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, sessionHash := managedFixture(t, backend)
			created, _ := readyManagedResource(t, s, user, sessionHash)
			if _, err := s.db.ExecContext(ctx, `UPDATE dune_managed_resources SET gone=TRUE,access_closed=TRUE WHERE runner_id=$1`, created.Runner.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `DELETE FROM dune_machines WHERE runner_id=$1`, created.Runner.ID); err != nil {
				t.Fatal(err)
			}
			selected, err := s.RunnerResource(ctx, created.Runner.ID)
			if err != nil {
				t.Fatal(err)
			}
			resource, err := s.ManagedResource(ctx, created.Runner.ID)
			if err != nil {
				t.Fatal(err)
			}
			destroyed, err := s.CreateManagedDestroy(ctx, user, sessionHash, wire.ID(), selected, resource, time.Minute)
			if err != nil || !destroyed.Finished || destroyed.Outcome != "succeeded" || destroyed.Exclusive || destroyed.AccessCloseOutcome != lifecycle.AccessCloseConfirmed {
				t.Fatal("confirmed deletion did not converge cleanup", destroyed, err)
			}
		})
	}
}
