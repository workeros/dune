package host

import (
	"context"
	"errors"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
)

var (
	// ErrAttachedRunnerNotFound means no active Attached Runner exists in the
	// exact owner scope supplied by the host.
	ErrAttachedRunnerNotFound = errors.New("attached Runner not found")
	// ErrAttachedResultUnknown means cancellation reached the metadata commit
	// boundary but its outcome could not be observed. Callers must refresh facts
	// before deciding whether to retry.
	ErrAttachedResultUnknown = errors.New("attached operation result unknown; refresh facts before retrying")
)

// AttachedRunnerFacts is the bounded Dune-owned state a trusted host can use
// for product authorization and Tenant deletion checks. A nil Binding means an
// enrollment is still pending. Enrollment tokens and machine credentials are
// never exposed.
type AttachedRunnerFacts struct {
	OwnerID   string
	Runner    runner.Runner
	CreatedBy identity.User
}

// AttachedRunnerManager exposes trusted, in-process Attached lifecycle facts.
// The host must authorize the caller before CancelEnrollment; Dune owns the SQL
// transaction and guarantees that this operation never detaches a bound Runner.
type AttachedRunnerManager interface {
	Get(ctx context.Context, ownerID, runnerID string) (AttachedRunnerFacts, error)
	HasActive(ctx context.Context, ownerID string) (bool, error)
	CancelEnrollment(ctx context.Context, ownerID, runnerID string) error
}

type attachedRunnerManager struct{ app *App }

// AttachedRunnerManager returns the Attached capability owned by this App.
// Calls participate in App draining and are rejected after shutdown starts.
func (a *App) AttachedRunnerManager() AttachedRunnerManager {
	return &attachedRunnerManager{app: a}
}

func (m *attachedRunnerManager) Get(ctx context.Context, ownerID, runnerID string) (AttachedRunnerFacts, error) {
	if ownerID == "" || runnerID == "" {
		return AttachedRunnerFacts{}, identity.ErrInvalidArgument
	}
	ctx, finish, err := m.app.adminContext(ctx)
	if err != nil {
		return AttachedRunnerFacts{}, err
	}
	defer finish()
	facts, err := m.app.store.AttachedRunner(ctx, ownerID, runnerID)
	if errors.Is(err, metadata.ErrNotFound) {
		return AttachedRunnerFacts{}, ErrAttachedRunnerNotFound
	}
	return AttachedRunnerFacts{OwnerID: facts.OwnerID, Runner: facts.Runner, CreatedBy: facts.CreatedBy}, err
}

func (m *attachedRunnerManager) HasActive(ctx context.Context, ownerID string) (bool, error) {
	if ownerID == "" {
		return false, identity.ErrInvalidArgument
	}
	ctx, finish, err := m.app.adminContext(ctx)
	if err != nil {
		return false, err
	}
	defer finish()
	return m.app.store.HasActiveAttached(ctx, ownerID)
}

func (m *attachedRunnerManager) CancelEnrollment(ctx context.Context, ownerID, runnerID string) error {
	if ownerID == "" || runnerID == "" {
		return identity.ErrInvalidArgument
	}
	ctx, finish, err := m.app.adminContext(ctx)
	if err != nil {
		return err
	}
	defer finish()
	err = m.app.store.CancelAttachedEnrollment(ctx, ownerID, runnerID)
	if errors.Is(err, metadata.ErrNotFound) {
		return ErrAttachedRunnerNotFound
	}
	if errors.Is(err, metadata.ErrCommitUnknown) {
		return ErrAttachedResultUnknown
	}
	return err
}
