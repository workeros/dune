package metadata

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
)

func scheduledManagedRenewal(t *testing.T, s *Store, user identity.User, sessionHash string) (lifecycle.Creation, lifecycle.RenewalSchedule) {
	t.Helper()
	created, _ := readyManagedResource(t, s, user, sessionHash)
	claim, err := s.ClaimManagedInspection(context.Background(), created.Runner.ID, "personal-v1", wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := s.RecordManagedInspection(context.Background(), claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{
		Status: lifecycle.InspectionConfirmed, ResourceRef: claim.ResourceRef, ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	if err != nil || schedule.RenewUntil.IsZero() {
		t.Fatal("renewal was not scheduled", schedule, err)
	}
	return created, schedule
}

func TestScheduledManagedRenewalCreatesOneClaimedOperation(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, sessionHash := managedFixture(t, backend)
			created, schedule := scheduledManagedRenewal(t, s, user, sessionHash)
			other := s
			if backend == "postgres" {
				var err error
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			candidates, err := s.RecoverableManagedRenewalSchedulesFor(ctx, []string{"sandbox"}, "personal-v1", 32)
			if err != nil || len(candidates) != 1 || candidates[0] != schedule {
				t.Fatal("frozen target was not recoverable", candidates, err)
			}
			if candidates, err := s.RecoverableManagedRenewalSchedulesFor(ctx, []string{"other"}, "personal-v1", 32); err != nil || len(candidates) != 0 {
				t.Fatal("unsupported Fabric leaked into renewal work", candidates, err)
			}
			if candidates, err := s.RecoverableManagedRenewalSchedulesFor(ctx, []string{"sandbox"}, "personal-v2", 32); err != nil || len(candidates) != 0 {
				t.Fatal("another policy version consumed the target", candidates, err)
			}

			// Accepted background maintenance does not become a browser request and
			// remains recoverable if the original account is later disabled.
			if _, err := s.db.ExecContext(ctx, `UPDATE dune_principals SET enabled=FALSE WHERE id=$1`, user.ID); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var claims []lifecycle.Operation
			var wg sync.WaitGroup
			for i := range 6 {
				wg.Go(func() {
					store := s
					if i%2 == 1 {
						store = other
					}
					claim, consumed, claimErr := store.ClaimScheduledManagedRenewal(ctx, schedule, "personal-v1", wire.ID(), time.Minute)
					if claimErr == nil && consumed && claim.ID != "" {
						mu.Lock()
						claims = append(claims, claim)
						mu.Unlock()
					} else if claimErr == nil || (!errors.Is(claimErr, lifecycle.ErrBusy) && !errors.Is(claimErr, lifecycle.ErrLeaseLost) && !errors.Is(claimErr, ErrConflict)) {
						t.Error("unexpected renewal claim result", consumed, claimErr)
					}
				})
			}
			wg.Wait()
			if len(claims) != 1 {
				t.Fatal("schedule created more than one operation", len(claims))
			}
			claim := claims[0]
			if claim.Action != "renew" || claim.RunnerID != created.Runner.ID || claim.PrincipalID != user.ID || !claim.Exclusive || claim.Revision != 1 || claim.Worker == "" || claim.Until.IsZero() || !strings.HasPrefix(claim.RequestKey, "maintenance-renew-") {
				t.Fatal("invalid automatic renewal operation", claim)
			}
			request, err := s.ManagedRenewal(ctx, claim.ID)
			if err != nil || request.OperationID != claim.ID || request.RunnerID != claim.RunnerID || request.PolicyVersion != "personal-v1" || !request.Until.Equal(schedule.RenewUntil) {
				t.Fatal("operation lost its frozen policy input", request, err)
			}
			persisted, err := s.ManagedRenewalSchedule(ctx, claim.RunnerID)
			if err != nil || persisted.Reason != "MUTATION_PENDING" || !persisted.RenewUntil.IsZero() || persisted.NextCheckAt.IsZero() {
				t.Fatal("consumed target remained dispatchable", persisted, err)
			}
			if candidates, err := s.RecoverableManagedRenewalSchedulesFor(ctx, []string{"sandbox"}, "personal-v1", 32); err != nil || len(candidates) != 0 {
				t.Fatal("consumed target was scheduled twice", candidates, err)
			}
			if err := s.YieldOperationLease(ctx, claim); err != nil {
				t.Fatal(err)
			}
			operations, err := other.RecoverableManagedRenewalsFor(ctx, []string{"sandbox"}, 32)
			if err != nil || len(operations) != 1 || operations[0].ID != claim.ID {
				t.Fatal("automatic operation was not recoverable", operations, err)
			}
			takeover, err := other.ClaimRecoverableManagedRenewal(ctx, claim.ID, wire.ID(), time.Minute)
			if err != nil || takeover.Revision != claim.Revision+1 {
				t.Fatal("automatic operation could not be taken over", takeover, err)
			}
			if _, _, _, err := s.BeginManagedRenewal(ctx, claim); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("old renewal claimant retained execution", err)
			}
		})
	}
}

func TestScheduledManagedRenewalCommitLossDoesNotDuplicate(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, sessionHash := managedFixture(t, backend)
			created, schedule := scheduledManagedRenewal(t, s, user, sessionHash)
			interrupted, commits := lostCommitStore(t, config)
			op, consumed, err := interrupted.ClaimScheduledManagedRenewal(ctx, schedule, "personal-v1", wire.ID(), time.Minute)
			if !errors.Is(err, ErrCommitUnknown) || consumed || op.ID != "" || commits.Load() != 1 {
				t.Fatal("uncertain schedule consumption returned execution", op, consumed, commits.Load(), err)
			}
			var operations, renewals int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_operations WHERE runner_id=$1 AND action='renew'`, created.Runner.ID).Scan(&operations); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_managed_renewals WHERE runner_id=$1`, created.Runner.ID).Scan(&renewals); err != nil {
				t.Fatal(err)
			}
			persisted, err := s.ManagedRenewalSchedule(ctx, created.Runner.ID)
			if err != nil || operations != 1 || renewals != 1 || !persisted.RenewUntil.IsZero() {
				t.Fatal("uncertain commit did not persist one consumed operation", operations, renewals, persisted, err)
			}
			if _, consumed, err := s.ClaimScheduledManagedRenewal(ctx, schedule, "personal-v1", wire.ID(), time.Minute); !errors.Is(err, lifecycle.ErrBusy) || consumed {
				t.Fatal("uncertain commit was replayed", consumed, err)
			}
		})
	}
}

func TestScheduledManagedRenewalExpiresWithoutProviderAction(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, sessionHash := managedFixture(t, backend)
			created, schedule := scheduledManagedRenewal(t, s, user, sessionHash)
			if _, err := s.db.ExecContext(ctx, `UPDATE dune_managed_maintenance SET renew_until=`+s.databaseClock()+`-1 WHERE runner_id=$1`, created.Runner.ID); err != nil {
				t.Fatal(err)
			}
			schedule, err := s.ManagedRenewalSchedule(ctx, created.Runner.ID)
			if err != nil {
				t.Fatal(err)
			}
			op, consumed, err := s.ClaimScheduledManagedRenewal(ctx, schedule, "personal-v1", wire.ID(), time.Minute)
			if err != nil || !consumed || op.ID != "" {
				t.Fatal("expired target became executable", op, consumed, err)
			}
			if _, err := s.ManagedRenewal(ctx, op.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("expired target left mutation intent", err)
			}
			persisted, err := s.ManagedRenewalSchedule(ctx, created.Runner.ID)
			if err != nil || !persisted.RenewUntil.IsZero() || persisted.Reason != "" || persisted.NextCheckAt.IsZero() {
				t.Fatal("expired target was not returned to inspection", persisted, err)
			}
		})
	}
}
