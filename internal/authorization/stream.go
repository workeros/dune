package authorization

import (
	"context"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/identity"
)

func earliest(first time.Time, rest ...time.Time) time.Time {
	for _, value := range rest {
		if value.Before(first) {
			first = value
		}
	}
	return first
}

func (l *Service) authenticate(ctx context.Context, token string) (identity.Authentication, error) {
	authenticated, err := l.sessions.Authenticate(ctx, token)
	if err != nil {
		return identity.Authentication{}, err
	}
	if err := ctx.Err(); err != nil {
		return identity.Authentication{}, err
	}
	if !authenticated.Valid() || authenticated.User.Namespace != l.sessions.Namespace() {
		return identity.Authentication{}, identity.ErrUnauthorized
	}
	return authenticated, nil
}

// validateAccess reads current identity and Runner facts. Its result is never
// cached here: only access.checkedStream owns the resulting action lease.
func (l *Service) validateAccess(ctx context.Context, record ConnectionAccess) (time.Time, error) {
	if err := l.ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if record.Namespace != l.sessions.Namespace() {
		return time.Time{}, identity.ErrUnauthorized
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if record.UpgradeVerification {
		return l.validateUpgradeVerification(ctx, record)
	}
	expires := time.Now().Add(access.StreamLeaseLimit)
	if record.Background {
		if record.Session != "" {
			return time.Time{}, identity.ErrUnauthorized
		}
	} else {
		authenticated, err := l.authenticate(ctx, record.Session)
		if err != nil {
			return time.Time{}, err
		}
		user := authenticated.User
		if user.ID != record.PrincipalID || user.Kind != record.PrincipalKind || user.Namespace != record.Namespace || user.Subject != record.Subject {
			return time.Time{}, identity.ErrUnauthorized
		}
		expires = authenticated.ExpiresAt
	}
	valid, err := l.bindings.CheckRunnerAccess(ctx, record)
	if err != nil {
		return time.Time{}, err
	}
	if !valid || !time.Now().Before(expires) {
		return time.Time{}, identity.ErrUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	return expires, nil
}

type streamChecker struct {
	service *Service
	record  ConnectionAccess
}

func (c streamChecker) Check(ctx context.Context, request access.Request) (access.Decision, error) {
	started := time.Now()
	if request.Scope != c.record.Scope() {
		return access.Decision{}, access.ErrDenied
	}
	expires, err := c.service.validateAccess(ctx, c.record)
	if err != nil {
		return access.Decision{}, err
	}
	if c.record.UpgradeVerification {
		if request.Operation != "runner.upgrade.probe" || request.Runtime.ID != "" {
			return access.Decision{}, access.ErrDenied
		}
		return access.Decision{Allowed: true, Reason: "RUNNER_UPGRADE_VERIFICATION", ID: request.RequestID, ValidUntil: earliest(started.Add(access.StreamLeaseLimit), expires)}, nil
	}
	decision, err := c.service.checker.Check(ctx, request)
	if err != nil {
		return access.Decision{}, err
	}
	decision.ValidUntil = earliest(started.Add(access.StreamLeaseLimit), expires, decision.ValidUntil)
	return decision, nil
}

func (l *Service) streamPolicy(record ConnectionAccess) *access.Policy {
	return &access.Policy{Scope: record.Scope(), Checker: streamChecker{service: l, record: record}, Observer: l.observer}
}
