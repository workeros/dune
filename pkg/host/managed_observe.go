package host

import (
	"context"
	"errors"
	"time"

	managedmodule "github.com/aiomni/dune/internal/managed"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/observe"
)

type managedProviderObserver struct {
	recorder *observationRecorder
}

type observedAvailabilityProvider struct {
	managedProviderObserver
	provider fabric.AvailabilityProvider
	fabricID string
}

func (p observedAvailabilityProvider) Availability(ctx context.Context) (fabric.Availability, error) {
	started := time.Now()
	result, err := p.provider.Availability(ctx)
	outcome := "invalid"
	if result.Valid() {
		outcome = "unavailable"
		if result.Available {
			outcome = "available"
		}
	}
	p.record(ctx, started, observe.Event{Name: observe.ManagedProviderCall, Operation: "availability", Suboperation: "read", FabricID: p.fabricID}, outcome, err)
	return result, err
}

func (o managedProviderObserver) actionEvent(action fabric.Action, operation, suboperation, resourceRef string) observe.Event {
	if resourceRef == "" {
		resourceRef = action.ResourceRef
	}
	return observe.Event{
		Name: observe.ManagedProviderCall, Operation: operation, Suboperation: suboperation,
		OperationID: action.OperationID, ActionID: action.ID, RunnerID: action.RunnerID,
		FabricID: action.FabricID, ResourceRef: resourceRef, Revision: action.ExecutionRevision,
	}
}

func (o managedProviderObserver) record(ctx context.Context, started time.Time, event observe.Event, outcome string, err error) {
	if err != nil {
		outcome = "unknown"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = "timed_out"
		}
	}
	if outcome == "" {
		outcome = "invalid"
	}
	event.Outcome = outcome
	event.DurationMicros = observationMicros(started)
	o.recorder.emit(event)
}

func providerOutcome(observation fabric.Observation) string {
	switch observation.Outcome {
	case fabric.OutcomeSucceeded, fabric.OutcomeFailed, fabric.OutcomeUnknown:
		return string(observation.Outcome)
	default:
		return ""
	}
}

func inspectionOutcome(inspection fabric.Inspection) string {
	switch inspection.Status {
	case fabric.InspectionConfirmed, fabric.InspectionUnknown:
		return string(inspection.Status)
	default:
		return ""
	}
}

type observedCreateProvider struct {
	managedProviderObserver
	provider fabric.CreateProvider
}

func (p observedCreateProvider) Create(ctx context.Context, call fabric.CreateCall) (fabric.Observation, error) {
	started := time.Now()
	result, err := p.provider.Create(ctx, call)
	p.record(ctx, started, p.actionEvent(call.Action, "create", "dispatch", ""), providerOutcome(result), err)
	return result, err
}

func (p observedCreateProvider) ReconcileCreate(ctx context.Context, call fabric.ReconcileCall) (fabric.Observation, error) {
	started := time.Now()
	result, err := p.provider.ReconcileCreate(ctx, call)
	p.record(ctx, started, p.actionEvent(call.Action, "create", "reconcile", call.KnownResourceRef), providerOutcome(result), err)
	return result, err
}

type observedBootstrapProvider struct {
	managedProviderObserver
	provider fabric.BootstrapProvider
}

func (p observedBootstrapProvider) Bootstrap(ctx context.Context, call fabric.BootstrapCall) (fabric.Observation, error) {
	started := time.Now()
	result, err := p.provider.Bootstrap(ctx, call)
	p.record(ctx, started, p.actionEvent(call.Action, "bootstrap", "dispatch", ""), providerOutcome(result), err)
	return result, err
}

func (p observedBootstrapProvider) ReconcileBootstrap(ctx context.Context, call fabric.BootstrapReconcileCall) (fabric.Observation, error) {
	started := time.Now()
	result, err := p.provider.ReconcileBootstrap(ctx, call)
	p.record(ctx, started, p.actionEvent(call.Action, "bootstrap", "reconcile", ""), providerOutcome(result), err)
	return result, err
}

type observedInspectProvider struct {
	managedProviderObserver
	provider fabric.InspectProvider
}

func (p observedInspectProvider) Inspect(ctx context.Context, call fabric.InspectCall) (fabric.Inspection, error) {
	started := time.Now()
	result, err := p.provider.Inspect(ctx, call)
	event := observe.Event{
		Name: observe.ManagedProviderCall, Operation: "inspect", Suboperation: "read",
		RunnerID: call.RunnerID, FabricID: call.FabricID, ResourceRef: call.ResourceRef, Revision: call.BindingRevision,
	}
	p.record(ctx, started, event, inspectionOutcome(result), err)
	return result, err
}

type observedRenewProvider struct {
	managedProviderObserver
	provider fabric.RenewProvider
}

func (p observedRenewProvider) Renew(ctx context.Context, call fabric.RenewCall) (fabric.Observation, error) {
	started := time.Now()
	result, err := p.provider.Renew(ctx, call)
	p.record(ctx, started, p.actionEvent(call.Action, "renew", "dispatch", ""), providerOutcome(result), err)
	return result, err
}

func (p observedRenewProvider) ReconcileRenew(ctx context.Context, call fabric.RenewReconcileCall) (fabric.Observation, error) {
	started := time.Now()
	result, err := p.provider.ReconcileRenew(ctx, call)
	p.record(ctx, started, p.actionEvent(call.Action, "renew", "reconcile", ""), providerOutcome(result), err)
	return result, err
}

type observedDestroyProvider struct {
	managedProviderObserver
	provider fabric.DestroyProvider
}

func (p observedDestroyProvider) Destroy(ctx context.Context, call fabric.DestroyCall) (fabric.Observation, error) {
	started := time.Now()
	result, err := p.provider.Destroy(ctx, call)
	p.record(ctx, started, p.actionEvent(call.Action, "destroy", "dispatch", ""), providerOutcome(result), err)
	return result, err
}

func (p observedDestroyProvider) ReconcileDestroy(ctx context.Context, call fabric.DestroyReconcileCall) (fabric.Observation, error) {
	started := time.Now()
	result, err := p.provider.ReconcileDestroy(ctx, call)
	p.record(ctx, started, p.actionEvent(call.Action, "destroy", "reconcile", ""), providerOutcome(result), err)
	return result, err
}

type observedCandidateProvider struct {
	managedProviderObserver
	provider fabric.CandidateProvider
}

func (p observedCandidateProvider) VerifyCandidate(ctx context.Context, call fabric.CandidateCall) (fabric.Observation, error) {
	started := time.Now()
	result, err := p.provider.VerifyCandidate(ctx, call)
	p.record(ctx, started, p.actionEvent(call.Action, "candidate", "verify", ""), providerOutcome(result), err)
	return result, err
}

func observeManagedProviders(availability map[string]fabric.AvailabilityProvider, providers managedmodule.ProviderSet, recorder *observationRecorder) (map[string]fabric.AvailabilityProvider, managedmodule.ProviderSet) {
	if recorder == nil || recorder.sink == nil {
		return availability, providers
	}
	observer := managedProviderObserver{recorder: recorder}
	observedAvailability := make(map[string]fabric.AvailabilityProvider, len(availability))
	result := managedmodule.ProviderSet{
		Create: make(map[string]fabric.CreateProvider, len(providers.Create)), Bootstrap: make(map[string]fabric.BootstrapProvider, len(providers.Bootstrap)),
		Inspect: make(map[string]fabric.InspectProvider, len(providers.Inspect)), Renew: make(map[string]fabric.RenewProvider, len(providers.Renew)),
		Destroy: make(map[string]fabric.DestroyProvider, len(providers.Destroy)), Candidate: make(map[string]fabric.CandidateProvider, len(providers.Candidate)),
	}
	for id, provider := range availability {
		observedAvailability[id] = observedAvailabilityProvider{managedProviderObserver: observer, provider: provider, fabricID: id}
	}
	for id, provider := range providers.Create {
		result.Create[id] = observedCreateProvider{managedProviderObserver: observer, provider: provider}
	}
	for id, provider := range providers.Bootstrap {
		result.Bootstrap[id] = observedBootstrapProvider{managedProviderObserver: observer, provider: provider}
	}
	for id, provider := range providers.Inspect {
		result.Inspect[id] = observedInspectProvider{managedProviderObserver: observer, provider: provider}
	}
	for id, provider := range providers.Renew {
		result.Renew[id] = observedRenewProvider{managedProviderObserver: observer, provider: provider}
	}
	for id, provider := range providers.Destroy {
		result.Destroy[id] = observedDestroyProvider{managedProviderObserver: observer, provider: provider}
	}
	for id, provider := range providers.Candidate {
		result.Candidate[id] = observedCandidateProvider{managedProviderObserver: observer, provider: provider}
	}
	return observedAvailability, result
}
