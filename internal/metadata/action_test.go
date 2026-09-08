package metadata

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
)

func creationClaim(t *testing.T, s *Store, user identity.User, hash string) lifecycle.Operation {
	t.Helper()
	ctx := context.Background()
	created, err := s.CreateManaged(ctx, user, hash, wire.ID(), managedSpec())
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.ClaimOperation(ctx, created.Operation.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestProviderActionReservationAndTakeover(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, hash := managedFixture(t, backend)
			original := creationClaim(t, s, user, hash)
			other := s
			if backend == "postgres" {
				var err error
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			request := lifecycle.ActionRequest{Kind: "create", Digest: original.Digest}
			var dispatches atomic.Int32
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					a, dispatch, err := other.BeginProviderAction(ctx, original, request)
					if err != nil {
						t.Error(err)
						return
					}
					if a.ID == "" {
						t.Error("reservation has no key")
					}
					if dispatch {
						dispatches.Add(1)
					}
				})
			}
			wg.Wait()
			if dispatches.Load() != 1 {
				t.Fatal("concurrent callers received duplicate dispatch rights", dispatches.Load())
			}
			action, err := s.ProviderAction(ctx, original.ID, "create")
			if err != nil {
				t.Fatal(err)
			}
			if action.Worker != original.Worker || action.ExecutionRevision != original.Revision || action.ResourceRef != "" || action.Outcome != "" || !action.CompletedAt.IsZero() {
				t.Fatal("invalid initial reservation", action)
			}
			if err := s.CheckProviderAction(ctx, original, action.ID); err != nil {
				t.Fatal(err)
			}
			changed := request
			changed.Digest = strings.Repeat("b", 64)
			if _, _, err := s.BeginProviderAction(ctx, original, changed); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("action key rebound to other arguments", err)
			}
			if _, _, err := s.BeginProviderAction(ctx, original, lifecycle.ActionRequest{Kind: "bootstrap", Digest: strings.Repeat("c", 64)}); !errors.Is(err, lifecycle.ErrBusy) {
				t.Fatal("bootstrap bypassed unresolved create", err)
			}
			if err := s.ReleaseOperationMutex(ctx, original); !errors.Is(err, lifecycle.ErrBusy) {
				t.Fatal("blank operation outcome released pending provider call", err)
			}
			if err := s.FinishOperation(ctx, original, "succeeded"); !errors.Is(err, lifecycle.ErrBusy) {
				t.Fatal("pending call was marked finished", err)
			}
			if _, err := s.db.Exec(`UPDATE dune_operations SET lease_until=0 WHERE id=$1`, original.ID); err != nil {
				t.Fatal(err)
			}
			takeover, err := other.ClaimOperation(ctx, original.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			found, dispatch, err := other.BeginProviderAction(ctx, takeover, request)
			if err != nil || dispatch || found != action {
				t.Fatal("takeover authorized a repeated call", err)
			}
			if err := other.CheckProviderAction(ctx, takeover, action.ID); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("new claimant reused original dispatch reservation", err)
			}
			if err := s.CheckProviderAction(ctx, original, action.ID); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("expired issuer passed pre-call check", err)
			}
			if err := s.RecordProviderAction(ctx, original, action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "stale"}); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("late worker wrote resource facts", err)
			}
			if err := other.RecordProviderAction(ctx, takeover, action.ID, lifecycle.ActionObservation{Outcome: "timed_out"}); err != nil {
				t.Fatal(err)
			}
			op, err := s.Operation(ctx, original.ID)
			if err != nil || op.Outcome != "timed_out" || !op.Exclusive {
				t.Fatal("timeout released business mutex", err)
			}
			if _, err := s.ManagedResource(ctx, original.RunnerID); !errors.Is(err, ErrNotFound) {
				t.Fatal("timeout invented a resource", err)
			}
			observation := lifecycle.ActionObservation{Outcome: "unknown", ResourceRef: "verified-resource", ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Millisecond)}
			if err := other.RecordProviderAction(ctx, takeover, action.ID, observation); err != nil {
				t.Fatal(err)
			}
			resource, err := s.ManagedResource(ctx, original.RunnerID)
			if err != nil {
				t.Fatal(err)
			}
			if resource.Ref != observation.ResourceRef || resource.ConfirmedAt.IsZero() || resource.Gone {
				t.Fatal("known partial result lost", resource)
			}
			changedRef := observation
			changedRef.ResourceRef = "another-resource"
			if err := other.RecordProviderAction(ctx, takeover, action.ID, changedRef); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("pending action changed its known resource", err)
			}
			observation.Outcome = "succeeded"
			if err := other.RecordProviderAction(ctx, takeover, action.ID, observation); err != nil {
				t.Fatal(err)
			}
			confirmed, err := s.ManagedResource(ctx, original.RunnerID)
			if err != nil || !confirmed.ConfirmedAt.Equal(resource.ConfirmedAt) {
				t.Fatal("query reset first-connection grace", err)
			}
			completed, err := s.ProviderAction(ctx, original.ID, "create")
			if err != nil || completed.Outcome != "succeeded" || completed.CompletedAt.IsZero() || completed.ID != action.ID {
				t.Fatal("confirmed stage not persisted", err)
			}
			if backend == "sqlite" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			if got, err := other.ManagedResource(ctx, original.RunnerID); err != nil || got != confirmed {
				t.Fatal("resource association lost across restart/pool", err)
			}
			if got, dispatch, err := other.BeginProviderAction(ctx, takeover, request); err != nil || dispatch || got != completed {
				t.Fatal("completed action was dispatched again", err)
			}
			bootstrap, dispatch, err := other.BeginProviderAction(ctx, takeover, lifecycle.ActionRequest{Kind: "bootstrap", Digest: strings.Repeat("c", 64)})
			if err != nil || !dispatch || bootstrap.ID == action.ID || bootstrap.ResourceRef != resource.Ref {
				t.Fatal("bootstrap lost independent key or verified target", err)
			}
			// Binding changes remain stronger than execution leases and action keys.
			if _, err := other.db.Exec(`UPDATE dune_runners SET binding_revision=binding_revision+1 WHERE id=$1`, original.RunnerID); err != nil {
				t.Fatal(err)
			}
			if err := other.CheckProviderAction(ctx, takeover, bootstrap.ID); !errors.Is(err, runner.ErrBindingChanged) {
				t.Fatal("pre-call check followed changed binding", err)
			}
			if err := other.RecordProviderAction(ctx, takeover, bootstrap.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: resource.Ref}); !errors.Is(err, runner.ErrBindingChanged) {
				t.Fatal("old binding wrote stage result", err)
			}
		})
	}
}

func TestProviderActionAtomicResourceAssociation(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, hash := managedFixture(t, backend)
			first := creationClaim(t, s, user, hash)
			second := creationClaim(t, s, user, hash)
			a, _, err := s.BeginProviderAction(ctx, first, lifecycle.ActionRequest{Kind: "create", Digest: first.Digest})
			if err != nil {
				t.Fatal(err)
			}
			b, _, err := s.BeginProviderAction(ctx, second, lifecycle.ActionRequest{Kind: "create", Digest: second.Digest})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RecordProviderAction(ctx, first, a.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "exclusive-resource"}); err != nil {
				t.Fatal(err)
			}
			before := snapshotRecords(t, s)
			if err := s.RecordProviderAction(ctx, second, b.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "exclusive-resource"}); !errors.Is(err, ErrConflict) {
				t.Fatal("two runners claimed the same fabric resource", err)
			}
			if !reflect.DeepEqual(before, snapshotRecords(t, s)) {
				t.Fatal("failed association changed action, resource or operation")
			}
			// A late action-table failure must roll back the already inserted resource.
			fail, undo := `CREATE TRIGGER action_failure BEFORE UPDATE ON dune_provider_actions BEGIN SELECT RAISE(ABORT,'action failure'); END`, `DROP TRIGGER action_failure`
			if backend == "postgres" {
				fail = `ALTER TABLE dune_provider_actions ADD CONSTRAINT action_failure CHECK(outcome<>'failed')`
				undo = `ALTER TABLE dune_provider_actions DROP CONSTRAINT action_failure`
			}
			if _, err := s.db.Exec(fail); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordProviderAction(ctx, second, b.ID, lifecycle.ActionObservation{Outcome: "failed", ResourceRef: "retained-partial-resource"}); !errors.Is(err, ErrConflict) {
				t.Fatal("late stage failure not injected", err)
			}
			if !reflect.DeepEqual(before, snapshotRecords(t, s)) {
				t.Fatal("late failure left partial resource association")
			}
			if _, err := s.db.Exec(undo); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordProviderAction(ctx, second, b.ID, lifecycle.ActionObservation{Outcome: "failed", ResourceRef: "retained-partial-resource"}); err != nil {
				t.Fatal(err)
			}
			op, err := s.Operation(ctx, second.ID)
			if err != nil || !op.Finished || op.Exclusive || op.Outcome != "failed" {
				t.Fatal("definite failure did not finish operation", err)
			}
			resource, err := s.ManagedResource(ctx, second.RunnerID)
			if err != nil || resource.Ref != "retained-partial-resource" || resource.Gone {
				t.Fatal("failure implicitly removed known resource", err)
			}
			if err := s.RecordProviderAction(ctx, first, a.ID, lifecycle.ActionObservation{Outcome: "unknown", ResourceRef: "replacement"}); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("completed action was overwritten", err)
			}
		})
	}
}

func TestProviderActionBootstrapRenewAndDestroy(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, hash := managedFixture(t, backend)
			creation := creationClaim(t, s, user, hash)
			action, _, err := s.BeginProviderAction(ctx, creation, lifecycle.ActionRequest{Kind: "create", Digest: creation.Digest})
			if err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(time.Hour).Truncate(time.Millisecond)
			if err := s.RecordProviderAction(ctx, creation, action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "resource", ExpiresAt: expires}); err != nil {
				t.Fatal(err)
			}
			bootstrap, dispatch, err := s.BeginProviderAction(ctx, creation, lifecycle.ActionRequest{Kind: "bootstrap", Digest: strings.Repeat("d", 64)})
			if err != nil || !dispatch {
				t.Fatal(err)
			}
			if err := s.RecordProviderAction(ctx, creation, bootstrap.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "resource"}); err != nil {
				t.Fatal(err)
			}
			waiting, err := s.Operation(ctx, creation.ID)
			if err != nil || waiting.Finished || waiting.Exclusive || waiting.Outcome != "" {
				t.Fatal("bootstrap confused readiness with success or blocked maintenance", err)
			}
			begin := func(kind string) lifecycle.Operation {
				intent := creation.Intent
				intent.ID = wire.ID()
				intent.RequestKey = wire.ID()
				intent.Digest = tokenHash(kind)
				intent.Action = kind
				op, err := s.BeginOperation(ctx, intent)
				if err != nil {
					t.Fatal(err)
				}
				op, err = s.ClaimOperation(ctx, op.ID, wire.ID(), time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				return op
			}
			renewal := begin("renew")
			until := expires.Add(time.Hour)
			renew, dispatch, err := s.BeginProviderAction(ctx, renewal, lifecycle.ActionRequest{Kind: "renew", Digest: tokenHash("fixed renewal arguments"), RenewUntil: until})
			if err != nil || !dispatch {
				t.Fatal(err)
			}
			if _, _, err := s.BeginProviderAction(ctx, renewal, lifecycle.ActionRequest{Kind: "renew", Digest: renew.Digest, RenewUntil: until.Add(time.Minute)}); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("renewal retry moved target expiry", err)
			}
			for _, o := range []lifecycle.ActionObservation{{Outcome: "succeeded", ResourceRef: "other", ExpiresAt: until}, {Outcome: "succeeded", ResourceRef: "resource", ExpiresAt: expires}, {Outcome: "succeeded", ResourceRef: "resource", ExpiresAt: until, Gone: true}} {
				if err := s.RecordProviderAction(ctx, renewal, renew.ID, o); err == nil {
					t.Fatal("unconfirmed renewal reported success")
				}
			}
			if err := s.RecordProviderAction(ctx, renewal, renew.ID, lifecycle.ActionObservation{Outcome: "timed_out"}); err != nil {
				t.Fatal(err)
			}
			if resource, err := s.ManagedResource(ctx, creation.RunnerID); err != nil || !resource.ExpiresAt.Equal(expires) {
				t.Fatal("timeout extended expiry", err)
			}
			if err := s.RecordProviderAction(ctx, renewal, renew.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "resource", ExpiresAt: until}); err != nil {
				t.Fatal(err)
			}
			if resource, err := s.ManagedResource(ctx, creation.RunnerID); err != nil || !resource.ExpiresAt.Equal(until) {
				t.Fatal("confirmed renewal missing", err)
			}
			destruction := begin("destroy")
			request := lifecycle.ActionRequest{Kind: "destroy", Digest: tokenHash("fixed destroy arguments")}
			if _, _, err := s.BeginProviderAction(ctx, destruction, request); !errors.Is(err, lifecycle.ErrBusy) {
				t.Fatal("destroy began before durable access closure", err)
			}
			// The lifecycle-specific destroy admission/stream closure is a separate
			// feature. This fixture supplies its prerequisite, never a user-facing bypass.
			if _, err := s.db.Exec(`UPDATE dune_managed_resources SET access_closed=TRUE WHERE runner_id=$1`, creation.RunnerID); err != nil {
				t.Fatal(err)
			}
			destroy, dispatch, err := s.BeginProviderAction(ctx, destruction, request)
			if err != nil || !dispatch {
				t.Fatal(err)
			}
			if err := s.RecordProviderAction(ctx, destruction, destroy.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "resource"}); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal("delete acceptance became confirmed deletion", err)
			}
			if err := s.RecordProviderAction(ctx, destruction, destroy.ID, lifecycle.ActionObservation{Outcome: "unknown"}); err != nil {
				t.Fatal(err)
			}
			if _, dispatch, err := s.BeginProviderAction(ctx, destruction, request); err != nil || dispatch {
				t.Fatal("unknown destroy was redispatched", err)
			}
			if err := s.RecordProviderAction(ctx, destruction, destroy.ID, lifecycle.ActionObservation{Outcome: "failed", ResourceRef: "resource"}); err != nil {
				t.Fatal(err)
			}
			if resource, err := s.ManagedResource(ctx, creation.RunnerID); err != nil || !resource.AccessClosed || resource.Gone {
				t.Fatal("failed destroy reopened access or removed reference", err)
			}
			cleanup := begin("destroy")
			last, _, err := s.BeginProviderAction(ctx, cleanup, request)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RecordProviderAction(ctx, cleanup, last.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "resource", Gone: true}); err != nil {
				t.Fatal(err)
			}
			if resource, err := s.ManagedResource(ctx, creation.RunnerID); err != nil || !resource.Gone || !resource.AccessClosed || resource.Ref != "resource" {
				t.Fatal("confirmed deletion lost history or access restriction", err)
			}
		})
	}
}

func TestProviderActionCommitLossDoesNotDispatch(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, hash := managedFixture(t, backend)
			op := creationClaim(t, s, user, hash)
			faulty, commits := lostCommitStore(t, config)
			request := lifecycle.ActionRequest{Kind: "create", Digest: op.Digest}
			if a, dispatch, err := faulty.BeginProviderAction(ctx, op, request); !errors.Is(err, ErrCommitUnknown) || dispatch || a.ID != "" || commits.Load() != 1 {
				t.Fatal("lost reservation acknowledgement granted dispatch or replayed", err)
			}
			action, err := s.ProviderAction(ctx, op.ID, "create")
			if err != nil {
				t.Fatal("cannot find uncertain reservation", err)
			}
			if got, dispatch, err := s.BeginProviderAction(ctx, op, request); err != nil || dispatch || got != action {
				t.Fatal("uncertain reservation was submitted again", err)
			}
			if err := faulty.RecordProviderAction(ctx, op, action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "durable-resource"}); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 2 {
				t.Fatal("result commit loss hidden or replayed", err)
			}
			resource, err := s.ManagedResource(ctx, op.RunnerID)
			if err != nil || resource.Ref != "durable-resource" {
				t.Fatal("cannot reconcile committed resource", err)
			}
			got, err := s.ProviderAction(ctx, op.ID, "create")
			if err != nil || got.ID != action.ID || got.Outcome != "succeeded" || got.CompletedAt.IsZero() {
				t.Fatal("resource and stage did not commit together", err)
			}
		})
	}
}
