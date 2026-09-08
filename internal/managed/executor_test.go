package managed

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/fabric"
)

type createProvider struct {
	mu               sync.Mutex
	createCalls      []fabric.CreateCall
	reconcileCalls   []fabric.ReconcileCall
	createResult     fabric.Observation
	createErr        error
	reconcileResults []fabric.Observation
	reconcileErr     error
}

func (p *createProvider) Create(_ context.Context, call fabric.CreateCall) (fabric.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.createCalls = append(p.createCalls, call)
	return p.createResult, p.createErr
}

func (p *createProvider) ReconcileCreate(_ context.Context, call fabric.ReconcileCall) (fabric.Observation, error) {
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

func claimedCreation(t *testing.T, fixture serviceFixture) lifecycle.Operation {
	return claimedCreationWithTTL(t, fixture, time.Minute)
}

func claimedCreationWithTTL(t *testing.T, fixture serviceFixture, ttl time.Duration) lifecycle.Operation {
	t.Helper()
	created, err := fixture.store.CreateManaged(fixture.ctx, fixture.user, credentialDigest(fixture.cookie), wire.ID(), createRequest("visible"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.store.ClaimOperation(fixture.ctx, created.Operation.ID, wire.ID(), ttl)
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func credentialDigest(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func TestExecutorDispatchesCreateOnceAndPersistsProviderFacts(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := claimedCreation(t, fixture)
	expires := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	provider := &createProvider{createResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "provider-resource", ExpiresAt: expires}}
	providers := map[string]fabric.CreateProvider{"sandbox": provider}
	executor, err := NewExecutor(fixture.store, providers)
	if err != nil {
		t.Fatal(err)
	}
	delete(providers, "sandbox")
	if err := executor.ExecuteCreate(fixture.ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err := executor.ExecuteCreate(fixture.ctx, claimed); err != nil {
		t.Fatal("completed action was not idempotent", err)
	}
	provider.mu.Lock()
	createCalls := append([]fabric.CreateCall(nil), provider.createCalls...)
	reconcileCount := len(provider.reconcileCalls)
	provider.mu.Unlock()
	if len(createCalls) != 1 || reconcileCount != 0 {
		t.Fatal("create was repeated or unnecessarily reconciled", len(createCalls), reconcileCount)
	}
	call := createCalls[0]
	if call.Action.ID == "" || call.Action.OperationID != claimed.ID || call.Action.RunnerID != claimed.RunnerID || call.Action.FabricID != "sandbox" || call.Action.RequestDigest != claimed.Digest || call.Action.ResourceRef != "" || call.Action.Issuer != claimed.Worker || call.Action.BindingRevision != 1 || call.Action.ExecutionRevision != claimed.Revision {
		t.Fatal("provider call did not carry the reserved execution identity", call.Action)
	}
	call.Request.Parameters["cpu"][0] = '7'
	recovered, err := fixture.store.ManagedCreation(fixture.ctx, claimed.PrincipalID, claimed.RequestKey)
	if err != nil || string(recovered.Spec.Parameters["cpu"]) != "2" {
		t.Fatal("provider mutated durable creation intent", err)
	}
	resource, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
	if err != nil || resource.Ref != "provider-resource" || !resource.ExpiresAt.Equal(expires) || resource.Gone {
		t.Fatal("confirmed provider facts were not committed", resource, err)
	}
}

func TestExecutorReconcilesUnknownCreateWithoutRedispatch(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := claimedCreation(t, fixture)
	provider := &createProvider{
		createResult:     fabric.Observation{Outcome: fabric.OutcomeUnknown, ResourceRef: "partial-resource"},
		reconcileResults: []fabric.Observation{{Outcome: fabric.OutcomeSucceeded, ResourceRef: "partial-resource"}},
	}
	executor, err := NewExecutor(fixture.store, map[string]fabric.CreateProvider{"sandbox": provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.ExecuteCreate(fixture.ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err := executor.ExecuteCreate(fixture.ctx, claimed); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.createCalls) != 1 || len(provider.reconcileCalls) != 1 || provider.reconcileCalls[0].Action.ID != provider.createCalls[0].Action.ID || provider.reconcileCalls[0].KnownResourceRef != "partial-resource" {
		t.Fatal("unknown action did not reconcile its original key", provider.createCalls, provider.reconcileCalls)
	}
	if action, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "create"); err != nil || action.Outcome != "succeeded" || action.CompletedAt.IsZero() {
		t.Fatal("reconciliation did not complete the original action", action, err)
	}
}

func TestExecutorTakeoverReconcilesOriginalCreate(t *testing.T) {
	fixture := newServiceFixture(t)
	original := claimedCreationWithTTL(t, fixture, time.Second)
	provider := &createProvider{
		createResult:     fabric.Observation{Outcome: fabric.OutcomeUnknown},
		reconcileResults: []fabric.Observation{{Outcome: fabric.OutcomeSucceeded, ResourceRef: "recovered-resource"}},
	}
	executor, err := NewExecutor(fixture.store, map[string]fabric.CreateProvider{"sandbox": provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.ExecuteCreate(fixture.ctx, original); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	takeover, err := fixture.store.ClaimOperation(fixture.ctx, original.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if takeover.Worker == original.Worker || takeover.Revision <= original.Revision {
		t.Fatal("operation was not claimed by a new execution", takeover.Lease)
	}
	if err := executor.ExecuteCreate(fixture.ctx, takeover); err != nil {
		t.Fatal(err)
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.createCalls) != 1 || len(provider.reconcileCalls) != 1 {
		t.Fatal("takeover repeated create instead of reconciling", len(provider.createCalls), len(provider.reconcileCalls))
	}
	createdAction := provider.createCalls[0].Action
	reconciledAction := provider.reconcileCalls[0].Action
	if reconciledAction.ID != createdAction.ID || reconciledAction.Issuer != original.Worker || reconciledAction.ExecutionRevision != original.Revision || reconciledAction.Issuer == takeover.Worker || reconciledAction.ExecutionRevision == takeover.Revision {
		t.Fatal("takeover did not preserve the original provider execution identity", createdAction, reconciledAction, takeover.Lease)
	}
}

func TestExecutorRecordsAmbiguousProviderErrors(t *testing.T) {
	for name, providerErr := range map[string]error{"timeout": context.DeadlineExceeded, "failure": errors.New("transport closed")} {
		t.Run(name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			claimed := claimedCreation(t, fixture)
			provider := &createProvider{createResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "unverified-resource"}, createErr: providerErr}
			executor, err := NewExecutor(fixture.store, map[string]fabric.CreateProvider{"sandbox": provider})
			if err != nil {
				t.Fatal(err)
			}
			if err := executor.ExecuteCreate(fixture.ctx, claimed); err != nil {
				t.Fatal(err)
			}
			expected := "unknown"
			if errors.Is(providerErr, context.DeadlineExceeded) {
				expected = "timed_out"
			}
			if action, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "create"); err != nil || action.Outcome != expected || !action.CompletedAt.IsZero() {
				t.Fatal("ambiguous provider result became terminal", action, err)
			}
			if _, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID); err == nil {
				t.Fatal("facts returned alongside a provider error were persisted")
			}
		})
	}
}

func TestExecutorRejectsMissingProviderAndInvalidFacts(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := claimedCreation(t, fixture)
	executor, err := NewExecutor(fixture.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.ExecuteCreate(fixture.ctx, claimed); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatal("missing provider reserved or executed an action", err)
	}
	if _, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "create"); err == nil {
		t.Fatal("missing provider left a dispatch reservation")
	}
	provider := &createProvider{createResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded}}
	executor, err = NewExecutor(fixture.store, map[string]fabric.CreateProvider{"sandbox": provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.ExecuteCreate(fixture.ctx, claimed); !errors.Is(err, ErrProviderContract) {
		t.Fatal("invalid success facts were accepted", err)
	}
	provider.createResult = fabric.Observation{Outcome: fabric.OutcomeFailed}
	if err := fixture.store.RecordProviderAction(fixture.ctx, claimed, provider.createCalls[0].Action.ID, lifecycle.ActionObservation{Outcome: "failed"}); err != nil {
		t.Fatal(err)
	}
	executor, err = NewExecutor(fixture.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.ExecuteCreate(fixture.ctx, claimed); err != nil {
		t.Fatal("completed action required its removed provider", err)
	}
	if _, err := NewExecutor(nil, nil); err == nil {
		t.Fatal("executor accepted no shared metadata")
	}
	if _, err := NewExecutor(fixture.store, map[string]fabric.CreateProvider{" attached": provider}); err == nil {
		t.Fatal("executor accepted an invalid provider namespace")
	}
}
