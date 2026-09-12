package authorization

import (
	"context"

	"github.com/aiomni/dune/internal/identity"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/runner"
)

func (l *Service) IssueEnrollment(ctx context.Context, cookie, name string) (string, int64, error) {
	user, err := l.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return "", 0, err
	}
	decision, err := l.Check(ctx, user, Resource{OwnerID: user.ID, Runner: runner.Runner{Kind: "attached"}}, "runner.create", "attached")
	if err != nil {
		return "", 0, err
	}
	ctx, cancel := context.WithDeadline(ctx, decision.ValidUntil)
	defer cancel()
	return l.bindings.IssueEnrollmentForSession(ctx, user, name, credentialHash(cookie))
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
func (l *Service) Revoke(ctx context.Context, cookie, target string) error {
	return l.revoke(ctx, cookie, target, nil)
}
func (l *Service) RevokeRunner(ctx context.Context, cookie string, binding runner.Binding) error {
	return l.revoke(ctx, cookie, binding.MachineID, &binding)
}
func (l *Service) revoke(ctx context.Context, cookie, target string, expected *runner.Binding) error {
	user, err := l.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return err
	}
	resource, decision, err := l.Resource(ctx, user, target, true, "runner.unbind")
	if err != nil {
		return err
	}
	if expected != nil && (resource.Runner.Binding == nil || *resource.Runner.Binding != *expected) {
		return runner.ErrBindingChanged
	}
	ctx, cancel := context.WithDeadline(ctx, decision.ValidUntil)
	defer cancel()
	return l.bindings.RevokeAuthorized(ctx, user, credentialHash(cookie), resource)
}
