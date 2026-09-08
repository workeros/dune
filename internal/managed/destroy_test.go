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

type destroyProvider struct {
	mu               sync.Mutex
	destroyCalls     []fabric.DestroyCall
	reconcileCalls   []fabric.DestroyReconcileCall
	destroyResult    fabric.Observation
	destroyErr       error
	reconcileResults []fabric.Observation
	reconcileErr     error
}

func (p *destroyProvider) Destroy(_ context.Context, call fabric.DestroyCall) (fabric.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyCalls = append(p.destroyCalls, call)
	return p.destroyResult, p.destroyErr
}

func (p *destroyProvider) ReconcileDestroy(_ context.Context, call fabric.DestroyReconcileCall) (fabric.Observation, error) {
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

func claimedDestroy(t *testing.T, fixture serviceFixture, ttl time.Duration) lifecycle.Operation {
	t.Helper()
	created := readyResourceForInspection(t, fixture)
	selected, err := fixture.store.RunnerResource(fixture.ctx, created.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := fixture.store.ManagedResource(fixture.ctx, created.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	destroyed, err := fixture.store.CreateManagedDestroy(fixture.ctx, fixture.user, credentialDigest(fixture.cookie), wire.ID(), selected, resource, wire.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.store.ClaimRecoverableManagedDestroy(fixture.ctx, destroyed.ID, wire.ID(), ttl)
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func TestDestroyExecutorDispatchesOnceAndPersistsGone(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := claimedDestroy(t, fixture, time.Minute)
	provider := &destroyProvider{destroyResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource", Gone: true}}
	providers := map[string]fabric.DestroyProvider{"sandbox": provider}
	executor, err := NewDestroyExecutor(fixture.store, providers)
	if err != nil {
		t.Fatal(err)
	}
	delete(providers, "sandbox")
	if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); err != nil {
		t.Fatal("completed deletion was not idempotent", err)
	}
	provider.mu.Lock()
	calls := append([]fabric.DestroyCall(nil), provider.destroyCalls...)
	reconciles := len(provider.reconcileCalls)
	provider.mu.Unlock()
	if len(calls) != 1 || reconciles != 0 {
		t.Fatal("deletion was repeated or unnecessarily reconciled", calls, reconciles)
	}
	action := calls[0].Action
	if action.ID == "" || action.OperationID != claimed.ID || action.RunnerID != claimed.RunnerID || action.FabricID != "sandbox" || action.ResourceRef != "bootstrap-resource" || action.RequestDigest != claimed.Digest || action.Issuer != claimed.Worker || action.ExecutionRevision != claimed.Revision {
		t.Fatal("destroy call lost its durable action identity", action)
	}
	resource, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
	if err != nil || !resource.Gone || !resource.AccessClosed || resource.Ref != "bootstrap-resource" {
		t.Fatal("confirmed deletion was not persisted", resource, err)
	}
	operation, err := fixture.store.Operation(fixture.ctx, claimed.ID)
	if err != nil || !operation.Finished || operation.Outcome != "succeeded" || operation.Exclusive || operation.Worker != "" {
		t.Fatal("successful deletion did not finish its operation", operation, err)
	}
}

func TestDestroyExecutorTakeoverOnlyReconciles(t *testing.T) {
	fixture := newServiceFixture(t)
	original := claimedDestroy(t, fixture, time.Second)
	provider := &destroyProvider{
		destroyResult:    fabric.Observation{Outcome: fabric.OutcomeUnknown},
		reconcileResults: []fabric.Observation{{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource", Gone: true}},
	}
	executor, err := NewDestroyExecutor(fixture.store, map[string]fabric.DestroyProvider{"sandbox": provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, original); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	takeover, err := fixture.store.ClaimRecoverableManagedDestroy(fixture.ctx, original.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, takeover); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.destroyCalls) != 1 || len(provider.reconcileCalls) != 1 {
		t.Fatal("takeover repeated deletion", len(provider.destroyCalls), len(provider.reconcileCalls))
	}
	first, reconciled := provider.destroyCalls[0].Action, provider.reconcileCalls[0].Action
	if reconciled.ID != first.ID || reconciled.Issuer != original.Worker || reconciled.ExecutionRevision != original.Revision || reconciled.Issuer == takeover.Worker {
		t.Fatal("takeover changed the original deletion action", first, reconciled)
	}
}

func TestDestroyExecutorErrorsAndFactsAreConservative(t *testing.T) {
	for name, providerErr := range map[string]error{"unavailable": errors.New("connection lost"), "deadline": context.DeadlineExceeded} {
		t.Run(name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			claimed := claimedDestroy(t, fixture, time.Minute)
			provider := &destroyProvider{destroyResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource", Gone: true}, destroyErr: providerErr}
			executor, err := NewDestroyExecutor(fixture.store, map[string]fabric.DestroyProvider{"sandbox": provider})
			if err != nil {
				t.Fatal(err)
			}
			if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); err != nil {
				t.Fatal(err)
			}
			action, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "destroy")
			expected := "unknown"
			if errors.Is(providerErr, context.DeadlineExceeded) {
				expected = "timed_out"
			}
			if err != nil || action.Outcome != expected || !action.CompletedAt.IsZero() {
				t.Fatal("ambiguous deletion became terminal", action, err)
			}
			resource, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
			if err != nil || resource.Gone || !resource.AccessClosed {
				t.Fatal("fields returned with provider error changed resource", resource, err)
			}
		})
	}

	fixture := newServiceFixture(t)
	claimed := claimedDestroy(t, fixture, time.Minute)
	executor, err := NewDestroyExecutor(fixture.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatal("missing destroy provider was not reported", err)
	}
	if _, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "destroy"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("missing provider reserved a destroy action", err)
	}
	bad := &destroyProvider{destroyResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource", Gone: false}}
	executor, err = NewDestroyExecutor(fixture.store, map[string]fabric.DestroyProvider{"sandbox": bad})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); !errors.Is(err, ErrProviderContract) {
		t.Fatal("success without Gone evidence was accepted", err)
	}
	if _, err := NewDestroyExecutor(nil, nil); err == nil {
		t.Fatal("destroy executor accepted no shared metadata")
	}
}

func TestWorkerAdvancesDestroyBeforeMaintenance(t *testing.T) {
	fixture := newServiceFixture(t)
	created := readyResourceForInspection(t, fixture)
	selected, err := fixture.store.RunnerResource(fixture.ctx, created.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := fixture.store.ManagedResource(fixture.ctx, created.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CreateManagedDestroy(fixture.ctx, fixture.user, credentialDigest(fixture.cookie), wire.ID(), selected, resource, wire.ID(), 0); err != nil {
		t.Fatal(err)
	}
	provider := &destroyProvider{destroyResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: resource.Ref, Gone: true}}
	worker, err := NewWorker(fixture.store, ProviderSet{Destroy: map[string]fabric.DestroyProvider{"sandbox": provider}}, testWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	worked, err := worker.RunOnce(fixture.ctx)
	if err != nil || !worked {
		t.Fatal("worker did not advance accepted deletion", worked, err)
	}
	provider.mu.Lock()
	calls := len(provider.destroyCalls)
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatal("worker did not dispatch one deletion", calls)
	}
	current, err := fixture.store.ManagedResource(fixture.ctx, created.RunnerID)
	if err != nil || !current.Gone {
		t.Fatal("worker did not persist provider deletion", current, err)
	}
}

func TestWorkerConfirmsGatewayClosureBeforeDestroy(t *testing.T) {
	fixture := newServiceFixture(t)
	created := readyResourceForInspection(t, fixture)
	selected, err := fixture.store.RunnerResource(fixture.ctx, created.RunnerID)
	if err != nil || selected.Runner.Binding == nil {
		t.Fatal("managed machine binding setup", selected, err)
	}
	resource, err := fixture.store.ManagedResource(fixture.ctx, created.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	instanceID := wire.ID()
	destroyed, err := fixture.store.CreateManagedDestroy(fixture.ctx, fixture.user, credentialDigest(fixture.cookie), wire.ID(), selected, resource, instanceID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	closeCalls := 0
	provider := &destroyProvider{destroyResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: resource.Ref, Gone: true}}
	config := testWorkerConfig()
	config.InstanceID = instanceID
	config.CloseTarget = func(target string) <-chan struct{} {
		closeCalls++
		if target != destroyed.MachineID {
			t.Fatalf("worker closed %q instead of %q", target, destroyed.MachineID)
		}
		return closed
	}
	worker, err := NewWorker(fixture.store, ProviderSet{Destroy: map[string]fabric.DestroyProvider{"sandbox": provider}}, config)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(fixture.ctx); err != nil || worked || closeCalls != 1 {
		t.Fatal("unfinished Gateway closure advanced deletion", worked, closeCalls, err)
	}
	close(closed)
	if worked, err := worker.RunOnce(fixture.ctx); err != nil || !worked {
		t.Fatal("completed Gateway closure was not confirmed", worked, err)
	}
	if len(provider.destroyCalls) != 0 {
		t.Fatal("closure confirmation also dispatched provider deletion")
	}
	current, err := fixture.store.ManagedDestroyOperation(fixture.ctx, destroyed.ID)
	if err != nil || current.AccessCloseOutcome != lifecycle.AccessCloseConfirmed {
		t.Fatal("worker did not persist closure confirmation", current, err)
	}
	if worked, err := worker.RunOnce(fixture.ctx); err != nil || !worked {
		t.Fatal("confirmed closure did not enable provider deletion", worked, err)
	}
	if len(provider.destroyCalls) != 1 {
		t.Fatal("provider deletion count", len(provider.destroyCalls))
	}
}
