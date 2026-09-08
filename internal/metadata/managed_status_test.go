package metadata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
)

func statusResource(t *testing.T, store *Store, user identity.User, sessionHash, ref, outcome string, expiresAt time.Time) lifecycle.Creation {
	t.Helper()
	created, err := store.CreateManaged(context.Background(), user, sessionHash, wire.ID(), managedSpec())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimOperation(context.Background(), created.Operation.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	action, dispatch, err := store.BeginProviderAction(context.Background(), claimed, lifecycle.ActionRequest{Kind: "create", Digest: claimed.Digest})
	if err != nil || !dispatch {
		t.Fatal("create action was not reserved", dispatch, err)
	}
	if err := store.RecordProviderAction(context.Background(), claimed, action.ID, lifecycle.ActionObservation{Outcome: outcome, ResourceRef: ref, ExpiresAt: expiresAt}); err != nil {
		t.Fatal(err)
	}
	return created
}

func TestManagedStatusSnapshotReportsOperatorPressure(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			store, _, user, sessionHash := managedFixture(t, backend)
			var now int64
			if err := store.db.QueryRowContext(ctx, `SELECT `+store.databaseClock()).Scan(&now); err != nil {
				t.Fatal(err)
			}

			ready, _ := readyManagedResource(t, store, user, sessionHash)
			if _, err := store.db.ExecContext(ctx, `UPDATE dune_managed_resources SET expires_at=$2 WHERE runner_id=$1`, ready.Runner.ID, now+5*time.Minute.Milliseconds()); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `INSERT INTO dune_managed_maintenance(runner_id,fabric_id,resource_ref,binding_revision,policy_version,reason,facts,observed_at,next_check_at)
				SELECT runner_id,fabric_id,resource_ref,1,'personal-v1','NOT_DUE','confirmed',$2,$3 FROM dune_managed_resources WHERE runner_id=$1`, ready.Runner.ID, now, now+time.Hour.Milliseconds()); err != nil {
				t.Fatal(err)
			}

			statusResource(t, store, user, sessionHash, "unknown-status-resource", "unknown", time.UnixMilli(now).Add(time.Hour))
			timedOut, err := store.CreateManaged(ctx, user, sessionHash, wire.ID(), managedSpec())
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimOperation(ctx, timedOut.Operation.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordOperationUncertainty(ctx, claimed, "timed_out"); err != nil {
				t.Fatal(err)
			}
			closed := statusResource(t, store, user, sessionHash, "closed-status-resource", "succeeded", time.UnixMilli(now).Add(time.Hour))
			if _, err := store.db.ExecContext(ctx, `UPDATE dune_managed_resources SET access_closed=TRUE WHERE runner_id=$1`, closed.Runner.ID); err != nil {
				t.Fatal(err)
			}

			status, err := store.ManagedStatusSnapshot(ctx, "personal-v1", 10*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if status.CheckedAt.IsZero() || status.Runners != 4 || status.KnownResources != 3 || status.AccessibleResources != 2 ||
				status.UnknownOperations != 1 || status.TimedOutOperations != 1 || status.RenewalBacklog != 1 ||
				status.ExpiryRiskResources != 1 || status.ResidualResources != 2 {
				t.Fatal("Managed pressure snapshot lost durable state", status)
			}
			if drifted, err := store.ManagedStatusSnapshot(ctx, "personal-v2", 10*time.Minute); err != nil || drifted.RenewalBacklog != 2 {
				t.Fatal("policy drift was absent from renewal backlog", drifted, err)
			}
			if _, err := store.ManagedStatusSnapshot(ctx, "", time.Minute); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal("empty policy version was accepted", err)
			}
			if _, err := store.ManagedStatusSnapshot(ctx, "personal-v1", 0); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal("zero expiry risk window was accepted", err)
			}
		})
	}
}
