// Package authorization binds user or machine authentication to a fixed
// Gateway connection. It contains product access policy, not protocol logic.
package authorization

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/runner"
)

var ErrNotFound = errors.New("machine not found")

type Sessions interface {
	Authenticate(context.Context, string) (identity.User, error)
	AuthenticateCLI(context.Context, string) (identity.User, error)
	Namespace() string
}

type Service struct {
	checker   access.Checker
	ownerOnly bool
	ctx       context.Context
	sessions  Sessions
	bindings  Repository
	bootID    string
}

func NewLocal(ctx context.Context, sessions Sessions, bindings Repository) *Service {
	return New(ctx, sessions, bindings, nil)
}
func New(ctx context.Context, sessions Sessions, bindings Repository, checker access.Checker) *Service {
	ownerOnly := checker == nil
	if checker == nil {
		checker = access.Owner{}
	}
	return &Service{ctx: ctx, sessions: sessions, bindings: bindings, ownerOnly: ownerOnly, checker: &boundedChecker{Checker: checker, slots: make(chan struct{}, 64)}}
}

// ClientGrant permits only the initial authenticated connection. Close removes
// the short-lived credential; established connections retain the original user
// and target and continue checking their session and ownership through Valid.
type ClientGrant struct {
	token   string
	expires int64
	valid   func() bool
	release func()
}

func (g *ClientGrant) Token() string    { return g.token }
func (g *ClientGrant) ExpiresAt() int64 { return g.expires }
func (g *ClientGrant) Valid() bool      { return g.valid() }
func (g *ClientGrant) Close()           { g.release() }

func (l *Service) Client(ctx context.Context, session, target string) (*ClientGrant, error) {
	return l.client(ctx, session, target, l.sessions.Authenticate, nil)
}
func (l *Service) ClientCLI(ctx context.Context, session, target string) (*ClientGrant, error) {
	return l.client(ctx, session, target, l.sessions.AuthenticateCLI, nil)
}
func (l *Service) ClientRunner(ctx context.Context, session string, binding runner.Binding) (*ClientGrant, error) {
	return l.client(ctx, session, binding.MachineID, l.sessions.Authenticate, &binding)
}
func (l *Service) ClientRunnerCLI(ctx context.Context, session string, binding runner.Binding) (*ClientGrant, error) {
	return l.client(ctx, session, binding.MachineID, l.sessions.AuthenticateCLI, &binding)
}
func (l *Service) client(ctx context.Context, session, target string, authenticate func(context.Context, string) (identity.User, error), binding *runner.Binding) (*ClientGrant, error) {
	if err := l.ctx.Err(); err != nil {
		return nil, err
	}
	user, err := authenticate(ctx, session)
	if err != nil {
		return nil, err
	}
	resource, decision, err := l.Resource(ctx, user, target, true, "runner.connect")
	if err != nil {
		return nil, err
	}
	if resource.Runner.Binding == nil {
		return nil, ErrNotFound
	}
	if binding != nil && *binding != *resource.Runner.Binding {
		return nil, runner.ErrBindingChanged
	}
	expected := *resource.Runner.Binding
	ctx, cancel := context.WithDeadline(ctx, decision.ValidUntil)
	defer cancel()
	token := ticketPrefix + wire.ID() + wire.ID()
	var record ConnectionAccess
	expires := time.Now().Add(TicketLifetime).Unix()
	record, err = l.bindings.CreateRunnerAccess(ctx, credentialHash(token), credentialHash(session), user.ID, user.Namespace, expected, expires)
	if err != nil {
		return nil, err
	}
	if record.OwnerID != resource.OwnerID || record.Subject != user.Subject {
		_ = l.bindings.DeleteAccess(ctx, credentialHash(token))
		return nil, runner.ErrBindingChanged
	}
	return &ClientGrant{token: token, expires: record.ExpiresAt, valid: l.validAccess(record), release: func() {
		ctx, cancel := context.WithTimeout(l.ctx, 500*time.Millisecond)
		defer cancel()
		// Cleanup is best-effort; expiry bounds an unconsumed ticket if storage
		// is unavailable. Consumed tickets have already been deleted atomically.
		_ = l.bindings.DeleteAccess(ctx, credentialHash(token))
	}}, nil
}

func (l *Service) validAccess(record ConnectionAccess) func() bool {
	// Captured values cannot be replaced by a request payload or another login.
	return func() bool {
		if l.ctx.Err() != nil || record.Namespace != l.sessions.Namespace() {
			return false
		}
		ctx, cancel := context.WithTimeout(l.ctx, 500*time.Millisecond)
		defer cancel()
		valid, err := l.bindings.CheckAccess(ctx, record, time.Now().Unix())
		return err == nil && valid
	}
}

// Authorize is used only by the connection-authentication entry point. Browser
// sessions are never machine credentials and tickets never authenticate fabricd.
func (l *Service) Authorize(token string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
	if err := l.ctx.Err(); err != nil {
		return gateway.BindingContext{}, nil, err
	}
	ctx, cancel := context.WithTimeout(l.ctx, access.CheckTimeout+500*time.Millisecond)
	defer cancel()
	if strings.HasPrefix(token, ticketPrefix) {
		if len(token) != len(ticketPrefix)+64 {
			return gateway.BindingContext{}, nil, identity.ErrUnauthorized
		}
		record, err := l.bindings.ConsumeAccess(ctx, credentialHash(token), l.sessions.Namespace(), time.Now().Unix())
		if err != nil {
			return gateway.BindingContext{}, nil, err
		}
		if record.ExpiresAt <= time.Now().Unix() {
			return gateway.BindingContext{}, nil, identity.ErrUnauthorized
		}
		fixed := record.Scope()
		if _, err := access.Evaluate(ctx, l.checker, access.Request{Scope: fixed, RequestID: wire.ID(), Operation: "runner.connect", Suboperation: "attached"}); err != nil {
			return gateway.BindingContext{}, nil, err
		}
		policy := &access.Policy{Scope: fixed, Checker: l.checker}
		if l.bootID != "" {
			policy.Delegate = l.delegate(record)
		}
		return (access.Grant{Target: record.Target, Role: gateway.RoleSDK, Valid: l.validAccess(record), Policy: policy}).Bind()
	}
	target, err := l.bindings.MachineCredential(ctx, token)
	if err != nil {
		return gateway.BindingContext{}, nil, err
	}
	return (access.Grant{Target: target, Role: gateway.RoleDaemon, Online: func(ctx context.Context, binding api.Binding) error {
		if binding.Target != target {
			return identity.ErrUnauthorized
		}
		return l.bindings.ConfirmMachineOnline(ctx, binding)
	}, Valid: func() bool {
		if l.ctx.Err() != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(l.ctx, 500*time.Millisecond)
		defer cancel()
		current, err := l.bindings.MachineCredential(ctx, token)
		return err == nil && current == target
	}}).Bind()
}

type boundedChecker struct {
	Checker access.Checker
	slots   chan struct{}
}

func (c *boundedChecker) Check(ctx context.Context, r access.Request) (access.Decision, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return access.Decision{}, ctx.Err()
	}
	return c.Checker.Check(ctx, r)
}
