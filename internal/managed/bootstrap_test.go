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

type bootstrapProvider struct {
	mu               sync.Mutex
	bootstrapCalls   []fabric.BootstrapCall
	reconcileCalls   []fabric.BootstrapReconcileCall
	bootstrapResult  fabric.Observation
	bootstrapErr     error
	reconcileResults []fabric.Observation
	reconcileErr     error
}

func (p *bootstrapProvider) Bootstrap(_ context.Context, call fabric.BootstrapCall) (fabric.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bootstrapCalls = append(p.bootstrapCalls, call)
	return p.bootstrapResult, p.bootstrapErr
}

func (p *bootstrapProvider) ReconcileBootstrap(_ context.Context, call fabric.BootstrapReconcileCall) (fabric.Observation, error) {
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

func bootstrapConfig() BootstrapConfig {
	return BootstrapConfig{
		PublicURL: "https://DUNE.test/base", GatewayURL: "wss://DUNE.test/machines/connect",
		Version: "fabricd-v1", EnrollmentLifetime: 10 * time.Minute,
	}
}

func claimedBootstrap(t *testing.T, fixture serviceFixture, ttl time.Duration) lifecycle.Operation {
	t.Helper()
	created := claimedCreation(t, fixture)
	create := &createProvider{createResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource", ExpiresAt: time.Now().Add(time.Hour)}}
	executor, err := NewExecutor(fixture.store, map[string]fabric.CreateProvider{"sandbox": create})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.ExecuteCreate(fixture.ctx, created); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.YieldOperationLease(fixture.ctx, created); err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.store.ClaimOperation(fixture.ctx, created.ID, wire.ID(), ttl)
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func TestBootstrapExecutorDispatchesOneResourceBoundGrant(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := claimedBootstrap(t, fixture, time.Minute)
	provider := &bootstrapProvider{bootstrapResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource"}}
	providers := map[string]fabric.BootstrapProvider{"sandbox": provider}
	executor, err := NewBootstrapExecutor(fixture.store, providers, bootstrapConfig())
	if err != nil {
		t.Fatal(err)
	}
	delete(providers, "sandbox")
	if err := executor.Execute(fixture.ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, claimed); err != nil {
		t.Fatal("completed bootstrap was not idempotent", err)
	}
	provider.mu.Lock()
	if len(provider.bootstrapCalls) != 1 || len(provider.reconcileCalls) != 0 {
		provider.mu.Unlock()
		t.Fatal("bootstrap was repeated or reconciled", len(provider.bootstrapCalls), len(provider.reconcileCalls))
	}
	call := provider.bootstrapCalls[0]
	provider.mu.Unlock()
	if call.Action.ID == "" || call.Action.OperationID != claimed.ID || call.Action.RunnerID != claimed.RunnerID || call.Action.FabricID != "sandbox" || call.Action.ResourceRef != "bootstrap-resource" || call.Action.Issuer != claimed.Worker || call.Action.ExecutionRevision != claimed.Revision || len(call.EnrollmentToken) != 64 || call.EnrollmentExpiresAt.IsZero() || call.Endpoint != "https://dune.test/base/" || call.GatewayURL != "wss://dune.test/machines/connect" || call.Version != "fabricd-v1" {
		t.Fatal("bootstrap call lost its frozen target or action identity", call)
	}
	if action, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "bootstrap"); err != nil || action.Outcome != "succeeded" || action.CompletedAt.IsZero() || action.Digest != call.Action.RequestDigest {
		t.Fatal("bootstrap result was not persisted", action, err)
	}
	machine, credential, err := fixture.store.Enroll(fixture.ctx, call.EnrollmentToken, "linux", "amd64")
	if err != nil || machine.RunnerID != claimed.RunnerID || credential == "" {
		t.Fatal("completed bootstrap grant did not bind fabricd", machine, err)
	}
}

func TestBootstrapExecutorUsesConsumedGrantAsRecoveryEvidence(t *testing.T) {
	fixture := newServiceFixture(t)
	original := claimedBootstrap(t, fixture, time.Second)
	provider := &bootstrapProvider{bootstrapResult: fabric.Observation{Outcome: fabric.OutcomeUnknown}}
	executor, err := NewBootstrapExecutor(fixture.store, map[string]fabric.BootstrapProvider{"sandbox": provider}, bootstrapConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, original); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	if len(provider.bootstrapCalls) != 1 {
		provider.mu.Unlock()
		t.Fatal("initial bootstrap was not dispatched")
	}
	call := provider.bootstrapCalls[0]
	provider.mu.Unlock()
	if _, _, err := fixture.store.Enroll(fixture.ctx, call.EnrollmentToken, "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	takeover, err := fixture.store.ClaimOperation(fixture.ctx, original.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewBootstrapExecutor(fixture.store, nil, bootstrapConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Execute(fixture.ctx, takeover); err != nil {
		t.Fatal("durable binding did not complete unknown bootstrap", err)
	}
	provider.mu.Lock()
	reconciles := len(provider.reconcileCalls)
	provider.mu.Unlock()
	if reconciles != 0 {
		t.Fatal("provider was queried after the grant was consumed")
	}
	if action, err := fixture.store.ProviderAction(fixture.ctx, original.ID, "bootstrap"); err != nil || action.Outcome != "succeeded" || action.CompletedAt.IsZero() || action.Worker != original.Worker || action.ExecutionRevision != original.Revision {
		t.Fatal("binding evidence changed the original action identity", action, err)
	}
}

func TestBootstrapExecutorTakeoverOnlyReconcilesOriginalAction(t *testing.T) {
	fixture := newServiceFixture(t)
	original := claimedBootstrap(t, fixture, time.Second)
	provider := &bootstrapProvider{
		bootstrapResult:  fabric.Observation{Outcome: fabric.OutcomeUnknown},
		reconcileResults: []fabric.Observation{{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource"}},
	}
	executor, err := NewBootstrapExecutor(fixture.store, map[string]fabric.BootstrapProvider{"sandbox": provider}, bootstrapConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, original); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	takeover, err := fixture.store.ClaimOperation(fixture.ctx, original.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	changed := bootstrapConfig()
	changed.Version = "fabricd-v2"
	recovery, err := NewBootstrapExecutor(fixture.store, map[string]fabric.BootstrapProvider{"sandbox": provider}, changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Execute(fixture.ctx, takeover); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.bootstrapCalls) != 1 || len(provider.reconcileCalls) != 1 {
		t.Fatal("takeover repeated bootstrap", len(provider.bootstrapCalls), len(provider.reconcileCalls))
	}
	first, reconciled := provider.bootstrapCalls[0].Action, provider.reconcileCalls[0].Action
	if reconciled.ID != first.ID || reconciled.Issuer != original.Worker || reconciled.ExecutionRevision != original.Revision || reconciled.Issuer == takeover.Worker {
		t.Fatal("takeover changed the provider action identity", first, reconciled, takeover.Lease)
	}
}

func TestBootstrapExecutorPersistsProviderDeadlineWithDatabaseContext(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := claimedBootstrap(t, fixture, time.Minute)
	provider := &bootstrapProvider{bootstrapErr: context.DeadlineExceeded, bootstrapResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "ignored"}}
	executor, err := NewBootstrapExecutor(fixture.store, map[string]fabric.BootstrapProvider{"sandbox": provider}, bootstrapConfig())
	if err != nil {
		t.Fatal(err)
	}
	providerCtx, cancel := context.WithCancel(fixture.ctx)
	cancel()
	if err := executor.execute(fixture.ctx, providerCtx, claimed); err != nil {
		t.Fatal(err)
	}
	action, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "bootstrap")
	if err != nil || action.Outcome != "timed_out" || !action.CompletedAt.IsZero() {
		t.Fatal("bootstrap deadline was not durably separated", action, err)
	}
}

func TestBootstrapExecutorRejectsUnavailableOrChangedConfiguration(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := claimedBootstrap(t, fixture, time.Minute)
	executor, err := NewBootstrapExecutor(fixture.store, nil, bootstrapConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, claimed); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatal("missing provider reserved bootstrap", err)
	}
	if _, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "bootstrap"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("missing provider persisted an action", err)
	}
	for _, config := range []BootstrapConfig{{}, {PublicURL: "https://dune.test/", GatewayURL: "ws://dune.test/tunnel", Version: "v1", EnrollmentLifetime: time.Minute}, {PublicURL: "https://dune.test/", Version: " v1", EnrollmentLifetime: time.Minute}, {PublicURL: "https://dune.test/", Version: "v1", EnrollmentLifetime: 11 * time.Minute}} {
		if _, err := NewBootstrapExecutor(fixture.store, nil, config); err == nil {
			t.Fatal("invalid bootstrap configuration accepted", config)
		}
	}
	if _, err := NewBootstrapExecutor(nil, nil, bootstrapConfig()); err == nil {
		t.Fatal("bootstrap executor accepted no shared metadata")
	}
}
