package managed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/fabric"
)

type RenewalExecutor struct {
	store     *metadata.Store
	providers map[string]fabric.RenewProvider
}

func NewRenewalExecutor(store *metadata.Store, providers map[string]fabric.RenewProvider) (*RenewalExecutor, error) {
	if store == nil {
		return nil, fmt.Errorf("managed renewal executor requires shared metadata")
	}
	copy := make(map[string]fabric.RenewProvider, len(providers))
	for id, provider := range providers {
		if !validProviderName(id) || id == "attached" || provider == nil {
			return nil, fmt.Errorf("invalid managed renewal provider configuration")
		}
		copy[id] = provider
	}
	return &RenewalExecutor{store: store, providers: copy}, nil
}

func validRenewalObservation(observation fabric.Observation, target time.Time) bool {
	if observation.Outcome != fabric.OutcomeSucceeded && observation.Outcome != fabric.OutcomeFailed && observation.Outcome != fabric.OutcomeUnknown {
		return false
	}
	if len(observation.ResourceRef) > 1024 || !utf8.ValidString(observation.ResourceRef) || strings.ContainsFunc(observation.ResourceRef, unicode.IsControl) || observation.Gone {
		return false
	}
	if !observation.ExpiresAt.IsZero() && (observation.ExpiresAt.UnixMilli() <= 0 || observation.ResourceRef == "") {
		return false
	}
	return observation.Outcome != fabric.OutcomeSucceeded || (observation.ResourceRef != "" && !observation.ExpiresAt.Before(target))
}

func renewalObservation(providerCtx context.Context, observation fabric.Observation, target time.Time, err error) (lifecycle.ActionObservation, error) {
	if err != nil {
		outcome := "unknown"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(providerCtx.Err(), context.DeadlineExceeded) {
			outcome = "timed_out"
		}
		return lifecycle.ActionObservation{Outcome: outcome}, nil
	}
	if !validRenewalObservation(observation, target) {
		return lifecycle.ActionObservation{}, ErrProviderContract
	}
	return lifecycle.ActionObservation{Outcome: string(observation.Outcome), ResourceRef: observation.ResourceRef, ExpiresAt: observation.ExpiresAt}, nil
}

// Execute advances one claimed automatic renewal. Only a newly committed
// action may call Renew; every later claimant reconciles the original key.
func (e *RenewalExecutor) Execute(ctx, providerCtx context.Context, claimed lifecycle.Operation) error {
	renewal, err := e.store.ManagedRenewal(ctx, claimed.ID)
	if err != nil {
		return err
	}
	if renewal.OperationID != claimed.ID || renewal.RunnerID != claimed.RunnerID || claimed.Action != "renew" {
		return lifecycle.ErrIntentConflict
	}
	existing, existingErr := e.store.ProviderAction(ctx, claimed.ID, "renew")
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
	action, dispatch, expired, err := e.store.BeginManagedRenewal(ctx, claimed)
	if err != nil || expired {
		return err
	}
	if !action.CompletedAt.IsZero() {
		return nil
	}
	resource, err := e.store.ManagedResource(ctx, claimed.RunnerID)
	if err != nil {
		return err
	}
	var observed fabric.Observation
	if dispatch {
		if err := e.store.CheckProviderAction(ctx, claimed, action.ID); err != nil {
			return err
		}
		observed, err = provider.Renew(providerCtx, fabric.RenewCall{Action: providerAction(claimed, action)})
	} else {
		observed, err = provider.ReconcileRenew(providerCtx, fabric.RenewReconcileCall{Action: providerAction(claimed, action), KnownExpiresAt: resource.ExpiresAt})
	}
	result, resultErr := renewalObservation(providerCtx, observed, renewal.Until, err)
	if resultErr != nil {
		return resultErr
	}
	return e.store.RecordProviderAction(ctx, claimed, action.ID, result)
}
