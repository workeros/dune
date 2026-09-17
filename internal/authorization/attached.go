package authorization

import (
	"context"

	"github.com/aiomni/dune/internal/identity"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/runner"
)

func (l *Service) IssueEnrollment(ctx context.Context, cookie, name string) (runner.Runner, string, int64, error) {
	authenticated, err := l.authenticate(ctx, cookie)
	if err != nil {
		return runner.Runner{}, "", 0, err
	}
	user := authenticated.User
	decision, err := l.Check(ctx, user, Resource{OwnerID: user.ID, Runner: runner.Runner{Kind: "attached"}}, "runner.create", "attached")
	if err != nil {
		return runner.Runner{}, "", 0, err
	}
	ctx, cancel := context.WithDeadline(ctx, earliest(decision.ValidUntil, authenticated.ExpiresAt))
	defer cancel()
	return l.bindings.IssueEnrollmentForSession(ctx, user, name, credentialHash(cookie))
}

func (l *Service) IssueTenantEnrollment(ctx context.Context, cookie, ownerID, name string) (runner.Runner, string, int64, error) {
	authenticated, err := l.authenticate(ctx, cookie)
	if err != nil {
		return runner.Runner{}, "", 0, err
	}
	user := authenticated.User
	decision, err := l.Check(ctx, user, Resource{OwnerID: ownerID, Runner: runner.Runner{Kind: "attached"}}, "runner.create", "attached")
	if err != nil {
		return runner.Runner{}, "", 0, err
	}
	ctx, cancel := context.WithDeadline(ctx, earliest(decision.ValidUntil, authenticated.ExpiresAt))
	defer cancel()
	return l.bindings.IssueTenantEnrollment(ctx, user, ownerID, name, credentialHash(cookie))
}
func (l *Service) EnrollmentDecision(ctx context.Context, token string) (access.Decision, error) {
	user, ownerID, kind, err := l.bindings.EnrollmentIdentity(ctx, token)
	if err != nil {
		return access.Decision{}, err
	}
	if user.Namespace != l.sessions.Namespace() {
		return access.Decision{}, identity.ErrUnauthorized
	}
	return l.Check(ctx, user, Resource{OwnerID: ownerID, Runner: runner.Runner{Kind: kind}}, "runner.create", kind)
}
func (l *Service) RevokeRunner(ctx context.Context, cookie string, binding runner.Binding) error {
	return l.revoke(ctx, cookie, "", binding)
}

// CancelEnrollment only cancels the current user's pending personal command.
// A concurrent successful enrollment keeps its exact binding and is never unbound.
func (l *Service) CancelEnrollment(ctx context.Context, cookie, runnerID string) error {
	authenticated, err := l.authenticate(ctx, cookie)
	if err != nil {
		return err
	}
	user := authenticated.User
	resource, err := l.bindings.RunnerResource(ctx, runnerID)
	if err != nil {
		return err
	}
	if resource.OwnerID != user.ID || resource.Runner.Kind != "attached" {
		return ErrNotFound
	}
	if resource.Runner.Binding != nil {
		return runner.ErrBindingChanged
	}
	decision, err := l.Check(ctx, user, resource, "runner.unbind", "attached")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, earliest(decision.ValidUntil, authenticated.ExpiresAt))
	defer cancel()
	return l.bindings.CancelEnrollmentForSession(ctx, user, runnerID, credentialHash(cookie))
}
func (l *Service) RevokeTenantRunner(ctx context.Context, cookie, ownerID string, binding runner.Binding) error {
	if ownerID == "" {
		return identity.ErrInvalidArgument
	}
	return l.revoke(ctx, cookie, ownerID, binding)
}
func (l *Service) revoke(ctx context.Context, cookie, ownerID string, expected runner.Binding) error {
	authenticated, err := l.authenticate(ctx, cookie)
	if err != nil {
		return err
	}
	user := authenticated.User
	resource, err := l.bindings.MachineResource(ctx, expected.MachineID)
	if err != nil {
		return err
	}
	if resource.Runner.Kind != "attached" || (ownerID != "" && resource.OwnerID != ownerID) {
		return ErrNotFound
	}
	if resource.Runner.Binding == nil || *resource.Runner.Binding != expected {
		return runner.ErrBindingChanged
	}
	decision, err := l.Check(ctx, user, resource, "runner.unbind", "attached")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, earliest(decision.ValidUntil, authenticated.ExpiresAt))
	defer cancel()
	return l.bindings.RevokeAuthorized(ctx, user, credentialHash(cookie), resource)
}
