package managed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/fabric"
)

type pauseProvider struct {
	mu          sync.Mutex
	pauseCalls  []fabric.PauseResumeCall
	resumeCalls []fabric.PauseResumeCall
	err         error
}

func (p *pauseProvider) Pause(_ context.Context, call fabric.PauseResumeCall) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pauseCalls = append(p.pauseCalls, call)
	return p.err
}

func (p *pauseProvider) Resume(_ context.Context, call fabric.PauseResumeCall) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resumeCalls = append(p.resumeCalls, call)
	return p.err
}

func acceptedPause(t *testing.T, fixture serviceFixture) lifecycle.Operation {
	t.Helper()
	created := readyResourceForInspection(t, fixture)
	claim, err := fixture.store.ClaimManagedInspection(fixture.ctx, created.RunnerID, "personal-v1", wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := &fabric.ResourceCapabilities{PauseResume: true}
	if _, err := fixture.store.RecordManagedInspection(fixture.ctx, claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{
		Status: lifecycle.InspectionConfirmed, ResourceRef: claim.ResourceRef, ExpiresAt: time.Now().Add(time.Hour), State: string(fabric.ResourceReady), Capabilities: capabilities,
	}); err != nil {
		t.Fatal(err)
	}
	selected, err := fixture.store.RunnerResource(fixture.ctx, created.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := fixture.store.ManagedResource(fixture.ctx, created.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := fixture.store.CreateManagedPauseResume(fixture.ctx, fixture.user, credentialDigest(fixture.cookie), wire.ID(), selected, resource, wire.ID(), "pause")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.store.ClaimOperation(fixture.ctx, paused.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func TestPauseExecutorReconcilesLostResponseWithoutRepeatingMutation(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := acceptedPause(t, fixture)
	provider := &pauseProvider{err: context.DeadlineExceeded}
	inspector := &inspectProvider{result: fabric.Inspection{
		Status: lifecycle.InspectionConfirmed, ResourceRef: "bootstrap-resource", ExpiresAt: time.Now().Add(time.Hour),
		State: fabric.ResourcePaused, Capabilities: &fabric.ResourceCapabilities{PauseResume: true},
	}}
	executor, err := NewPauseResumeExecutor(fixture.store, map[string]fabric.PauseResumeProvider{"sandbox": provider}, map[string]fabric.InspectProvider{"sandbox": inspector})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, claimed); err != nil {
		t.Fatal(err)
	}
	action, err := fixture.store.ProviderAction(fixture.ctx, claimed.ID, "pause")
	if err != nil || action.Outcome != "timed_out" || !action.CompletedAt.IsZero() {
		t.Fatal("lost response was not left for reconciliation", action, err)
	}
	if err := fixture.store.YieldOperationLease(fixture.ctx, claimed); err != nil {
		t.Fatal(err)
	}
	takeover, err := fixture.store.ClaimOperation(fixture.ctx, claimed.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	provider.err = nil
	if err := executor.Execute(fixture.ctx, fixture.ctx, takeover); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	pauseCalls, resumeCalls := len(provider.pauseCalls), len(provider.resumeCalls)
	provider.mu.Unlock()
	inspector.mu.Lock()
	inspectCalls := append([]fabric.InspectCall(nil), inspector.calls...)
	inspector.mu.Unlock()
	if pauseCalls != 1 || resumeCalls != 0 || len(inspectCalls) != 1 {
		t.Fatal("pause was replayed instead of reconciled", pauseCalls, resumeCalls, len(inspectCalls))
	}
	call := inspectCalls[0]
	if call.ProviderBindingID != claimed.ProviderBindingID || call.ProviderBindingRevision != claimed.ProviderBindingRevision || call.ResourceRef != "bootstrap-resource" {
		t.Fatal("inspection lost the exact provider binding", call)
	}
	operation, err := fixture.store.Operation(fixture.ctx, claimed.ID)
	resource, resourceErr := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
	if err != nil || resourceErr != nil || !operation.Finished || operation.Outcome != "succeeded" || resource.State != string(fabric.ResourcePaused) || !resource.AccessSuspended {
		t.Fatal("provider-confirmed pause did not finish safely", operation, resource, errors.Join(err, resourceErr))
	}
}
