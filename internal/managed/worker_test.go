package managed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/fabric"
)

func pendingCreation(t *testing.T, fixture serviceFixture) lifecycle.Creation {
	t.Helper()
	created, err := fixture.store.CreateManaged(fixture.ctx, fixture.user, credentialDigest(fixture.cookie), wire.ID(), createRequest("visible"))
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func testWorkerConfig() WorkerConfig {
	return WorkerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: time.Second, CallTimeout: time.Second}
}

func TestWorkerClaimsCreateAndYieldsItsExecutionLease(t *testing.T) {
	fixture := newServiceFixture(t)
	created := pendingCreation(t, fixture)
	provider := &createProvider{createResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "created-resource"}}
	worker, err := NewWorker(fixture.store, map[string]fabric.CreateProvider{"sandbox": provider}, testWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	worked, err := worker.RunOnce(fixture.ctx)
	if err != nil || !worked {
		t.Fatal("worker did not advance accepted creation", worked, err)
	}
	operation, err := fixture.store.Operation(fixture.ctx, created.Operation.ID)
	if err != nil || operation.Revision != 1 || operation.Worker != "" || !operation.Until.IsZero() || operation.Outcome != "" || operation.Finished || !operation.Exclusive {
		t.Fatal("create stage did not yield only its execution lease", operation, err)
	}
	if worked, err := worker.RunOnce(fixture.ctx); err != nil || worked {
		t.Fatal("completed create stage was scheduled again", worked, err)
	}
}

type timeoutProvider struct {
	reconciled bool
}

func (p *timeoutProvider) Create(ctx context.Context, _ fabric.CreateCall) (fabric.Observation, error) {
	<-ctx.Done()
	return fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "ignored"}, ctx.Err()
}

func (p *timeoutProvider) ReconcileCreate(_ context.Context, call fabric.ReconcileCall) (fabric.Observation, error) {
	p.reconciled = true
	if call.Action.ID == "" || call.KnownResourceRef != "" {
		return fabric.Observation{}, errors.New("wrong reconciliation identity")
	}
	return fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "reconciled-resource"}, nil
}

func TestWorkerPersistsCallTimeoutThenOnlyReconciles(t *testing.T) {
	fixture := newServiceFixture(t)
	created := pendingCreation(t, fixture)
	provider := &timeoutProvider{}
	config := testWorkerConfig()
	config.CallTimeout = 40 * time.Millisecond
	worker, err := NewWorker(fixture.store, map[string]fabric.CreateProvider{"sandbox": provider}, config)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(fixture.ctx); err != nil || !worked {
		t.Fatal("timed out call did not produce a durable recovery point", worked, err)
	}
	action, err := fixture.store.ProviderAction(fixture.ctx, created.Operation.ID, "create")
	if err != nil || action.Outcome != "timed_out" || !action.CompletedAt.IsZero() {
		t.Fatal("provider deadline was not persisted independently", action, err)
	}
	if worked, err := worker.RunOnce(fixture.ctx); err != nil || worked {
		t.Fatal("pending action ignored its durable retry lease", worked, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if worked, err := worker.RunOnce(fixture.ctx); err != nil || !worked || !provider.reconciled {
		t.Fatal("timed out create was not reconciled", worked, provider.reconciled, err)
	}
	action, err = fixture.store.ProviderAction(fixture.ctx, created.Operation.ID, "create")
	if err != nil || action.Outcome != "succeeded" || action.CompletedAt.IsZero() {
		t.Fatal("reconciliation did not finish create", action, err)
	}
}

type heldProvider struct {
	called  chan struct{}
	release chan struct{}
}

func (p *heldProvider) Create(ctx context.Context, _ fabric.CreateCall) (fabric.Observation, error) {
	close(p.called)
	select {
	case <-p.release:
		return fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "held-resource"}, nil
	case <-ctx.Done():
		return fabric.Observation{}, ctx.Err()
	}
}
func (*heldProvider) ReconcileCreate(context.Context, fabric.ReconcileCall) (fabric.Observation, error) {
	return fabric.Observation{}, errors.New("unexpected reconciliation")
}

func TestWorkerRenewsLeaseWhileProviderCallIsActive(t *testing.T) {
	fixture := newServiceFixture(t)
	created := pendingCreation(t, fixture)
	provider := &heldProvider{called: make(chan struct{}), release: make(chan struct{})}
	config := testWorkerConfig()
	config.CallTimeout = 4 * time.Second
	worker, err := NewWorker(fixture.store, map[string]fabric.CreateProvider{"sandbox": provider}, config)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		worked, err := worker.RunOnce(fixture.ctx)
		if !worked && err == nil {
			err = errors.New("worker did not claim operation")
		}
		done <- err
	}()
	select {
	case <-provider.called:
	case <-time.After(time.Second):
		t.Fatal("provider was not called")
	}
	before, err := fixture.store.Operation(fixture.ctx, created.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	operation, err := fixture.store.Operation(fixture.ctx, created.Operation.ID)
	if err != nil || !operation.Until.After(before.Until) {
		t.Fatal("active provider call lease was not renewed", operation.Until, err)
	}
	if _, err := fixture.store.ClaimOperation(fixture.ctx, created.Operation.ID, wire.ID(), time.Second); !errors.Is(err, lifecycle.ErrBusy) {
		t.Fatal("another worker took an active provider call", err)
	}
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerLeavesUnsupportedFabricUnclaimed(t *testing.T) {
	fixture := newServiceFixture(t)
	created := pendingCreation(t, fixture)
	worker, err := NewWorker(fixture.store, nil, testWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(fixture.ctx); err != nil || worked {
		t.Fatal("unsupported Fabric was claimed", worked, err)
	}
	claimed, err := fixture.store.ClaimOperation(fixture.ctx, created.Operation.ID, wire.ID(), time.Second)
	if err != nil || claimed.Revision != 1 {
		t.Fatal("unsupported operation was changed", claimed, err)
	}
	if _, err := NewWorker(fixture.store, nil, WorkerConfig{}); err == nil {
		t.Fatal("invalid worker bounds were accepted")
	}
	if _, err := fixture.store.RecoverableManagedCreates(fixture.ctx, 0); !errors.Is(err, metadata.ErrInvalidArgument) {
		t.Fatal("invalid recovery batch accepted", err)
	}
}

func TestWorkerRunPollsForLaterWorkAndStops(t *testing.T) {
	fixture := newServiceFixture(t)
	provider := &createProvider{createResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "polled-resource"}}
	worker, err := NewWorker(fixture.store, map[string]fabric.CreateProvider{"sandbox": provider}, testWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(fixture.ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	created := pendingCreation(t, fixture)
	deadline := time.Now().Add(2 * time.Second)
	for {
		resource, err := fixture.store.ManagedResource(fixture.ctx, created.Runner.ID)
		if err == nil && resource.Ref == "polled-resource" {
			break
		}
		if err != nil && !errors.Is(err, metadata.ErrNotFound) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("persistent worker did not observe later work")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop with its owner context")
	}
}
