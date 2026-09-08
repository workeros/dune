package managed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/fabric"
)

type candidateProvider struct {
	mu     sync.Mutex
	calls  []fabric.CandidateCall
	result fabric.Observation
	err    error
}

func (p *candidateProvider) VerifyCandidate(_ context.Context, call fabric.CandidateCall) (fabric.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
	return p.result, p.err
}

func completeReviewProviders(create fabric.CreateProvider, candidate fabric.CandidateProvider) ProviderSet {
	if create == nil {
		create = &createProvider{}
	}
	if candidate == nil {
		candidate = &candidateProvider{}
	}
	return ProviderSet{
		Create: map[string]fabric.CreateProvider{"sandbox": create}, Bootstrap: map[string]fabric.BootstrapProvider{"sandbox": &bootstrapProvider{}},
		Renew: map[string]fabric.RenewProvider{"sandbox": &renewProvider{}}, Destroy: map[string]fabric.DestroyProvider{"sandbox": &destroyProvider{}},
		Candidate: map[string]fabric.CandidateProvider{"sandbox": candidate},
	}
}

func reviewWorkerConfig() WorkerConfig {
	config := testWorkerConfig()
	config.Bootstrap = BootstrapConfig{PublicURL: "https://dune.example.test/tools/", Version: "review-test", EnrollmentLifetime: time.Minute}
	config.RenewalPolicyVersion = "personal-v1"
	return config
}

func unresolvedCreateAction(t *testing.T, fixture serviceFixture) (lifecycle.Operation, lifecycle.ProviderAction) {
	t.Helper()
	claimed := claimedCreationWithTTL(t, fixture, time.Second)
	action, dispatch, err := fixture.store.BeginProviderAction(fixture.ctx, claimed, lifecycle.ActionRequest{Kind: "create", Digest: claimed.Digest})
	if err != nil || !dispatch {
		t.Fatal("reserve create action", dispatch, err)
	}
	if err := fixture.store.RecordProviderAction(fixture.ctx, claimed, action.ID, lifecycle.ActionObservation{Outcome: "unknown"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	return claimed, action
}

func TestManualCandidateReviewVerifiesBeforeAssociatingResource(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed, action := unresolvedCreateAction(t, fixture)
	var accessRequest access.Request
	service := newService(t, fixture, checkFunc(func(_ context.Context, request access.Request) (access.Decision, error) {
		accessRequest = request
		return allowDecision(), nil
	}), nil)
	review, err := service.Review(fixture.ctx, fixture.cookie, "candidate-review", lifecycle.ReviewRequest{OperationID: claimed.ID, Mode: lifecycle.ReviewCandidate, Candidate: "verified-candidate", Reason: "matched provider request metadata"})
	if err != nil {
		t.Fatal(err)
	}
	if accessRequest.Operation != "runner.resolve" || accessRequest.Suboperation != "managed" || accessRequest.Binding.RunnerID != claimed.RunnerID || accessRequest.Binding.FabricID != claimed.FabricID || accessRequest.Binding.Revision != claimed.BindingRevision {
		t.Fatal("manual review access check lost its fixed Runner scope", accessRequest)
	}
	provider := &candidateProvider{result: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "verified-candidate", ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Millisecond)}}
	worker, err := NewWorker(fixture.store, completeReviewProviders(nil, provider), reviewWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	worked, err := worker.RunOnce(fixture.ctx)
	if err != nil || !worked {
		t.Fatal("manual review was not executed", worked, err)
	}
	provider.mu.Lock()
	calls := append([]fabric.CandidateCall(nil), provider.calls...)
	provider.mu.Unlock()
	if len(calls) != 1 || calls[0].CandidateResourceRef != "verified-candidate" || calls[0].Action.ID != action.ID || calls[0].Action.Issuer != claimed.Worker || calls[0].Action.ExecutionRevision != claimed.Revision {
		t.Fatal("candidate verification lost original action identity", calls)
	}
	resource, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
	if err != nil || resource.Ref != "verified-candidate" {
		t.Fatal("verified candidate was not associated", resource, err)
	}
	finished, err := service.ReviewStatus(fixture.ctx, fixture.cookie, review.ID)
	if err != nil || finished.Outcome != "succeeded" || finished.VerifiedResourceRef != "verified-candidate" || finished.CompletedAt.IsZero() {
		t.Fatal("manual review audit did not finish", finished, err)
	}
}

func TestManualReconcileUsesOnlyOriginalAction(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed, action := unresolvedCreateAction(t, fixture)
	service := newService(t, fixture, checkFunc(func(context.Context, access.Request) (access.Decision, error) {
		return allowDecision(), nil
	}), nil)
	review, err := service.Review(fixture.ctx, fixture.cookie, "reconcile-review", lifecycle.ReviewRequest{OperationID: claimed.ID, Mode: lifecycle.ReviewReconcile, Reason: "requested a fresh provider query"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &createProvider{reconcileResults: []fabric.Observation{{Outcome: fabric.OutcomeFailed}}}
	worker, err := NewWorker(fixture.store, completeReviewProviders(provider, nil), reviewWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	worked, err := worker.RunOnce(fixture.ctx)
	if err != nil || !worked {
		t.Fatal("manual reconciliation was not executed", worked, err)
	}
	provider.mu.Lock()
	createCalls, reconcileCalls := len(provider.createCalls), append([]fabric.ReconcileCall(nil), provider.reconcileCalls...)
	provider.mu.Unlock()
	if createCalls != 0 || len(reconcileCalls) != 1 || reconcileCalls[0].Action.ID != action.ID || reconcileCalls[0].Action.Issuer != claimed.Worker {
		t.Fatal("manual query repeated or rebound the create action", createCalls, reconcileCalls)
	}
	finished, err := service.ReviewStatus(fixture.ctx, fixture.cookie, review.ID)
	if err != nil || finished.Outcome != "failed" || finished.CompletedAt.IsZero() {
		t.Fatal("manual query result was not audited", finished, err)
	}
}

func TestManualReviewRejectsDeniedAccessAndUnverifiedCandidate(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed, _ := unresolvedCreateAction(t, fixture)
	denied := newService(t, fixture, checkFunc(func(context.Context, access.Request) (access.Decision, error) {
		return denyDecision(), nil
	}), nil)
	if _, err := denied.Review(fixture.ctx, fixture.cookie, "denied-review", lifecycle.ReviewRequest{OperationID: claimed.ID, Mode: lifecycle.ReviewReconcile, Reason: "try"}); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("denied review disclosed the operation", err)
	}
	service := newService(t, fixture, checkFunc(func(context.Context, access.Request) (access.Decision, error) { return allowDecision(), nil }), nil)
	review, err := service.Review(fixture.ctx, fixture.cookie, "mismatch-review", lifecycle.ReviewRequest{OperationID: claimed.ID, Mode: lifecycle.ReviewCandidate, Candidate: "candidate", Reason: "provider search"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &candidateProvider{result: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "different"}}
	worker, err := NewWorker(fixture.store, completeReviewProviders(nil, provider), reviewWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(fixture.ctx); !worked || !errors.Is(err, ErrProviderContract) {
		t.Fatal("mismatched provider evidence was accepted", worked, err)
	}
	if _, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("unverified candidate changed resource association", err)
	}
	pending, err := service.ReviewStatus(fixture.ctx, fixture.cookie, review.ID)
	if err != nil || !pending.CompletedAt.IsZero() || pending.Outcome != "" {
		t.Fatal("contract failure was recorded as verified", pending, err)
	}
}
