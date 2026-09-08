package managed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/fabric"
)

type renewProvider struct {
	mu               sync.Mutex
	renewCalls       []fabric.RenewCall
	reconcileCalls   []fabric.RenewReconcileCall
	renewResult      fabric.Observation
	renewErr         error
	reconcileResults []fabric.Observation
	reconcileErr     error
}

func (p *renewProvider) Renew(_ context.Context, call fabric.RenewCall) (fabric.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.renewCalls = append(p.renewCalls, call)
	return p.renewResult, p.renewErr
}

func (p *renewProvider) ReconcileRenew(_ context.Context, call fabric.RenewReconcileCall) (fabric.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reconcileCalls = append(p.reconcileCalls, call)
	if len(p.reconcileResults) == 0 {
		return fabric.Observation{}, p.reconcileErr
	}
	result := p.reconcileResults[0]
	p.reconcileResults = p.reconcileResults[1:]
	return result, p.reconcileErr
}

func claimedAutomaticRenewal(t *testing.T, fixture serviceFixture, ttl time.Duration) lifecycle.Operation {
	t.Helper()
	created := readyResourceForInspection(t, fixture)
	claim, err := fixture.store.ClaimManagedInspection(fixture.ctx, created.RunnerID, "personal-v1", wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := fixture.store.RecordManagedInspection(fixture.ctx, claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{
		Status: lifecycle.InspectionConfirmed, ResourceRef: claim.ResourceRef, ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	if err != nil || schedule.RenewUntil.IsZero() {
		t.Fatal("renewal was not scheduled", schedule, err)
	}
	op, consumed, err := fixture.store.ClaimScheduledManagedRenewal(fixture.ctx, schedule, "personal-v1", wire.ID(), ttl)
	if err != nil || !consumed || op.ID == "" {
		t.Fatal("renewal operation was not claimed", op, consumed, err)
	}
	return op
}

func TestRenewalExecutorDispatchesOnceAndPersistsExpiry(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := claimedAutomaticRenewal(t, fixture, time.Minute)
	request, err := fixture.store.ManagedRenewal(fixture.ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	expires := request.Until.Add(time.Minute)
	provider := &renewProvider{renewResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource", ExpiresAt: expires}}
	providers := map[string]fabric.RenewProvider{"sandbox": provider}
	executor, err := NewRenewalExecutor(fixture.store, providers)
	if err != nil {
		t.Fatal(err)
	}
	delete(providers, "sandbox")
	if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); err != nil {
		t.Fatal("completed renewal was not idempotent", err)
	}
	provider.mu.Lock()
	renewCalls := append([]fabric.RenewCall(nil), provider.renewCalls...)
	reconciles := len(provider.reconcileCalls)
	provider.mu.Unlock()
	if len(renewCalls) != 1 || reconciles != 0 {
		t.Fatal("renewal was repeated or unnecessarily reconciled", renewCalls, reconciles)
	}
	action := renewCalls[0].Action
	if action.ID == "" || action.OperationID != claimed.ID || action.RunnerID != claimed.RunnerID || action.FabricID != "sandbox" || action.ResourceRef != "bootstrap-resource" || action.Issuer != claimed.Worker || action.ExecutionRevision != claimed.Revision || !action.RenewUntil.Equal(request.Until) {
		t.Fatal("renewal call lost its frozen action identity", action)
	}
	resource, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
	if err != nil || !resource.ExpiresAt.Equal(expires) {
		t.Fatal("confirmed renewal expiry was not persisted", resource, err)
	}
	operation, err := fixture.store.Operation(fixture.ctx, claimed.ID)
	if err != nil || !operation.Finished || operation.Outcome != "succeeded" || operation.Exclusive || operation.Worker != "" {
		t.Fatal("successful renewal did not finish its operation", operation, err)
	}
	schedule, err := fixture.store.ManagedRenewalSchedule(fixture.ctx, claimed.RunnerID)
	if err != nil || schedule.Reason != "" || schedule.NextCheckAt.IsZero() || !schedule.RenewUntil.IsZero() {
		t.Fatal("renewal completion did not return to inspection", schedule, err)
	}
}

func TestRenewalExecutorTakeoverOnlyReconciles(t *testing.T) {
	fixture := newServiceFixture(t)
	original := claimedAutomaticRenewal(t, fixture, time.Second)
	request, err := fixture.store.ManagedRenewal(fixture.ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider := &renewProvider{
		renewResult:      fabric.Observation{Outcome: fabric.OutcomeUnknown},
		reconcileResults: []fabric.Observation{{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource", ExpiresAt: request.Until}},
	}
	executor, err := NewRenewalExecutor(fixture.store, map[string]fabric.RenewProvider{"sandbox": provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, original); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	takeover, err := fixture.store.ClaimRecoverableManagedRenewal(fixture.ctx, original.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, takeover); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.renewCalls) != 1 || len(provider.reconcileCalls) != 1 {
		t.Fatal("takeover repeated renewal", len(provider.renewCalls), len(provider.reconcileCalls))
	}
	first, reconciled := provider.renewCalls[0].Action, provider.reconcileCalls[0]
	if reconciled.Action.ID != first.ID || reconciled.Action.Issuer != original.Worker || reconciled.Action.ExecutionRevision != original.Revision || reconciled.Action.Issuer == takeover.Worker || reconciled.KnownExpiresAt.IsZero() {
		t.Fatal("takeover changed the original action", first, reconciled, takeover.Lease)
	}
}

func TestRenewalExecutorErrorsAreConservative(t *testing.T) {
	for name, providerErr := range map[string]error{"unavailable": errors.New("connection lost"), "deadline": context.DeadlineExceeded} {
		t.Run(name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			claimed := claimedAutomaticRenewal(t, fixture, time.Minute)
			before, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
			if err != nil {
				t.Fatal(err)
			}
			provider := &renewProvider{renewResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "wrong", ExpiresAt: time.Now().Add(24 * time.Hour)}, renewErr: providerErr}
			executor, err := NewRenewalExecutor(fixture.store, map[string]fabric.RenewProvider{"sandbox": provider})
			if err != nil {
				t.Fatal(err)
			}
			if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); err != nil {
				t.Fatal(err)
			}
			action, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "renew")
			expected := "unknown"
			if errors.Is(providerErr, context.DeadlineExceeded) {
				expected = "timed_out"
			}
			if err != nil || action.Outcome != expected || !action.CompletedAt.IsZero() {
				t.Fatal("ambiguous renewal became terminal", action, err)
			}
			after, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
			if err != nil || after != before {
				t.Fatal("fields returned with renewal error changed resource", before, after, err)
			}
		})
	}
}

func TestRenewalExecutorRejectsMissingProviderAndInvalidFacts(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := claimedAutomaticRenewal(t, fixture, time.Minute)
	executor, err := NewRenewalExecutor(fixture.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatal("missing renewal provider was not reported", err)
	}
	if _, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "renew"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("missing provider reserved a renewal action", err)
	}
	request, err := fixture.store.ManagedRenewal(fixture.ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider := &renewProvider{renewResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource", ExpiresAt: request.Until.Add(-time.Millisecond)}}
	executor, err = NewRenewalExecutor(fixture.store, map[string]fabric.RenewProvider{"sandbox": provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); !errors.Is(err, ErrProviderContract) {
		t.Fatal("renewal success before its target was accepted", err)
	}
	if _, err := NewRenewalExecutor(nil, nil); err == nil {
		t.Fatal("renewal executor accepted no shared metadata")
	}
	if _, err := NewRenewalExecutor(fixture.store, map[string]fabric.RenewProvider{"attached": provider}); err == nil {
		t.Fatal("renewal executor accepted Attached as a provider")
	}
}

func TestWorkerConsumesAndExecutesScheduledRenewal(t *testing.T) {
	fixture := newServiceFixture(t)
	created := readyResourceForInspection(t, fixture)
	claim, err := fixture.store.ClaimManagedInspection(fixture.ctx, created.RunnerID, "personal-v1", wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := fixture.store.RecordManagedInspection(fixture.ctx, claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{
		Status: lifecycle.InspectionConfirmed, ResourceRef: claim.ResourceRef, ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &renewProvider{renewResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: claim.ResourceRef, ExpiresAt: schedule.RenewUntil}}
	config := testWorkerConfig()
	config.RenewalPolicyVersion = "personal-v1"
	worker, err := NewWorker(fixture.store, ProviderSet{Renew: map[string]fabric.RenewProvider{"sandbox": provider}}, config)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(fixture.ctx); err != nil || !worked {
		t.Fatal("worker did not execute scheduled renewal", worked, err)
	}
	provider.mu.Lock()
	calls := len(provider.renewCalls)
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatal("worker did not dispatch exactly one renewal", calls)
	}
	if candidates, err := fixture.store.RecoverableManagedRenewalSchedulesFor(fixture.ctx, []string{"sandbox"}, "personal-v1", 32); err != nil || len(candidates) != 0 {
		t.Fatal("worker left the schedule dispatchable", candidates, err)
	}
	if _, err := NewWorker(fixture.store, ProviderSet{Renew: map[string]fabric.RenewProvider{"sandbox": provider}}, testWorkerConfig()); err == nil {
		t.Fatal("renewal worker accepted no policy version")
	}
}
