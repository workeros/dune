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

func historyCreation(t *testing.T, s *Store, user identity.User, sessionHash, name string) lifecycle.Creation {
	t.Helper()
	spec := managedSpec()
	spec.Name = name
	created, err := s.CreateManaged(context.Background(), user, sessionHash, wire.ID(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func finishHistoryCreate(t *testing.T, s *Store, created lifecycle.Creation, finishedAt int64, outcome string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE dune_operations SET finished=TRUE,finished_at=$2,outcome=$3,exclusive=FALSE,worker='',lease_until=0 WHERE id=$1`, created.Operation.ID, finishedAt, outcome); err != nil {
		t.Fatal(err)
	}
}

func addHistoryResource(t *testing.T, s *Store, created lifecycle.Creation, ref string, timestamp int64, gone, closed bool) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO dune_managed_resources(runner_id,fabric_id,resource_ref,confirmed_at,expires_at,gone,access_closed) VALUES($1,$2,$3,$4,$5,$6,$7)`, created.Runner.ID, created.Operation.FabricID, ref, timestamp, timestamp+time.Hour.Milliseconds(), gone, closed); err != nil {
		t.Fatal(err)
	}
}

func addHistoryOperation(t *testing.T, s *Store, created lifecycle.Creation, action, outcome string, finishedAt int64) string {
	t.Helper()
	id := wire.ID()
	finished := finishedAt > 0
	createdAt := finishedAt - time.Second.Milliseconds()
	if !finished {
		createdAt = time.Now().UnixMilli()
	}
	if _, err := s.db.Exec(`INSERT INTO dune_operations(id,request_key,request_digest,principal_id,identity_namespace,identity_subject,runner_id,fabric_id,binding_revision,action,created_at,finished_at,finished,outcome,exclusive) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,FALSE)`,
		id, wire.ID(), strings.Repeat("a", 64), created.Operation.PrincipalID, created.Operation.Namespace, created.Operation.Subject,
		created.Runner.ID, created.Operation.FabricID, created.Operation.BindingRevision, action, createdAt, finishedAt, finished, outcome); err != nil {
		t.Fatal(err)
	}
	return id
}

func addHistoryRenewal(t *testing.T, s *Store, created lifecycle.Creation, ref string, finishedAt int64, outcome string, withAudit bool) string {
	t.Helper()
	id := addHistoryOperation(t, s, created, "renew", outcome, finishedAt)
	renewUntil := finishedAt + time.Hour.Milliseconds()
	if finishedAt == 0 {
		renewUntil = time.Now().Add(time.Hour).UnixMilli()
	}
	if _, err := s.db.Exec(`INSERT INTO dune_managed_renewals(operation_id,runner_id,policy_version,renew_until) VALUES($1,$2,'history-v1',$3)`, id, created.Runner.ID, renewUntil); err != nil {
		t.Fatal(err)
	}
	if withAudit {
		actionID := wire.ID()
		if _, err := s.db.Exec(`INSERT INTO dune_provider_actions(id,operation_id,kind,request_digest,resource_ref,renew_until,worker,execution_revision,started_at,completed_at,outcome) VALUES($1,$2,'renew',$3,$4,$5,$6,1,$7,$8,'succeeded')`, actionID, id, strings.Repeat("b", 64), ref, renewUntil, wire.ID(), finishedAt-time.Second.Milliseconds(), finishedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO dune_managed_reviews(id,request_key,request_digest,principal_id,identity_namespace,identity_subject,operation_id,action_id,mode,candidate_resource_ref,reason,created_at,completed_at,outcome) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'reconcile','','history audit',$9,$10,'succeeded')`, wire.ID(), wire.ID(), strings.Repeat("c", 64), created.Operation.PrincipalID, created.Operation.Namespace, created.Operation.Subject, id, actionID, finishedAt-time.Second.Milliseconds(), finishedAt); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func mustClaimHistoryOperation(t *testing.T, s *Store, id string) lifecycle.Operation {
	t.Helper()
	claimed, err := s.ClaimOperation(context.Background(), id, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func TestManagedHistoryCleanupRetainsRecoveryFacts(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, sessionHash := managedFixture(t, backend)
			var now int64
			if err := s.db.QueryRowContext(ctx, `SELECT `+s.databaseClock()).Scan(&now); err != nil {
				t.Fatal(err)
			}
			old := now - (31 * 24 * time.Hour).Milliseconds()
			recent := now - (29 * 24 * time.Hour).Milliseconds()

			active := historyCreation(t, s, user, sessionHash, "active history")
			finishHistoryCreate(t, s, active, old, "succeeded")
			addHistoryResource(t, s, active, "active-history-resource", old, false, false)
			oldRenewalA := addHistoryRenewal(t, s, active, "active-history-resource", old-2000, "succeeded", true)
			oldRenewalB := addHistoryRenewal(t, s, active, "active-history-resource", old-1000, "failed", false)
			recentRenewal := addHistoryRenewal(t, s, active, "active-history-resource", recent, "succeeded", false)
			unknownRenewal := addHistoryRenewal(t, s, active, "active-history-resource", 0, "unknown", false)
			unknownReviewRenewal := addHistoryRenewal(t, s, active, "active-history-resource", old+1000, "succeeded", true)
			if _, err := s.db.Exec(`UPDATE dune_managed_reviews SET outcome='unknown' WHERE operation_id=$1`, unknownReviewRenewal); err != nil {
				t.Fatal(err)
			}

			residual := historyCreation(t, s, user, sessionHash, "residual history")
			finishHistoryCreate(t, s, residual, old, "succeeded")
			addHistoryResource(t, s, residual, "residual-history-resource", old, false, true)

			gone := historyCreation(t, s, user, sessionHash, "gone history")
			finishHistoryCreate(t, s, gone, old, "succeeded")
			addHistoryResource(t, s, gone, "gone-history-resource", old, true, true)
			destroyID := addHistoryOperation(t, s, gone, "destroy", "succeeded", old)
			if _, err := s.db.Exec(`INSERT INTO dune_managed_destroys(operation_id,runner_id,resource_ref,machine_id,access_closed_at,close_deadline,access_close_outcome) VALUES($1,$2,'gone-history-resource','',$3,$3,'confirmed')`, destroyID, gone.Runner.ID, old); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT INTO dune_managed_destroy_closures(operation_id,instance_id,machine_id,acknowledged_at) VALUES($1,$2,'',$3)`, destroyID, wire.ID(), old); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE dune_runners SET created_at=1 WHERE id=$1`, gone.Runner.ID); err != nil {
				t.Fatal(err)
			}

			timedOut := historyCreation(t, s, user, sessionHash, "timed out closure history")
			finishHistoryCreate(t, s, timedOut, old, "succeeded")
			addHistoryResource(t, s, timedOut, "timed-out-history-resource", old, true, true)
			timedOutDestroyID := addHistoryOperation(t, s, timedOut, "destroy", "succeeded", old)
			if _, err := s.db.Exec(`INSERT INTO dune_managed_destroys(operation_id,runner_id,resource_ref,machine_id,access_closed_at,close_deadline,access_close_outcome) VALUES($1,$2,'timed-out-history-resource','',$3,$3,'timed_out')`, timedOutDestroyID, timedOut.Runner.ID, old); err != nil {
				t.Fatal(err)
			}

			failed := historyCreation(t, s, user, sessionHash, "failed history")
			finishHistoryCreate(t, s, failed, old, "failed")
			if _, err := s.db.Exec(`UPDATE dune_runners SET created_at=2 WHERE id=$1`, failed.Runner.ID); err != nil {
				t.Fatal(err)
			}

			pending := historyCreation(t, s, user, sessionHash, "pending history")
			if err := s.RecordOperationUncertainty(ctx, mustClaimHistoryOperation(t, s, pending.Operation.ID), "unknown"); err != nil {
				t.Fatal(err)
			}

			for _, invalid := range []struct {
				retention time.Duration
				limit     int
			}{{23 * time.Hour, 1}, {367 * 24 * time.Hour, 1}, {30 * 24 * time.Hour, 0}, {30 * 24 * time.Hour, 33}} {
				if _, err := s.PruneManagedHistory(ctx, invalid.retention, invalid.limit); !errors.Is(err, ErrInvalidArgument) {
					t.Fatal("invalid cleanup bounds accepted", invalid, err)
				}
			}

			first, err := s.PruneManagedHistory(ctx, 30*24*time.Hour, 1)
			if err != nil || first.Operations != 1 || first.Runners != 0 {
				t.Fatal("bounded cleanup removed the wrong first batch", first, err)
			}
			if _, err := s.Operation(ctx, oldRenewalA); !errors.Is(err, ErrNotFound) {
				t.Fatal("oldest renewal history was retained", err)
			}
			second, err := s.PruneManagedHistory(ctx, 30*24*time.Hour, 3)
			if err != nil || second.Operations != 3 || second.Runners != 1 {
				t.Fatal("terminal history did not converge", second, err)
			}
			for _, id := range []string{oldRenewalB, gone.Operation.ID, destroyID} {
				if _, err := s.Operation(ctx, id); !errors.Is(err, ErrNotFound) {
					t.Fatal("old terminal operation survived", id, err)
				}
			}
			if _, err := s.Operation(ctx, failed.Operation.ID); err != nil {
				t.Fatal("batch operation budget was exceeded", err)
			}
			third, err := s.PruneManagedHistory(ctx, 30*24*time.Hour, 32)
			if err != nil || third.Operations != 1 || third.Runners != 1 {
				t.Fatal("bounded cleanup did not resume", third, err)
			}
			for _, id := range []string{recentRenewal, unknownRenewal, unknownReviewRenewal, residual.Operation.ID, timedOut.Operation.ID, timedOutDestroyID, pending.Operation.ID, active.Operation.ID} {
				if _, err := s.Operation(ctx, id); err != nil {
					t.Fatal("required recovery history was removed", id, err)
				}
			}
			for _, runnerID := range []string{gone.Runner.ID, failed.Runner.ID} {
				if _, err := s.Runner(ctx, user.ID, runnerID); !errors.Is(err, ErrNotFound) {
					t.Fatal("fully terminal Runner was retained", runnerID, err)
				}
			}
			for _, runnerID := range []string{active.Runner.ID, residual.Runner.ID, timedOut.Runner.ID, pending.Runner.ID} {
				if _, err := s.Runner(ctx, user.ID, runnerID); err != nil {
					t.Fatal("live or unresolved Runner was removed", runnerID, err)
				}
			}
		})
	}
}

func TestPostgresManagedHistoryCleanupClaimsRowsOnce(t *testing.T) {
	ctx := context.Background()
	s, config, user, sessionHash := managedFixture(t, "postgres")
	other, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var now int64
	if err := s.db.QueryRowContext(ctx, `SELECT `+s.databaseClock()).Scan(&now); err != nil {
		t.Fatal(err)
	}
	old := now - (31 * 24 * time.Hour).Milliseconds()
	active := historyCreation(t, s, user, sessionHash, "concurrent history")
	finishHistoryCreate(t, s, active, old, "succeeded")
	addHistoryResource(t, s, active, "concurrent-history-resource", old, false, false)
	for range 8 {
		addHistoryRenewal(t, s, active, "concurrent-history-resource", old, "succeeded", false)
		old--
	}
	var wg sync.WaitGroup
	results := make(chan ManagedHistoryCleanup, 2)
	errs := make(chan error, 2)
	for _, store := range []*Store{s, other} {
		wg.Go(func() {
			cleanup, err := store.PruneManagedHistory(ctx, 30*24*time.Hour, 32)
			results <- cleanup
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	var removed int64
	for result := range results {
		removed += result.Operations
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if removed != 8 {
		t.Fatal("concurrent cleanup double-counted or missed rows", removed)
	}
}
