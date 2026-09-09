package host

import (
	"context"
	"fmt"

	"github.com/aiomni/dune/pkg/fabric"
)

// resolvedManagedProvider preserves the existing worker shape while making
// every durable call resolve the exact binding frozen on the Operation.
type resolvedManagedProvider struct {
	resolver fabric.ProviderResolver
	fabricID string
}

func (p resolvedManagedProvider) exact(ctx context.Context, id string, revision int64) (fabric.ProviderBinding, error) {
	binding, err := p.resolver.Exact(ctx, id, revision)
	if err != nil {
		return fabric.ProviderBinding{}, err
	}
	if !binding.Valid() || binding.FabricID != p.fabricID || binding.ID != id || binding.Revision != revision {
		return fabric.ProviderBinding{}, fmt.Errorf("managed provider binding does not match durable reference")
	}
	return binding, nil
}

func (p resolvedManagedProvider) Availability(context.Context) (fabric.Availability, error) {
	return fabric.Availability{}, fabric.ErrProviderUnavailable
}

func (p resolvedManagedProvider) Create(ctx context.Context, call fabric.CreateCall) (fabric.Observation, error) {
	binding, err := p.exact(ctx, call.Action.ProviderBindingID, call.Action.ProviderBindingRevision)
	if err != nil {
		return fabric.Observation{}, err
	}
	return binding.Create.Create(ctx, call)
}

func (p resolvedManagedProvider) ReconcileCreate(ctx context.Context, call fabric.ReconcileCall) (fabric.Observation, error) {
	binding, err := p.exact(ctx, call.Action.ProviderBindingID, call.Action.ProviderBindingRevision)
	if err != nil {
		return fabric.Observation{}, err
	}
	return binding.Create.ReconcileCreate(ctx, call)
}

func (p resolvedManagedProvider) Bootstrap(ctx context.Context, call fabric.BootstrapCall) (fabric.Observation, error) {
	binding, err := p.exact(ctx, call.Action.ProviderBindingID, call.Action.ProviderBindingRevision)
	if err != nil {
		return fabric.Observation{}, err
	}
	return binding.Bootstrap.Bootstrap(ctx, call)
}

func (p resolvedManagedProvider) ReconcileBootstrap(ctx context.Context, call fabric.BootstrapReconcileCall) (fabric.Observation, error) {
	binding, err := p.exact(ctx, call.Action.ProviderBindingID, call.Action.ProviderBindingRevision)
	if err != nil {
		return fabric.Observation{}, err
	}
	return binding.Bootstrap.ReconcileBootstrap(ctx, call)
}

func (p resolvedManagedProvider) Inspect(ctx context.Context, call fabric.InspectCall) (fabric.Inspection, error) {
	binding, err := p.exact(ctx, call.ProviderBindingID, call.ProviderBindingRevision)
	if err != nil {
		return fabric.Inspection{}, err
	}
	return binding.Inspect.Inspect(ctx, call)
}

func (p resolvedManagedProvider) Renew(ctx context.Context, call fabric.RenewCall) (fabric.Observation, error) {
	binding, err := p.exact(ctx, call.Action.ProviderBindingID, call.Action.ProviderBindingRevision)
	if err != nil {
		return fabric.Observation{}, err
	}
	return binding.Renew.Renew(ctx, call)
}

func (p resolvedManagedProvider) ReconcileRenew(ctx context.Context, call fabric.RenewReconcileCall) (fabric.Observation, error) {
	binding, err := p.exact(ctx, call.Action.ProviderBindingID, call.Action.ProviderBindingRevision)
	if err != nil {
		return fabric.Observation{}, err
	}
	return binding.Renew.ReconcileRenew(ctx, call)
}

func (p resolvedManagedProvider) Destroy(ctx context.Context, call fabric.DestroyCall) (fabric.Observation, error) {
	binding, err := p.exact(ctx, call.Action.ProviderBindingID, call.Action.ProviderBindingRevision)
	if err != nil {
		return fabric.Observation{}, err
	}
	return binding.Destroy.Destroy(ctx, call)
}

func (p resolvedManagedProvider) ReconcileDestroy(ctx context.Context, call fabric.DestroyReconcileCall) (fabric.Observation, error) {
	binding, err := p.exact(ctx, call.Action.ProviderBindingID, call.Action.ProviderBindingRevision)
	if err != nil {
		return fabric.Observation{}, err
	}
	return binding.Destroy.ReconcileDestroy(ctx, call)
}

func (p resolvedManagedProvider) VerifyCandidate(ctx context.Context, call fabric.CandidateCall) (fabric.Observation, error) {
	binding, err := p.exact(ctx, call.Action.ProviderBindingID, call.Action.ProviderBindingRevision)
	if err != nil {
		return fabric.Observation{}, err
	}
	return binding.Candidate.VerifyCandidate(ctx, call)
}

func (p resolvedManagedProvider) Pause(ctx context.Context, call fabric.PauseResumeCall) error {
	binding, err := p.exact(ctx, call.ProviderBindingID, call.ProviderBindingRevision)
	if err != nil {
		return err
	}
	if binding.PauseResume == nil {
		return fabric.ErrProviderUnavailable
	}
	return binding.PauseResume.Pause(ctx, call)
}

func (p resolvedManagedProvider) Resume(ctx context.Context, call fabric.PauseResumeCall) error {
	binding, err := p.exact(ctx, call.ProviderBindingID, call.ProviderBindingRevision)
	if err != nil {
		return err
	}
	if binding.PauseResume == nil {
		return fabric.ErrProviderUnavailable
	}
	return binding.PauseResume.Resume(ctx, call)
}
