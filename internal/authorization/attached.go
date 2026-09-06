package authorization

import (
	"context"
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
	user, err := l.bindings.EnrollmentUser(ctx, token)
	if err != nil {
		return access.Decision{}, err
	}
	user.Namespace = l.sessions.Namespace()
	return l.Check(ctx, user, Resource{OwnerID: user.ID, Runner: runner.Runner{Kind: "attached"}}, "runner.create", "attached")
}
func (l *Service) Revoke(ctx context.Context, cookie, target string) error {
	user, err := l.sessions.Authenticate(ctx, cookie)
	if err != nil {
		return err
	}
	resource, decision, err := l.Resource(ctx, user, target, true, "runner.unbind")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, decision.ValidUntil)
	defer cancel()
	return l.bindings.RevokeAuthorized(ctx, user, credentialHash(cookie), resource)
}
