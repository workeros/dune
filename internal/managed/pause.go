package managed

import (
	"context"
	"errors"
	"fmt"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/fabric"
)

type PauseResumeExecutor struct {
	store     *metadata.Store
	providers map[string]fabric.PauseResumeProvider
	inspect   map[string]fabric.InspectProvider
}

func NewPauseResumeExecutor(store *metadata.Store, providers map[string]fabric.PauseResumeProvider, inspect map[string]fabric.InspectProvider) (*PauseResumeExecutor, error) {
	if store == nil {
		return nil, fmt.Errorf("managed pause/resume executor requires shared metadata")
	}
	result := &PauseResumeExecutor{store: store, providers: map[string]fabric.PauseResumeProvider{}, inspect: map[string]fabric.InspectProvider{}}
	for id, provider := range providers {
		inspection := inspect[id]
		if !validProviderName(id) || id == "attached" || provider == nil || inspection == nil {
			return nil, fmt.Errorf("invalid managed pause/resume provider configuration")
		}
		result.providers[id], result.inspect[id] = provider, inspection
	}
	return result, nil
}

func (e *PauseResumeExecutor) Execute(ctx, providerCtx context.Context, claimed lifecycle.Operation) error {
	if claimed.Action != "pause" && claimed.Action != "resume" {
		return lifecycle.ErrIntentConflict
	}
	resource, err := e.store.ManagedResource(ctx, claimed.RunnerID)
	if err != nil {
		return err
	}
	if resource.FabricID != claimed.FabricID || resource.ProviderBindingID != claimed.ProviderBindingID || resource.ProviderBindingRevision != claimed.ProviderBindingRevision || resource.Ref == "" || resource.Gone || resource.AccessClosed || !resource.AccessSuspended {
		return lifecycle.ErrIntentConflict
	}
	provider, ok := e.providers[claimed.FabricID]
	if !ok {
		return ErrProviderUnavailable
	}
	inspector := e.inspect[claimed.FabricID]
	action, dispatch, err := e.store.BeginProviderAction(ctx, claimed, lifecycle.ActionRequest{Kind: claimed.Action, Digest: claimed.Digest})
	if err != nil {
		return err
	}
	if !action.CompletedAt.IsZero() {
		return nil
	}
	call := fabric.PauseResumeCall{
		RunnerID: claimed.RunnerID, FabricID: claimed.FabricID, ResourceRef: resource.Ref, BindingRevision: claimed.BindingRevision,
		ProviderBindingID: claimed.ProviderBindingID, ProviderBindingRevision: claimed.ProviderBindingRevision,
	}
	if dispatch {
		if err := e.store.CheckProviderAction(ctx, claimed, action.ID); err != nil {
			return err
		}
		if claimed.Action == "pause" {
			err = provider.Pause(providerCtx, call)
		} else {
			err = provider.Resume(providerCtx, call)
		}
		if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(providerCtx.Err(), context.DeadlineExceeded)) {
			return e.store.RecordProviderAction(ctx, claimed, action.ID, lifecycle.ActionObservation{Outcome: "timed_out"})
		}
	}
	inspection, inspectErr := inspector.Inspect(providerCtx, fabric.InspectCall{
		RunnerID: claimed.RunnerID, FabricID: claimed.FabricID, ResourceRef: resource.Ref, BindingRevision: claimed.BindingRevision,
		ProviderBindingID: claimed.ProviderBindingID, ProviderBindingRevision: claimed.ProviderBindingRevision,
	})
	result, resultErr := inspectionResult(providerCtx, inspection, inspectErr)
	if resultErr != nil {
		return resultErr
	}
	observation := lifecycle.ActionObservation{Outcome: "unknown"}
	if result.Status == lifecycle.InspectionConfirmed {
		observation.ResourceRef, observation.ExpiresAt, observation.Gone = result.ResourceRef, result.ExpiresAt, result.Gone
		observation.State, observation.Capabilities = result.State, result.Capabilities
		if result.Gone {
			observation.Outcome = "failed"
		} else if (claimed.Action == "pause" && result.State == string(fabric.ResourcePaused)) || (claimed.Action == "resume" && result.State == string(fabric.ResourceReady)) {
			observation.Outcome = "succeeded"
		}
	}
	return e.store.RecordProviderAction(ctx, claimed, action.ID, observation)
}
