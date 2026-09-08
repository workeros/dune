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
