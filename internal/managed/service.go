// Package managed assembles template validation, access checks and durable
// lifecycle operations. Provider I/O belongs outside its database transactions.
package managed

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/fabric"
)

type Service struct {
	catalog      *fabric.Catalog
	availability map[string]fabric.AvailabilityProvider
	sessions     authorization.Sessions
	access       *authorization.Service
	store        *metadata.Store
	instanceID   string
	resolver     fabric.ProviderResolver
}

const providerAvailabilityTimeout = time.Second

// New uses the host's existing sessions, access service and shared SQL store.
// It starts no workers and does not make Managed available on a public endpoint.
func New(catalog *fabric.Catalog, availability map[string]fabric.AvailabilityProvider, sessions authorization.Sessions, checks *authorization.Service, store *metadata.Store, instanceID string, resolvers ...fabric.ProviderResolver) (*Service, error) {
	if catalog == nil || len(availability) == 0 || sessions == nil || checks == nil || store == nil || !wire.ValidID(instanceID) {
		return nil, fmt.Errorf("managed service requires catalog, sessions, access checks and shared metadata")
	}
	if len(resolvers) > 1 {
		return nil, fmt.Errorf("managed service accepts one provider resolver")
	}
	providers := make(map[string]fabric.AvailabilityProvider, len(availability))
	for id, provider := range availability {
		if provider == nil {
			return nil, fmt.Errorf("managed service requires provider availability checks")
		}
		providers[id] = provider
	}
	var resolver fabric.ProviderResolver
	if len(resolvers) == 1 {
		resolver = resolvers[0]
	}
	return &Service{catalog: catalog, availability: providers, sessions: sessions, access: checks, store: store, instanceID: instanceID, resolver: resolver}, nil
}

func (s *Service) providerAvailability(ctx context.Context, tenantID, fabricID string) (fabric.ProviderBindingRef, fabric.Availability) {
	ref := fabric.ProviderBindingRef{ID: fabricID, Revision: 1}
	provider, ok := s.availability[fabricID]
	if !ok {
		return fabric.ProviderBindingRef{}, fabric.Availability{Reason: fabric.AvailabilityConfiguration}
	}
	if s.resolver != nil {
		binding, err := s.resolver.Current(ctx, tenantID, fabricID)
		if err != nil || !binding.Valid() || binding.FabricID != fabricID {
			return fabric.ProviderBindingRef{}, fabric.Availability{Reason: fabric.AvailabilityConfiguration}
		}
		ref, provider = binding.ProviderBindingRef, binding.Availability
	}
	checkCtx, cancel := context.WithTimeout(ctx, providerAvailabilityTimeout)
	defer cancel()
	status, err := provider.Availability(checkCtx)
	if err != nil {
		return ref, fabric.Availability{Reason: fabric.AvailabilityUnreachable}
	}
	if !status.Valid() {
		return ref, fabric.Availability{Reason: fabric.AvailabilityUnknown}
	}
	return ref, status
}

func (s *Service) Templates(ctx context.Context, cookie string) ([]fabric.TemplateStatus, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := []fabric.TemplateStatus{}
	availability := make(map[string]fabric.Availability)
	for _, t := range s.catalog.Available() {
		_, err := s.access.TemplateDecision(ctx, user, t, "template.list")
		if errors.Is(err, access.ErrUnavailable) {
			return nil, err
		}
		if errors.Is(err, access.ErrDenied) {
			continue
		}
		if err != nil {
			return nil, err
		}
		status, ok := availability[t.FabricID]
		if !ok {
			_, status = s.providerAvailability(ctx, user.ID, t.FabricID)
			availability[t.FabricID] = status
		}
		out = append(out, fabric.TemplateStatus{Template: t, Availability: status})
	}
	return out, nil
}

func hideDenied(err error) error {
	if errors.Is(err, access.ErrDenied) && !errors.Is(err, access.ErrUnavailable) {
		return fabric.ErrTemplateNotFound
	}
	return err
}

func (s *Service) template(ctx context.Context, user identity.User, fabricID, id, version string) (fabric.TemplateStatus, access.Decision, fabric.ProviderBindingRef, error) {
	t, err := s.catalog.Template(fabricID, id, version)
	if err != nil || t.Disabled {
		return fabric.TemplateStatus{}, access.Decision{}, fabric.ProviderBindingRef{}, fabric.ErrTemplateNotFound
	}
	decision, err := s.access.TemplateDecision(ctx, user, t, "template.get")
	if err != nil {
		return fabric.TemplateStatus{}, access.Decision{}, fabric.ProviderBindingRef{}, hideDenied(err)
	}
	binding, availability := s.providerAvailability(ctx, user.ID, t.FabricID)
	return fabric.TemplateStatus{Template: t, Availability: availability}, decision, binding, nil
}

func (s *Service) Template(ctx context.Context, cookie, fabricID, id, version string) (fabric.TemplateStatus, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return fabric.TemplateStatus{}, err
	}
	t, _, _, err := s.template(ctx, user, fabricID, id, version)
	return t, err
}

// Create authenticates a browser session, checks the configured template and
// creation separately, validates the exact public request and commits its intent.
// The SQL transaction rechecks the original session. A successful result is
// durable acceptance only, not provider allocation or environment readiness.
func (s *Service) Create(ctx context.Context, cookie, requestKey string, request fabric.CreateRequest) (lifecycle.Creation, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return lifecycle.Creation{}, err
	}
	if previous, previousErr := s.store.ManagedCreation(ctx, user.ID, requestKey); previousErr == nil {
		requested, encodeErr := request.Encode()
		accepted, acceptedErr := previous.Spec.Encode()
		if encodeErr != nil || acceptedErr != nil || requested != accepted || previous.Operation.Namespace != user.Namespace || previous.Operation.Subject != user.Subject {
			return lifecycle.Creation{}, lifecycle.ErrIntentConflict
		}
		return previous, nil
	} else if !errors.Is(previousErr, metadata.ErrNotFound) {
		return lifecycle.Creation{}, previousErr
	}
	template, view, binding, err := s.template(ctx, user, request.FabricID, request.TemplateID, request.TemplateVersion)
	if err != nil {
		return lifecycle.Creation{}, err
	}
	if !template.Available {
		return lifecycle.Creation{}, fabric.ErrProviderUnavailable
	}
	ctx, cancel := context.WithDeadline(ctx, view.ValidUntil)
	defer cancel()
	allowed, err := s.access.TemplateDecision(ctx, user, template.Template, "runner.create")
	if err != nil {
		return lifecycle.Creation{}, err
	}
	ctx, stop := context.WithDeadline(ctx, allowed.ValidUntil)
	defer stop()
	frozen, err := s.catalog.ValidateRequest(request)
	if err != nil {
		return lifecycle.Creation{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(cookie)))
	return s.store.CreateManaged(ctx, user, hash, requestKey, frozen, binding)
}

// Destroy authenticates the current browser actor and checks the current
// Managed Runner before atomically closing its access and accepting cleanup.
// A successful result does not mean the provider resource has been deleted.
func (s *Service) Destroy(ctx context.Context, cookie, requestKey, runnerID string, closeTimeout time.Duration) (lifecycle.ManagedDestruction, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return lifecycle.ManagedDestruction{}, err
	}
	selected, decision, err := s.access.Resource(ctx, user, runnerID, false, "runner.destroy")
	if err != nil {
		return lifecycle.ManagedDestruction{}, err
	}
	if selected.Runner.Kind != "managed" {
		return lifecycle.ManagedDestruction{}, authorization.ErrNotFound
	}
	ctx, cancel := context.WithDeadline(ctx, decision.ValidUntil)
	defer cancel()
	resource, err := s.store.ManagedResource(ctx, runnerID)
	if err != nil {
		return lifecycle.ManagedDestruction{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(cookie)))
	return s.store.CreateManagedDestroy(ctx, user, hash, requestKey, selected, resource, s.instanceID, closeTimeout)
}

func (s *Service) Pause(ctx context.Context, cookie, requestKey, runnerID string) (lifecycle.ManagedPauseResume, error) {
	return s.pauseResume(ctx, cookie, requestKey, runnerID, "pause")
}

func (s *Service) Resume(ctx context.Context, cookie, requestKey, runnerID string) (lifecycle.ManagedPauseResume, error) {
	return s.pauseResume(ctx, cookie, requestKey, runnerID, "resume")
}

func (s *Service) pauseResume(ctx context.Context, cookie, requestKey, runnerID, action string) (lifecycle.ManagedPauseResume, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return lifecycle.ManagedPauseResume{}, err
	}
	selected, decision, err := s.access.Resource(ctx, user, runnerID, false, "runner."+action)
	if err != nil {
		return lifecycle.ManagedPauseResume{}, err
	}
	if selected.Runner.Kind != "managed" {
		return lifecycle.ManagedPauseResume{}, authorization.ErrNotFound
	}
	ctx, cancel := context.WithDeadline(ctx, decision.ValidUntil)
	defer cancel()
	resource, err := s.store.ManagedResource(ctx, runnerID)
	if err != nil {
		return lifecycle.ManagedPauseResume{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(cookie)))
	return s.store.CreateManagedPauseResume(ctx, user, hash, requestKey, selected, resource, s.instanceID, action)
}

// Status returns one authorized lifecycle snapshot for the current browser
// actor. It never exposes provider action keys or another owner's request.
func (s *Service) Status(ctx context.Context, cookie, operationID string) (lifecycle.ManagedStatus, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return lifecycle.ManagedStatus{}, err
	}
	operation, err := s.store.Operation(ctx, operationID)
	if err != nil {
		return lifecycle.ManagedStatus{}, err
	}
	if operation.PrincipalID != user.ID || operation.Namespace != user.Namespace || operation.Subject != user.Subject {
		return lifecycle.ManagedStatus{}, authorization.ErrNotFound
	}
	if _, _, err := s.access.Resource(ctx, user, operation.RunnerID, false, "runner.get"); err != nil {
		return lifecycle.ManagedStatus{}, err
	}
	return s.operationStatus(ctx, operation)
}

// RunnerStatus returns lifecycle facts for an authorized Managed Runner. The
// selected operation may have been initiated by another authorized actor, so
// user-facing views must not expose its principal or request key.
func (s *Service) RunnerStatus(ctx context.Context, cookie, runnerID string) (lifecycle.ManagedStatus, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return lifecycle.ManagedStatus{}, err
	}
	selected, _, err := s.access.Resource(ctx, user, runnerID, false, "runner.get")
	if err != nil {
		return lifecycle.ManagedStatus{}, err
	}
	if selected.Runner.Kind != "managed" {
		return lifecycle.ManagedStatus{}, authorization.ErrNotFound
	}
	operation, err := s.store.ManagedRunnerOperation(ctx, runnerID)
	if err != nil {
		return lifecycle.ManagedStatus{}, err
	}
	return s.operationStatus(ctx, operation)
}

// Review records a current user's request to re-check one unresolved provider
// action. Provider I/O is performed later by the durable worker. A candidate
// reference is only a claim for the adapter to verify, never trusted input.
func (s *Service) Review(ctx context.Context, cookie, requestKey string, request lifecycle.ReviewRequest) (lifecycle.Review, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return lifecycle.Review{}, err
	}
	operation, err := s.store.Operation(ctx, request.OperationID)
	if err != nil {
		return lifecycle.Review{}, err
	}
	selected, decision, err := s.access.Resource(ctx, user, operation.RunnerID, false, "runner.resolve")
	if err != nil {
		return lifecycle.Review{}, err
	}
	if selected.Runner.Kind != "managed" {
		return lifecycle.Review{}, authorization.ErrNotFound
	}
	ctx, cancel := context.WithDeadline(ctx, decision.ValidUntil)
	defer cancel()
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(cookie)))
	return s.store.CreateManagedReview(ctx, user, hash, requestKey, selected, request)
}

// ReviewStatus is actor-scoped because it contains the operator's reason and
// submitted candidate. Lifecycle status remains separately shareable through
// RunnerStatus after its normal access check.
func (s *Service) ReviewStatus(ctx context.Context, cookie, id string) (lifecycle.Review, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return lifecycle.Review{}, err
	}
	review, err := s.store.ManagedReview(ctx, user.ID, id)
	if err != nil {
		return lifecycle.Review{}, err
	}
	if review.Namespace != user.Namespace || review.Subject != user.Subject {
		return lifecycle.Review{}, authorization.ErrNotFound
	}
	return review, nil
}

func (s *Service) OperationReview(ctx context.Context, cookie, operationID string) (lifecycle.Review, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return lifecycle.Review{}, err
	}
	review, err := s.store.ManagedOperationReview(ctx, user.ID, operationID)
	if err != nil {
		return lifecycle.Review{}, err
	}
	if review.Namespace != user.Namespace || review.Subject != user.Subject {
		return lifecycle.Review{}, authorization.ErrNotFound
	}
	return review, nil
}

func (s *Service) operationStatus(ctx context.Context, operation lifecycle.Operation) (lifecycle.ManagedStatus, error) {
	status := lifecycle.ManagedStatus{Operation: operation}
	if resource, err := s.store.ManagedResource(ctx, operation.RunnerID); err == nil {
		status.ResourceRef, status.ExpiresAt, status.AccessClosed = resource.Ref, resource.ExpiresAt, resource.AccessClosed
		status.AccessSuspended, status.ResourceState, status.Capabilities = resource.AccessSuspended, resource.State, resource.Capabilities
	} else if !errors.Is(err, metadata.ErrNotFound) {
		return lifecycle.ManagedStatus{}, err
	}
	if schedule, err := s.store.ManagedRenewalSchedule(ctx, operation.RunnerID); err == nil {
		status.RenewalPolicyVersion, status.RenewalReason = schedule.PolicyVersion, schedule.Reason
		status.RenewalObservedAt, status.RenewalNextCheckAt, status.RenewalUntil = schedule.ObservedAt, schedule.NextCheckAt, schedule.RenewUntil
	} else if !errors.Is(err, metadata.ErrNotFound) {
		return lifecycle.ManagedStatus{}, err
	}
	switch operation.Action {
	case "create":
		status.Stage = "queued"
		created, err := s.store.ProviderAction(ctx, operation.ID, "create")
		if err == nil {
			status.Stage, status.ProviderOutcome = "creating", created.Outcome
			if !created.CompletedAt.IsZero() && created.Outcome == "succeeded" {
				status.Stage, status.ProviderOutcome = "bootstrapping", ""
				bootstrapped, bootstrapErr := s.store.ProviderAction(ctx, operation.ID, "bootstrap")
				if bootstrapErr == nil {
					status.ProviderOutcome = bootstrapped.Outcome
					if !bootstrapped.CompletedAt.IsZero() && bootstrapped.Outcome == "succeeded" {
						status.Stage, status.ProviderOutcome = "waiting_connection", ""
					}
				} else if !errors.Is(bootstrapErr, metadata.ErrNotFound) {
					return lifecycle.ManagedStatus{}, bootstrapErr
				}
			}
		} else if !errors.Is(err, metadata.ErrNotFound) {
			return lifecycle.ManagedStatus{}, err
		}
	case "renew":
		status.Stage = "renewing"
		if action, err := s.store.ProviderAction(ctx, operation.ID, "renew"); err == nil {
			status.ProviderOutcome = action.Outcome
		} else if !errors.Is(err, metadata.ErrNotFound) {
			return lifecycle.ManagedStatus{}, err
		}
	case "destroy":
		destroyed, err := s.store.ManagedDestroyOperation(ctx, operation.ID)
		if err != nil {
			return lifecycle.ManagedStatus{}, err
		}
		status.AccessCloseOutcome, status.AccessCloseDeadline = destroyed.AccessCloseOutcome, destroyed.CloseDeadline
		status.Stage = "destroying"
		if strings.HasPrefix(operation.RequestKey, "managed-cleanup-") {
			status.Stage = "cleaning_up"
		}
		if destroyed.AccessCloseOutcome == lifecycle.AccessCloseWaiting {
			status.Stage = "closing_access"
		}
		if action, err := s.store.ProviderAction(ctx, operation.ID, "destroy"); err == nil {
			status.ProviderOutcome = action.Outcome
		} else if !errors.Is(err, metadata.ErrNotFound) {
			return lifecycle.ManagedStatus{}, err
		}
	case "pause", "resume":
		status.Stage = "pausing"
		if operation.Action == "resume" {
			status.Stage = "resuming"
		}
		if action, err := s.store.ProviderAction(ctx, operation.ID, operation.Action); err == nil {
			status.ProviderOutcome = action.Outcome
			if operation.Action == "resume" && !action.CompletedAt.IsZero() && action.Outcome == "succeeded" && !operation.Finished {
				status.Stage, status.ProviderOutcome = "waiting_connection", ""
			}
		} else if !errors.Is(err, metadata.ErrNotFound) {
			return lifecycle.ManagedStatus{}, err
		}
	}
	if operation.Finished {
		status.Stage = operation.Outcome
	}
	return status, nil
}
