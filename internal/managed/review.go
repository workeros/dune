package managed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/fabric"
)

// ReviewExecutor performs read-only provider checks requested by an authorized
// user. It never calls Create, Bootstrap, Renew or Destroy.
type ReviewExecutor struct {
	store      *metadata.Store
	create     map[string]fabric.CreateProvider
	bootstrap  map[string]fabric.BootstrapProvider
	renew      map[string]fabric.RenewProvider
	destroy    map[string]fabric.DestroyProvider
	candidates map[string]fabric.CandidateProvider
}

func NewReviewExecutor(store *metadata.Store, providers ProviderSet) (*ReviewExecutor, error) {
	if store == nil {
		return nil, fmt.Errorf("managed review executor requires shared metadata")
	}
	result := &ReviewExecutor{
		store: store, create: cloneProviderMap(providers.Create), bootstrap: cloneProviderMap(providers.Bootstrap),
		renew: cloneProviderMap(providers.Renew), destroy: cloneProviderMap(providers.Destroy), candidates: cloneProviderMap(providers.Candidate),
	}
	for id, provider := range providers.Candidate {
		if !validProviderName(id) || id == "attached" || provider == nil {
			return nil, fmt.Errorf("invalid managed candidate provider configuration")
		}
	}
	return result, nil
}

func cloneProviderMap[T any](source map[string]T) map[string]T {
	result := make(map[string]T, len(source))
	for id, provider := range source {
		result[id] = provider
	}
	return result
}

func (e *ReviewExecutor) configuredFabrics() []string {
	result := make([]string, 0, len(e.candidates))
	for id := range e.candidates {
		if e.create[id] == nil || e.bootstrap[id] == nil || e.renew[id] == nil || e.destroy[id] == nil {
			continue
		}
		result = append(result, id)
	}
	return result
}

func (e *ReviewExecutor) reconcile(ctx, providerCtx context.Context, operation lifecycle.Operation, action lifecycle.ProviderAction) (fabric.Observation, error) {
	switch action.Kind {
	case "create":
		provider, ok := e.create[operation.FabricID]
		if !ok {
			return fabric.Observation{}, ErrProviderUnavailable
		}
		known := ""
		resource, err := e.store.ManagedResource(ctx, operation.RunnerID)
		if err == nil {
			known = resource.Ref
		} else if !errors.Is(err, metadata.ErrNotFound) {
			return fabric.Observation{}, err
		}
		return provider.ReconcileCreate(providerCtx, fabric.ReconcileCall{Action: providerAction(operation, action), KnownResourceRef: known})
	case "bootstrap":
		provider, ok := e.bootstrap[operation.FabricID]
		if !ok {
			return fabric.Observation{}, ErrProviderUnavailable
		}
		return provider.ReconcileBootstrap(providerCtx, fabric.BootstrapReconcileCall{Action: providerAction(operation, action)})
	case "renew":
		provider, ok := e.renew[operation.FabricID]
		if !ok {
			return fabric.Observation{}, ErrProviderUnavailable
		}
		resource, err := e.store.ManagedResource(ctx, operation.RunnerID)
		if err != nil {
			return fabric.Observation{}, err
		}
		return provider.ReconcileRenew(providerCtx, fabric.RenewReconcileCall{Action: providerAction(operation, action), KnownExpiresAt: resource.ExpiresAt})
	case "destroy":
		provider, ok := e.destroy[operation.FabricID]
		if !ok {
			return fabric.Observation{}, ErrProviderUnavailable
		}
		return provider.ReconcileDestroy(providerCtx, fabric.DestroyReconcileCall{Action: providerAction(operation, action)})
	default:
		return fabric.Observation{}, ErrProviderContract
	}
}

func reviewObservation(providerCtx context.Context, action lifecycle.ProviderAction, observation fabric.Observation, err error) (lifecycle.ActionObservation, error) {
	switch action.Kind {
	case "renew":
		return renewalObservation(providerCtx, observation, action.RenewUntil, err)
	case "destroy":
		return destroyObservation(providerCtx, observation, err)
	case "create", "bootstrap":
		if err != nil && errors.Is(providerCtx.Err(), context.DeadlineExceeded) {
			err = context.DeadlineExceeded
		}
		return providerObservation(observation, err)
	default:
		return lifecycle.ActionObservation{}, ErrProviderContract
	}
}

func (e *ReviewExecutor) Execute(ctx, providerCtx context.Context, review lifecycle.Review, operation lifecycle.Operation) error {
	action, err := e.store.ProviderActionByID(ctx, review.OperationID, review.ActionID)
	if err != nil {
		return err
	}
	if action.OperationID != operation.ID || !action.CompletedAt.IsZero() {
		return lifecycle.ErrIntentConflict
	}
	var observed fabric.Observation
	if review.Mode == lifecycle.ReviewCandidate {
		provider, ok := e.candidates[operation.FabricID]
		if !ok {
			return ErrProviderUnavailable
		}
		observed, err = provider.VerifyCandidate(providerCtx, fabric.CandidateCall{Action: providerAction(operation, action), CandidateResourceRef: review.Candidate})
		if err == nil && observed.ResourceRef != "" && observed.ResourceRef != review.Candidate {
			return ErrProviderContract
		}
		if err == nil && observed.Outcome == fabric.OutcomeSucceeded && observed.ResourceRef != review.Candidate {
			return ErrProviderContract
		}
	} else if review.Mode == lifecycle.ReviewReconcile {
		observed, err = e.reconcile(ctx, providerCtx, operation, action)
	} else {
		return ErrProviderContract
	}
	result, resultErr := reviewObservation(providerCtx, action, observed, err)
	if resultErr != nil {
		return resultErr
	}
	if err := e.store.RecordProviderAction(ctx, operation, action.ID, result); err != nil {
		return err
	}
	if err := e.store.CompleteManagedReview(ctx, review); err != nil {
		return err
	}
	if result.Outcome == "succeeded" || result.Outcome == "failed" {
		return e.store.YieldOperationLease(ctx, operation)
	}
	return nil
}

func (e *ReviewExecutor) ExecuteWithLease(ctx context.Context, review lifecycle.Review, operation lifecycle.Operation, leaseTTL, callTimeout time.Duration) error {
	providerCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- e.Execute(ctx, providerCtx, review, operation) }()
	ticker := time.NewTicker(leaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case err := <-completed:
			return err
		case <-ticker.C:
			nextReview, nextOperation, err := e.store.RenewManagedReviewLease(ctx, review, operation, leaseTTL)
			if err != nil {
				cancel()
				executionErr := <-completed
				if executionErr == nil {
					return nil
				}
				return errors.Join(err, executionErr)
			}
			review, operation = nextReview, nextOperation
		case <-ctx.Done():
			cancel()
			<-completed
			return ctx.Err()
		}
	}
}
