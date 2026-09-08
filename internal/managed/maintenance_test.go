package managed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/renewal"
)

type inspectProvider struct {
	mu      sync.Mutex
	calls   []fabric.InspectCall
	result  fabric.Inspection
	err     error
	called  chan struct{}
	release chan struct{}
}

func (p *inspectProvider) Inspect(ctx context.Context, call fabric.InspectCall) (fabric.Inspection, error) {
	p.mu.Lock()
	p.calls = append(p.calls, call)
	called, release := p.called, p.release
	p.mu.Unlock()
	if called != nil {
		select {
		case <-called:
		default:
			close(called)
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return fabric.Inspection{}, ctx.Err()
		}
	}
	return p.result, p.err
}

func inspectionWorkerConfig() WorkerConfig {
	config := testWorkerConfig()
	config.RenewalPolicy = renewal.Personal{Config: renewal.DefaultConfig()}
	config.RenewalPolicyVersion = "personal-v1"
	return config
}

func readyResourceForInspection(t *testing.T, fixture serviceFixture) lifecycle.Operation {
	t.Helper()
	claimed := claimedBootstrap(t, fixture, time.Minute)
	provider := &bootstrapProvider{bootstrapResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "bootstrap-resource"}}
	executor, err := NewBootstrapExecutor(fixture.store, map[string]fabric.BootstrapProvider{"sandbox": provider}, bootstrapConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, claimed); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	call := provider.bootstrapCalls[0]
	provider.mu.Unlock()
	machine, _, err := fixture.store.Enroll(fixture.ctx, call.EnrollmentToken, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	binding := api.Binding{Version: api.Version, Target: machine.ID, Incarnation: wire.ID(), Generation: 1, Capabilities: []string{"runtime.list"}, Limits: map[string]int{"streams": 8}}
	if err := fixture.store.ConfirmMachineOnline(fixture.ctx, binding); err != nil {
		t.Fatal(err)
	}
	return claimed
}

func TestMaintenanceExecutorPersistsBoundProviderFacts(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := readyResourceForInspection(t, fixture)
	expires := time.Now().Add(5 * time.Minute).Truncate(time.Millisecond)
	provider := &inspectProvider{result: fabric.Inspection{Status: fabric.InspectionConfirmed, ResourceRef: "bootstrap-resource", ExpiresAt: expires}}
	providers := map[string]fabric.InspectProvider{"sandbox": provider}
	executor, err := NewMaintenanceExecutor(fixture.store, providers, renewal.Personal{Config: renewal.DefaultConfig()}, "personal-v1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	delete(providers, "sandbox")
	claim, err := fixture.store.ClaimManagedInspection(fixture.ctx, claimed.RunnerID, "personal-v1", wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, claim); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	calls := append([]fabric.InspectCall(nil), provider.calls...)
	provider.mu.Unlock()
	if len(calls) != 1 || calls[0].RunnerID != claimed.RunnerID || calls[0].FabricID != "sandbox" || calls[0].ResourceRef != "bootstrap-resource" || calls[0].BindingRevision != 1 {
		t.Fatal("inspection was not bound to the durable resource", calls)
	}
	schedule, err := fixture.store.ManagedRenewalSchedule(fixture.ctx, claimed.RunnerID)
	if err != nil || schedule.Facts != lifecycle.InspectionConfirmed || schedule.Reason != "RENEW" || schedule.RenewUntil.IsZero() {
		t.Fatal("confirmed inspection did not persist its decision", schedule, err)
	}
	resource, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
	if err != nil || !resource.ExpiresAt.Equal(expires) {
		t.Fatal("confirmed provider expiry was not persisted", resource, err)
	}
}

func TestMaintenanceExecutorTreatsProviderErrorsAsUnknown(t *testing.T) {
	for name, providerErr := range map[string]error{"unavailable": errors.New("provider unavailable"), "deadline": context.DeadlineExceeded} {
		t.Run(name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			claimed := readyResourceForInspection(t, fixture)
			before, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
			if err != nil {
				t.Fatal(err)
			}
			provider := &inspectProvider{
				result: fabric.Inspection{Status: fabric.InspectionConfirmed, ResourceRef: "wrong-resource", Gone: true},
				err:    providerErr,
			}
			executor, err := NewMaintenanceExecutor(fixture.store, map[string]fabric.InspectProvider{"sandbox": provider}, renewal.Personal{Config: renewal.DefaultConfig()}, "personal-v1", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := fixture.store.ClaimManagedInspection(fixture.ctx, claimed.RunnerID, "personal-v1", wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := executor.Execute(fixture.ctx, fixture.ctx, claim); err != nil {
				t.Fatal(err)
			}
			schedule, err := fixture.store.ManagedRenewalSchedule(fixture.ctx, claimed.RunnerID)
			expected := lifecycle.InspectionUnknown
			if errors.Is(providerErr, context.DeadlineExceeded) {
				expected = lifecycle.InspectionTimedOut
			}
			if err != nil || schedule.Facts != expected || schedule.Reason != "FACTS_UNKNOWN" || schedule.NextCheckAt.IsZero() {
				t.Fatal("provider error was not durably reduced to an unknown fact", schedule, err)
			}
			after, err := fixture.store.ManagedResource(fixture.ctx, claimed.RunnerID)
			if err != nil || after != before {
				t.Fatal("fields returned with a provider error changed the resource", before, after, err)
			}
		})
	}
}

func TestMaintenanceExecutorRejectsInvalidConfigurationAndFacts(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := readyResourceForInspection(t, fixture)
	config := renewal.DefaultConfig()
	policy := renewal.Personal{Config: config}
	provider := &inspectProvider{result: fabric.Inspection{Status: fabric.InspectionConfirmed, ResourceRef: "bootstrap-resource"}}
	if _, err := NewMaintenanceExecutor(nil, nil, policy, "personal-v1", time.Second); err == nil {
		t.Fatal("maintenance executor accepted no shared metadata")
	}
	if _, err := NewMaintenanceExecutor(fixture.store, map[string]fabric.InspectProvider{" attached": provider}, policy, "personal-v1", time.Second); err == nil {
		t.Fatal("maintenance executor accepted an invalid provider namespace")
	}
	if _, err := NewMaintenanceExecutor(fixture.store, map[string]fabric.InspectProvider{"sandbox": provider}, nil, "personal-v1", time.Second); err == nil {
		t.Fatal("maintenance executor accepted no renewal policy")
	}
	if _, err := NewMaintenanceExecutor(fixture.store, map[string]fabric.InspectProvider{"sandbox": provider}, policy, "personal-v1", 0); err == nil {
		t.Fatal("maintenance executor accepted an unbounded policy call")
	}
	executor, err := NewMaintenanceExecutor(fixture.store, map[string]fabric.InspectProvider{"sandbox": provider}, policy, "personal-v1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := fixture.store.ClaimManagedInspection(fixture.ctx, claimed.RunnerID, "personal-v1", wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, claim); !errors.Is(err, ErrProviderContract) {
		t.Fatal("invalid live facts were accepted", err)
	}
	if _, err := fixture.store.ManagedRenewalSchedule(fixture.ctx, claimed.RunnerID); err != nil {
		t.Fatal("invalid provider facts removed the durable claim", err)
	}
	missing, err := NewMaintenanceExecutor(fixture.store, nil, policy, "personal-v1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Execute(fixture.ctx, fixture.ctx, claim); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatal("missing provider was not reported", err)
	}
}

func TestMaintenanceExecutorUsesCustomPolicyOutsideTransaction(t *testing.T) {
	fixture := newServiceFixture(t)
	claimed := readyResourceForInspection(t, fixture)
	expires := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	provider := &inspectProvider{result: fabric.Inspection{Status: fabric.InspectionConfirmed, ResourceRef: "bootstrap-resource", ExpiresAt: expires}}
	var captured renewal.Input
	policyHadDeadline := false
	policy := renewal.PolicyFunc(func(ctx context.Context, input renewal.Input) (renewal.Decision, error) {
		captured = input
		_, policyHadDeadline = ctx.Deadline()
		if _, err := fixture.store.ManagedResource(fixture.ctx, input.RunnerID); err != nil {
			return renewal.Decision{}, err
		}
		return renewal.Decision{Renew: true, Until: input.Now.Add(2 * time.Hour), Reason: "ENTERPRISE_RENEW"}, nil
	})
	executor, err := NewMaintenanceExecutor(fixture.store, map[string]fabric.InspectProvider{"sandbox": provider}, policy, "enterprise-v7", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := fixture.store.ClaimManagedInspection(fixture.ctx, claimed.RunnerID, "enterprise-v7", wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(fixture.ctx, fixture.ctx, claim); err != nil {
		t.Fatal(err)
	}
	if !policyHadDeadline || captured.PrincipalID != claimed.PrincipalID || !captured.PrincipalEnabled || captured.Namespace != claimed.Namespace || captured.RunnerID != claimed.RunnerID || captured.FabricID != claimed.FabricID || captured.CreatedAt.IsZero() || !captured.State.EverReady || !captured.State.FactsConfirmed || !captured.State.ExpiresAt.Equal(expires) {
		t.Fatal("custom policy did not receive the authoritative bounded input", captured)
	}
	schedule, err := fixture.store.ManagedRenewalSchedule(fixture.ctx, claimed.RunnerID)
	if err != nil || schedule.PolicyVersion != "enterprise-v7" || schedule.Reason != "ENTERPRISE_RENEW" || !schedule.RenewUntil.Equal(captured.Now.Add(2*time.Hour)) || !schedule.NextCheckAt.IsZero() {
		t.Fatal("custom policy decision was not durably persisted", schedule, err)
	}
}

func TestMaintenanceExecutorPersistsPolicyFailures(t *testing.T) {
	tests := []struct {
		name    string
		policy  renewal.Policy
		reason  string
		timeout time.Duration
	}{
		{name: "error", policy: renewal.PolicyFunc(func(context.Context, renewal.Input) (renewal.Decision, error) {
			return renewal.Decision{}, errors.New("private policy failure")
		}), reason: "POLICY_ERROR"},
		{name: "invalid", policy: renewal.PolicyFunc(func(context.Context, renewal.Input) (renewal.Decision, error) {
			return renewal.Decision{Renew: true, Reason: "unsafe"}, nil
		}), reason: "POLICY_INVALID"},
		{name: "deadline", policy: renewal.PolicyFunc(func(ctx context.Context, _ renewal.Input) (renewal.Decision, error) {
			<-ctx.Done()
			return renewal.Decision{}, ctx.Err()
		}), reason: "POLICY_ERROR", timeout: 30 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			claimed := readyResourceForInspection(t, fixture)
			provider := &inspectProvider{result: fabric.Inspection{Status: fabric.InspectionConfirmed, ResourceRef: "bootstrap-resource", ExpiresAt: time.Now().Add(time.Hour)}}
			policyTimeout := test.timeout
			if policyTimeout == 0 {
				policyTimeout = time.Second
			}
			executor, err := NewMaintenanceExecutor(fixture.store, map[string]fabric.InspectProvider{"sandbox": provider}, test.policy, "enterprise-v8", policyTimeout)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := fixture.store.ClaimManagedInspection(fixture.ctx, claimed.RunnerID, "enterprise-v8", wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := executor.Execute(fixture.ctx, fixture.ctx, claim); err != nil {
				t.Fatal("policy failure escaped the durable scheduler", err)
			}
			schedule, err := fixture.store.ManagedRenewalSchedule(fixture.ctx, claimed.RunnerID)
			if err != nil || schedule.Reason != test.reason || schedule.NextCheckAt.IsZero() || !schedule.RenewUntil.IsZero() {
				t.Fatal("policy failure was not durably normalized", schedule, err)
			}
			if delta := schedule.NextCheckAt.Sub(schedule.ObservedAt); delta <= 0 || delta > renewalPolicyFailureRecheck {
				t.Fatal("policy failure did not get a bounded retry", delta)
			}
		})
	}
}

func TestWorkerRenewsInspectionLeaseAndPrioritizesMaintenance(t *testing.T) {
	fixture := newServiceFixture(t)
	ready := readyResourceForInspection(t, fixture)
	pending := pendingCreation(t, fixture)
	inspect := &inspectProvider{
		result:  fabric.Inspection{Status: fabric.InspectionConfirmed, ResourceRef: "bootstrap-resource", ExpiresAt: time.Now().Add(time.Hour)},
		called:  make(chan struct{}),
		release: make(chan struct{}),
	}
	create := &createProvider{createResult: fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "should-not-run"}}
	config := inspectionWorkerConfig()
	config.CallTimeout = 4 * time.Second
	worker, err := NewWorker(fixture.store, ProviderSet{
		Create:  map[string]fabric.CreateProvider{"sandbox": create},
		Inspect: map[string]fabric.InspectProvider{"sandbox": inspect},
	}, config)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		worked, runErr := worker.RunOnce(fixture.ctx)
		if !worked && runErr == nil {
			runErr = errors.New("worker did not claim inspection")
		}
		done <- runErr
	}()
	select {
	case <-inspect.called:
	case <-time.After(time.Second):
		t.Fatal("worker did not prioritize due inspection")
	}
	before, err := fixture.store.ManagedRenewalSchedule(fixture.ctx, ready.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	after, err := fixture.store.ManagedRenewalSchedule(fixture.ctx, ready.RunnerID)
	if err != nil || !after.Until.After(before.Until) {
		t.Fatal("active inspection lease was not renewed", before.Until, after.Until, err)
	}
	if _, err := fixture.store.ClaimManagedInspection(fixture.ctx, ready.RunnerID, "personal-v1", wire.ID(), time.Second); !errors.Is(err, lifecycle.ErrBusy) && !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Fatal("another worker took an active inspection", err)
	}
	create.mu.Lock()
	createCalls := len(create.createCalls)
	create.mu.Unlock()
	if createCalls != 0 {
		t.Fatal("new create starved resource maintenance", createCalls)
	}
	close(inspect.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if operation, err := fixture.store.Operation(fixture.ctx, pending.Operation.ID); err != nil || operation.Revision != 0 {
		t.Fatal("maintenance changed an unrelated create", operation, err)
	}
}

func TestWorkerPersistsInspectionTimeoutAndLeavesUnsupportedFabric(t *testing.T) {
	fixture := newServiceFixture(t)
	ready := readyResourceForInspection(t, fixture)
	timedOut := &inspectProvider{called: make(chan struct{}), release: make(chan struct{})}
	config := inspectionWorkerConfig()
	config.CallTimeout = 40 * time.Millisecond
	worker, err := NewWorker(fixture.store, ProviderSet{Inspect: map[string]fabric.InspectProvider{"sandbox": timedOut}}, config)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(fixture.ctx); err != nil || !worked {
		t.Fatal("timed out inspection did not persist a schedule", worked, err)
	}
	schedule, err := fixture.store.ManagedRenewalSchedule(fixture.ctx, ready.RunnerID)
	if err != nil || schedule.Facts != lifecycle.InspectionTimedOut || schedule.Reason != "FACTS_UNKNOWN" {
		t.Fatal("inspection timeout was not persisted", schedule, err)
	}

	second := newServiceFixture(t)
	unsupported := readyResourceForInspection(t, second)
	other := &inspectProvider{}
	worker, err = NewWorker(second.store, ProviderSet{Inspect: map[string]fabric.InspectProvider{"other-fabric": other}}, inspectionWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(second.ctx); err != nil || worked {
		t.Fatal("unsupported Fabric was claimed for inspection", worked, err)
	}
	claim, err := second.store.ClaimManagedInspection(second.ctx, unsupported.RunnerID, "personal-v1", wire.ID(), time.Second)
	if err != nil || claim.Revision != 1 {
		t.Fatal("unsupported inspection was changed", claim, err)
	}
	if _, err := NewWorker(second.store, ProviderSet{Inspect: map[string]fabric.InspectProvider{"sandbox": &inspectProvider{}}}, testWorkerConfig()); err == nil {
		t.Fatal("inspection worker accepted missing renewal configuration")
	}
}
