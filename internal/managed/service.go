// Package managed assembles template validation, access checks and durable
// lifecycle operations. Provider I/O belongs outside its database transactions.
package managed

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/fabric"
)

type Service struct {
	catalog  *fabric.Catalog
	sessions authorization.Sessions
	access   *authorization.Service
	store    *metadata.Store
}

// New uses the host's existing sessions, access service and shared SQL store.
// It starts no workers and does not make Managed available on a public endpoint.
func New(catalog *fabric.Catalog, sessions authorization.Sessions, checks *authorization.Service, store *metadata.Store) (*Service, error) {
	if catalog == nil || sessions == nil || checks == nil || store == nil {
		return nil, fmt.Errorf("managed service requires catalog, sessions, access checks and shared metadata")
	}
	return &Service{catalog: catalog, sessions: sessions, access: checks, store: store}, nil
}

func (s *Service) Templates(ctx context.Context, cookie string) ([]fabric.Template, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := []fabric.Template{}
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
		out = append(out, t)
	}
	return out, nil
}

func hideDenied(err error) error {
	if errors.Is(err, access.ErrDenied) && !errors.Is(err, access.ErrUnavailable) {
		return fabric.ErrTemplateNotFound
	}
	return err
}

func (s *Service) template(ctx context.Context, user identity.User, fabricID, id, version string) (fabric.Template, access.Decision, error) {
	t, err := s.catalog.Template(fabricID, id, version)
	if err != nil || t.Disabled {
		return fabric.Template{}, access.Decision{}, fabric.ErrTemplateNotFound
	}
	decision, err := s.access.TemplateDecision(ctx, user, t, "template.get")
	if err != nil {
		return fabric.Template{}, access.Decision{}, hideDenied(err)
	}
	return t, decision, nil
}

func (s *Service) Template(ctx context.Context, cookie, fabricID, id, version string) (fabric.Template, error) {
	user, err := s.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return fabric.Template{}, err
	}
	t, _, err := s.template(ctx, user, fabricID, id, version)
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
	template, view, err := s.template(ctx, user, request.FabricID, request.TemplateID, request.TemplateVersion)
	if err != nil {
		return lifecycle.Creation{}, err
	}
	ctx, cancel := context.WithDeadline(ctx, view.ValidUntil)
	defer cancel()
	allowed, err := s.access.TemplateDecision(ctx, user, template, "runner.create")
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
	return s.store.CreateManaged(ctx, user, hash, requestKey, frozen)
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
	return s.store.CreateManagedDestroy(ctx, user, hash, requestKey, selected, resource, closeTimeout)
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

func (s *Service) operationStatus(ctx context.Context, operation lifecycle.Operation) (lifecycle.ManagedStatus, error) {
	status := lifecycle.ManagedStatus{Operation: operation}
	if resource, err := s.store.ManagedResource(ctx, operation.RunnerID); err == nil {
		status.ResourceRef, status.ExpiresAt, status.AccessClosed = resource.Ref, resource.ExpiresAt, resource.AccessClosed
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
		if destroyed.AccessCloseOutcome == lifecycle.AccessCloseWaiting {
			status.Stage = "closing_access"
		}
		if action, err := s.store.ProviderAction(ctx, operation.ID, "destroy"); err == nil {
			status.ProviderOutcome = action.Outcome
		} else if !errors.Is(err, metadata.ErrNotFound) {
			return lifecycle.ManagedStatus{}, err
		}
	}
	if operation.Finished {
		status.Stage = operation.Outcome
	}
	return status, nil
}
