package managed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/fabric"
)

type DestroyExecutor struct {
	store     *metadata.Store
	providers map[string]fabric.DestroyProvider
}

func NewDestroyExecutor(store *metadata.Store, providers map[string]fabric.DestroyProvider) (*DestroyExecutor, error) {
	if store == nil {
		return nil, fmt.Errorf("managed destroy executor requires shared metadata")
	}
	copy := make(map[string]fabric.DestroyProvider, len(providers))
	for id, provider := range providers {
		if !validProviderName(id) || id == "attached" || provider == nil {
			return nil, fmt.Errorf("invalid managed destroy provider configuration")
		}
		copy[id] = provider
	}
	return &DestroyExecutor{store: store, providers: copy}, nil
}

func validDestroyObservation(observation fabric.Observation) bool {
	if observation.Outcome != fabric.OutcomeSucceeded && observation.Outcome != fabric.OutcomeFailed && observation.Outcome != fabric.OutcomeUnknown {
		return false
	}
	if len(observation.ResourceRef) > 1024 || !utf8.ValidString(observation.ResourceRef) || strings.ContainsFunc(observation.ResourceRef, unicode.IsControl) || !observation.ExpiresAt.IsZero() {
		return false
	}
	if observation.Outcome == fabric.OutcomeSucceeded {
		return observation.ResourceRef != "" && observation.Gone
	}
	return !observation.Gone
}

func destroyObservation(providerCtx context.Context, observation fabric.Observation, err error) (lifecycle.ActionObservation, error) {
	if err != nil {
		outcome := "unknown"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(providerCtx.Err(), context.DeadlineExceeded) {
			outcome = "timed_out"
		}
		return lifecycle.ActionObservation{Outcome: outcome}, nil
	}
	if !validDestroyObservation(observation) {
		return lifecycle.ActionObservation{}, ErrProviderContract
	}
	return lifecycle.ActionObservation{Outcome: string(observation.Outcome), ResourceRef: observation.ResourceRef, Gone: observation.Gone}, nil
}

// Execute advances one claimed deletion. Only the first confirmed action
// reservation may call Destroy; all later claimants reconcile that action.
func (e *DestroyExecutor) Execute(ctx, providerCtx context.Context, claimed lifecycle.Operation) error {
	destruction, err := e.store.ManagedDestroyOperation(ctx, claimed.ID)
	if err != nil {
		return err
	}
	if destruction.Operation.ID != claimed.ID || destruction.RunnerID != claimed.RunnerID || claimed.Action != "destroy" || destruction.AccessCloseOutcome == lifecycle.AccessCloseWaiting {
		return lifecycle.ErrIntentConflict
	}
	existing, existingErr := e.store.ProviderAction(ctx, claimed.ID, "destroy")
	if existingErr == nil && !existing.CompletedAt.IsZero() {
		return nil
	}
	if existingErr != nil && !errors.Is(existingErr, metadata.ErrNotFound) {
		return existingErr
	}
	provider, ok := e.providers[claimed.FabricID]
	if !ok {
		return ErrProviderUnavailable
	}
	action, dispatch, err := e.store.BeginProviderAction(ctx, claimed, lifecycle.ActionRequest{Kind: "destroy", Digest: claimed.Digest})
	if err != nil {
		return err
	}
	if !action.CompletedAt.IsZero() {
		return nil
	}
	var observed fabric.Observation
	if dispatch {
		if err := e.store.CheckProviderAction(ctx, claimed, action.ID); err != nil {
			return err
		}
		observed, err = provider.Destroy(providerCtx, fabric.DestroyCall{Action: providerAction(claimed, action)})
	} else {
		observed, err = provider.ReconcileDestroy(providerCtx, fabric.DestroyReconcileCall{Action: providerAction(claimed, action)})
	}
	result, resultErr := destroyObservation(providerCtx, observed, err)
	if resultErr != nil {
		return resultErr
	}
	return e.store.RecordProviderAction(ctx, claimed, action.ID, result)
}
