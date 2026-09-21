// Package authorization binds user or machine authentication to a fixed
// Gateway connection. It contains product access policy, not protocol logic.
package authorization

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
)

var ErrNotFound = errors.New("machine not found")

type Sessions interface {
	Authenticate(context.Context, string) (identity.Authentication, error)
	Namespace() string
}

type Service struct {
	checker   access.Checker
	observer  access.CheckObserver
	ownerOnly bool
	ctx       context.Context
	sessions  Sessions
	bindings  Repository
	bootID    string
	mu        sync.Mutex
	tickets   map[string]ConnectionAccess
	peerSeen  map[string]time.Time
}

func New(ctx context.Context, sessions Sessions, bindings Repository, checker access.Checker, observer access.CheckObserver) *Service {
	ownerOnly := checker == nil
	if checker == nil {
		checker = access.Owner{}
	}
	return &Service{ctx: ctx, sessions: sessions, bindings: bindings, ownerOnly: ownerOnly, checker: &boundedChecker{Checker: checker, slots: make(chan struct{}, 64)}, observer: observer, tickets: map[string]ConnectionAccess{}, peerSeen: map[string]time.Time{}}
}

func (l *Service) evaluate(ctx context.Context, request access.Request) (access.Decision, error) {
	started := time.Now()
	decision, err := access.Evaluate(ctx, l.checker, request)
	l.observer.Notify(request, decision, err, time.Since(started))
	return decision, err
}

// ClientGrant permits only the initial authenticated connection. Close removes
// the short-lived credential; established connections retain the original user
// and target and revalidate their session, Runner and policy for every action
// lease. Valid performs an uncached check for in-process callers.
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

func (l *Service) ClientRunner(ctx context.Context, session string, binding runner.Binding) (*ClientGrant, error) {
	if err := l.ctx.Err(); err != nil {
		return nil, err
	}
	authenticated, err := l.authenticate(ctx, session)
	if err != nil {
		return nil, err
	}
	user := authenticated.User
	resource, decision, err := l.Resource(ctx, user, binding.MachineID, true, "runner.connect")
	if err != nil {
		return nil, err
	}
	if resource.Runner.Binding == nil {
		return nil, ErrNotFound
	}
	if binding != *resource.Runner.Binding {
		return nil, runner.ErrBindingChanged
	}
	expected := *resource.Runner.Binding
	ctx, cancel := context.WithDeadline(ctx, decision.ValidUntil)
	defer cancel()
	token := ticketPrefix + wire.ID() + wire.ID()
	expires := earliest(time.Now().Add(TicketLifetime), authenticated.ExpiresAt, decision.ValidUntil)
	record := ConnectionAccess{Session: session, PrincipalID: user.ID, PrincipalKind: user.Kind, Namespace: user.Namespace, Subject: user.Subject, Target: expected.MachineID, RunnerID: expected.RunnerID, FabricID: expected.FabricID, BindingRevision: expected.Revision, OwnerID: resource.OwnerID, ExpiresAt: expires.Unix()}
	l.mu.Lock()
	now := time.Now().Unix()
	pending := 0
	for key, ticket := range l.tickets {
		if ticket.ExpiresAt <= now {
			delete(l.tickets, key)
		} else if ticket.Session == session {
			pending++
		}
	}
	if pending >= 64 || len(l.tickets) >= maxPendingTickets {
		l.mu.Unlock()
		return nil, identity.ErrLoginLimit
	}
	l.tickets[token] = record
	l.mu.Unlock()
	return &ClientGrant{token: token, expires: record.ExpiresAt, valid: l.validAccess(record), release: func() {
		l.mu.Lock()
		delete(l.tickets, token)
		l.mu.Unlock()
	}}, nil
}

func (l *Service) validAccess(record ConnectionAccess) func() bool {
	return func() bool {
		_, err := l.validateAccess(l.ctx, record)
		return err == nil
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
		l.mu.Lock()
		record, found := l.tickets[token]
		delete(l.tickets, token)
		l.mu.Unlock()
		if !found {
			return gateway.BindingContext{}, nil, identity.ErrUnauthorized
		}
		if record.ExpiresAt <= time.Now().Unix() {
			return gateway.BindingContext{}, nil, identity.ErrUnauthorized
		}
		if !l.validAccess(record)() {
			return gateway.BindingContext{}, nil, identity.ErrUnauthorized
		}
		fixed := record.Scope()
		if _, err := l.evaluate(ctx, access.Request{Scope: fixed, RequestID: wire.ID(), Operation: "runner.connect", Suboperation: "attached"}); err != nil {
			return gateway.BindingContext{}, nil, err
		}
		policy := l.streamPolicy(record)
		if l.bootID != "" {
			policy.Delegate = l.delegate(record)
		}
		return (access.Grant{Target: record.Target, Role: gateway.RoleSDK, Policy: policy}).Bind()
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
