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

var ErrProviderUnavailable = errors.New("managed provider unavailable")
var ErrProviderContract = errors.New("managed provider returned invalid facts")

// Executor performs provider I/O outside SQL transactions. The provider map is
// copied at construction so configuration cannot be changed underneath an
// action. It currently executes only the create stage; later stages compose
// their own narrow provider capabilities around the same journal.
type Executor struct {
	store     *metadata.Store
	providers map[string]fabric.CreateProvider
}

func NewExecutor(store *metadata.Store, providers map[string]fabric.CreateProvider) (*Executor, error) {
	if store == nil {
		return nil, fmt.Errorf("managed executor requires shared metadata")
	}
	copy := make(map[string]fabric.CreateProvider, len(providers))
	for id, provider := range providers {
		if id == "" || id == "attached" || len(id) > 128 || !utf8.ValidString(id) || strings.TrimSpace(id) != id || strings.ContainsFunc(id, unicode.IsControl) || provider == nil {
			return nil, fmt.Errorf("invalid managed provider configuration")
		}
		copy[id] = provider
	}
	return &Executor{store: store, providers: copy}, nil
}

func providerAction(op lifecycle.Operation, action lifecycle.ProviderAction) fabric.Action {
	return fabric.Action{
		ID: action.ID, OperationID: action.OperationID, RunnerID: op.RunnerID,
		FabricID: op.FabricID, RequestDigest: action.Digest,
		ResourceRef: action.ResourceRef, Issuer: action.Worker,
		BindingRevision: op.BindingRevision, ExecutionRevision: action.ExecutionRevision,
	}
}

func validCreateObservation(observation fabric.Observation) bool {
	if observation.Outcome != fabric.OutcomeSucceeded && observation.Outcome != fabric.OutcomeFailed && observation.Outcome != fabric.OutcomeUnknown {
		return false
	}
	if len(observation.ResourceRef) > 1024 || !utf8.ValidString(observation.ResourceRef) || strings.ContainsFunc(observation.ResourceRef, unicode.IsControl) {
		return false
	}
	if (!observation.ExpiresAt.IsZero() && (observation.ExpiresAt.UnixMilli() <= 0 || observation.ResourceRef == "")) || observation.Gone {
		return false
	}
	return observation.Outcome != fabric.OutcomeSucceeded || observation.ResourceRef != ""
}

func providerObservation(observation fabric.Observation, err error) (lifecycle.ActionObservation, error) {
	if err != nil {
		outcome := "unknown"
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = "timed_out"
		}
		return lifecycle.ActionObservation{Outcome: outcome}, nil
	}
	if !validCreateObservation(observation) {
		return lifecycle.ActionObservation{}, ErrProviderContract
	}
	return lifecycle.ActionObservation{
		Outcome: string(observation.Outcome), ResourceRef: observation.ResourceRef,
		ExpiresAt: observation.ExpiresAt, Gone: observation.Gone,
	}, nil
}

// ExecuteCreate advances one already-claimed creation operation. A confirmed
// first reservation may call Create exactly once. Every later invocation only
// reconciles the original action, including after timeout, process restart,
// lease takeover or a lost database acknowledgement.
// The caller supplies a bounded context for provider I/O. The database lease is
// checked immediately before dispatch and again on result commit; its absolute
// database-clock time is deliberately not compared to the worker's local clock.
func (e *Executor) ExecuteCreate(ctx context.Context, claimed lifecycle.Operation) error {
	creation, err := e.store.ManagedCreation(ctx, claimed.PrincipalID, claimed.RequestKey)
	if err != nil {
		return err
	}
	if creation.Operation.ID != claimed.ID || creation.Operation.Intent != claimed.Intent {
		return lifecycle.ErrIntentConflict
	}
	existing, existingErr := e.store.ProviderAction(ctx, claimed.ID, "create")
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
	action, dispatch, err := e.store.BeginProviderAction(ctx, claimed, lifecycle.ActionRequest{Kind: "create", Digest: claimed.Digest})
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
		observed, err = provider.Create(ctx, fabric.CreateCall{Action: providerAction(claimed, action), Request: creation.Spec})
	} else {
		knownResourceRef := ""
		resource, resourceErr := e.store.ManagedResource(ctx, claimed.RunnerID)
		if resourceErr == nil {
			if resource.FabricID != claimed.FabricID || resource.Gone {
				return lifecycle.ErrIntentConflict
			}
			knownResourceRef = resource.Ref
		} else if !errors.Is(resourceErr, metadata.ErrNotFound) {
			return resourceErr
		}
		observed, err = provider.ReconcileCreate(ctx, fabric.ReconcileCall{Action: providerAction(claimed, action), KnownResourceRef: knownResourceRef})
	}
	result, resultErr := providerObservation(observed, err)
	if resultErr != nil {
		return resultErr
	}
	return e.store.RecordProviderAction(ctx, claimed, action.ID, result)
}
