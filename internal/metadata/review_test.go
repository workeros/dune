package metadata

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/storage"
)

type reviewFixture struct {
	store     *Store
	config    storage.Config
	user      identity.User
	hash      string
	selected  authorization.Resource
	operation lifecycle.Operation
	action    lifecycle.ProviderAction
	review    lifecycle.Review
}

func unresolvedReviewFixture(t *testing.T, backend string) reviewFixture {
	t.Helper()
	ctx := context.Background()
	s, config, user, hash := managedFixture(t, backend)
	claimed := creationClaim(t, s, user, hash)
	action, dispatch, err := s.BeginProviderAction(ctx, claimed, lifecycle.ActionRequest{Kind: "create", Digest: claimed.Digest})
	if err != nil || !dispatch {
		t.Fatal("reserve create", dispatch, err)
	}
	if err := s.RecordProviderAction(ctx, claimed, action.ID, lifecycle.ActionObservation{Outcome: "unknown", ResourceRef: "possible-resource"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE dune_operations SET lease_until=0 WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	selected, err := s.RunnerResource(ctx, claimed.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	request := lifecycle.ReviewRequest{OperationID: claimed.ID, Mode: lifecycle.ReviewReconcile, Reason: "provider request history was checked"}
	review, err := s.CreateManagedReview(ctx, user, hash, "review-key", selected, request)
	if err != nil {
		t.Fatal(err)
	}
	if review.ActionID != action.ID || review.PrincipalID != user.ID || review.Reason != request.Reason || !review.CompletedAt.IsZero() || review.Outcome != "" {
		t.Fatal("review intent lost its audit facts", review)
	}
	return reviewFixture{store: s, config: config, user: user, hash: hash, selected: selected, operation: claimed, action: action, review: review}
}

func TestManagedReviewIntentClaimAndCompletion(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			fixture := unresolvedReviewFixture(t, backend)
			latest, err := fixture.store.ManagedOperationReview(ctx, fixture.user.ID, fixture.operation.ID)
			if err != nil || latest.ID != fixture.review.ID {
				t.Fatal("operation did not recover its latest review", latest, err)
			}
			other := fixture.store
			if backend == "postgres" {
				var err error
				other, err = Open(ctx, fixture.config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			var successes atomic.Int32
			var claimed lifecycle.Operation
			var review lifecycle.Review
			var mu sync.Mutex
			var wg sync.WaitGroup
			for range 6 {
				wg.Go(func() {
					r, op, err := other.ClaimManagedReview(ctx, fixture.review.ID, wire.ID(), time.Minute)
					if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrLeaseLost) {
						return
					}
					if err != nil {
						t.Error(err)
						return
					}
					successes.Add(1)
					mu.Lock()
					review, claimed = r, op
					mu.Unlock()
				})
			}
			wg.Wait()
			if successes.Load() != 1 || claimed.ID != fixture.operation.ID || claimed.Worker == "" || review.ID != fixture.review.ID || review.Worker != claimed.Worker {
				t.Fatal("review was not claimed exactly once", successes.Load(), review, claimed)
			}
			renewedReview, renewedOperation, err := other.RenewManagedReviewLease(ctx, review, claimed, time.Minute)
			if err != nil || renewedReview.Until.Before(review.Until) || renewedOperation.Until.Before(claimed.Until) {
				t.Fatal("review and operation leases did not renew together", err)
			}
			if err := other.RecordProviderAction(ctx, claimed, fixture.action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "possible-resource"}); err != nil {
				t.Fatal(err)
			}
			if err := other.CompleteManagedReview(ctx, review); err != nil {
				t.Fatal(err)
			}
			completed, err := fixture.store.ManagedReview(ctx, fixture.user.ID, fixture.review.ID)
			if err != nil || completed.Outcome != "succeeded" || completed.VerifiedResourceRef != "possible-resource" || completed.CompletedAt.IsZero() || completed.Worker != "" {
				t.Fatal("review completion did not preserve verified result", completed, err)
			}
			if candidates, err := fixture.store.RecoverableManagedReviewsFor(ctx, []string{"sandbox"}, 32); err != nil || len(candidates) != 0 {
				t.Fatal("completed review remained recoverable", candidates, err)
			}
		})
	}
}

func TestManagedReviewIdempotencyValidationAndResolvedRecovery(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			fixture := unresolvedReviewFixture(t, backend)
			request := lifecycle.ReviewRequest{OperationID: fixture.operation.ID, Mode: lifecycle.ReviewReconcile, Reason: fixture.review.Reason}
			repeated, err := fixture.store.CreateManagedReview(ctx, fixture.user, fixture.hash, fixture.review.RequestKey, fixture.selected, request)
			if err != nil || repeated != fixture.review {
				t.Fatal("review retry changed durable intent", repeated, err)
			}
			changed := request
			changed.Reason = "different evidence"
			if _, err := fixture.store.CreateManagedReview(ctx, fixture.user, fixture.hash, fixture.review.RequestKey, fixture.selected, changed); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("request key was rebound", err)
			}
			for _, invalid := range []lifecycle.ReviewRequest{
				{OperationID: fixture.operation.ID, Mode: lifecycle.ReviewCandidate, Reason: request.Reason},
				{OperationID: fixture.operation.ID, Mode: lifecycle.ReviewReconcile, Candidate: "untrusted", Reason: request.Reason},
				{OperationID: fixture.operation.ID, Mode: "force", Reason: request.Reason},
				{OperationID: fixture.operation.ID, Mode: lifecycle.ReviewReconcile, Reason: "\n"},
			} {
				if _, err := fixture.store.CreateManagedReview(ctx, fixture.user, fixture.hash, wire.ID(), fixture.selected, invalid); !errors.Is(err, ErrInvalidArgument) {
					t.Fatal("invalid review was accepted", invalid, err)
				}
			}
			if _, err := fixture.store.CreateManagedReview(ctx, fixture.user, fixture.hash, wire.ID(), fixture.selected, lifecycle.ReviewRequest{
				OperationID: fixture.operation.ID, Mode: lifecycle.ReviewCandidate, Candidate: "conflicting-resource", Reason: "candidate search",
			}); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("candidate overwrote a known partial association", err)
			}
			// If normal recovery resolves the action first, the review is closed from
			// durable action facts without another provider call.
			claim, err := fixture.store.ClaimOperation(ctx, fixture.operation.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.store.RecordProviderAction(ctx, claim, fixture.action.ID, lifecycle.ActionObservation{Outcome: "failed"}); err != nil {
				t.Fatal(err)
			}
			candidates, err := fixture.store.RecoverableManagedReviewsFor(ctx, []string{"sandbox"}, 32)
			if err != nil || len(candidates) != 1 {
				t.Fatal("resolved review was not available for audit closure", candidates, err)
			}
			closed, operation, err := fixture.store.ClaimManagedReview(ctx, fixture.review.ID, wire.ID(), time.Minute)
			if err != nil || !operation.CreatedAt.IsZero() || closed.Outcome != "failed" || closed.CompletedAt.IsZero() {
				t.Fatal("resolved action did not close review without a claim", closed, operation, err)
			}
			if _, err := fixture.store.db.Exec(`UPDATE dune_sessions SET expires_at=0 WHERE principal_id=$1`, fixture.user.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.store.CreateManagedReview(ctx, fixture.user, fixture.hash, fixture.review.RequestKey, fixture.selected, request); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("review retry bypassed current browser session check", err)
			}
		})
	}
}

func TestManagedReviewCommitLossRecoversOriginalIntent(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, hash := managedFixture(t, backend)
			claimed := creationClaim(t, s, user, hash)
			action, _, err := s.BeginProviderAction(ctx, claimed, lifecycle.ActionRequest{Kind: "create", Digest: claimed.Digest})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RecordProviderAction(ctx, claimed, action.ID, lifecycle.ActionObservation{Outcome: "unknown"}); err != nil {
				t.Fatal(err)
			}
			selected, err := s.RunnerResource(ctx, claimed.RunnerID)
			if err != nil {
				t.Fatal(err)
			}
			faulty, commits := lostCommitStore(t, config)
			request := lifecycle.ReviewRequest{OperationID: claimed.ID, Mode: lifecycle.ReviewCandidate, Candidate: "candidate-after-unknown", Reason: "provider metadata search"}
			key := wire.ID()
			if review, err := faulty.CreateManagedReview(ctx, user, hash, key, selected, request); !errors.Is(err, ErrCommitUnknown) || review.ID != "" || commits.Load() != 1 {
				t.Fatal("lost review acknowledgement returned false acceptance", review, commits.Load(), err)
			}
			recovered, err := s.ManagedOperationReview(ctx, user.ID, claimed.ID)
			if err != nil || recovered.ID == "" || recovered.RequestKey != key || recovered.ActionID != action.ID {
				t.Fatal("committed review intent was not recoverable", recovered, err)
			}
			replayed, err := s.CreateManagedReview(ctx, user, hash, key, selected, request)
			if err != nil || replayed != recovered {
				t.Fatal("review retry created another intent", replayed, recovered, err)
			}
		})
	}
}
