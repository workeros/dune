// Package authorization binds local user or machine authentication to a fixed
// Gateway connection. It contains product access policy, not protocol logic.
package authorization

import (
	"context"
	"errors"
	"sync"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
)

var ErrNotFound = errors.New("machine not found")

type Sessions interface {
	Authenticate(context.Context, string) (identity.User, error)
}

type Bindings interface {
	Owns(string, string) bool
	MachineCredential(string) (string, bool)
}

type Local struct {
	ctx      context.Context
	sessions Sessions
	bindings Bindings
	mu       sync.Mutex
	tickets  map[string]access.Grant
}

func NewLocal(ctx context.Context, sessions Sessions, bindings Bindings) *Local {
	return &Local{ctx: ctx, sessions: sessions, bindings: bindings, tickets: make(map[string]access.Grant)}
}

// ClientGrant permits only the initial authenticated connection. Close removes
// the short-lived credential; established connections retain the original user
// and target and continue checking their session and ownership through Valid.
type ClientGrant struct {
	token   string
	valid   func() bool
	release func()
}

func (g *ClientGrant) Token() string { return g.token }
func (g *ClientGrant) Valid() bool   { return g.valid() }
func (g *ClientGrant) Close()        { g.release() }

func (l *Local) Client(ctx context.Context, session, target string) (*ClientGrant, error) {
	if err := l.ctx.Err(); err != nil {
		return nil, err
	}
	user, err := l.sessions.Authenticate(ctx, session)
	if err != nil {
		return nil, err
	}
	if !l.bindings.Owns(user.ID, target) {
		return nil, ErrNotFound
	}
	// Captured values cannot be replaced by a request payload or another login.
	valid := func() bool {
		if l.ctx.Err() != nil {
			return false
		}
		current, err := l.sessions.Authenticate(l.ctx, session)
		return err == nil && current.ID == user.ID && l.bindings.Owns(user.ID, target)
	}
	token := wire.ID() + wire.ID()
	l.mu.Lock()
	l.tickets[token] = access.Grant{Target: target, Role: gateway.RoleSDK, Valid: valid}
	l.mu.Unlock()
	return &ClientGrant{token: token, valid: valid, release: func() {
		l.mu.Lock()
		delete(l.tickets, token)
		l.mu.Unlock()
	}}, nil
}

// Authorize is used only by the connection-authentication entry point. Browser
// sessions are never machine credentials and tickets never authenticate fabricd.
func (l *Local) Authorize(token string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
	if err := l.ctx.Err(); err != nil {
		return gateway.BindingContext{}, nil, err
	}
	l.mu.Lock()
	grant, ok := l.tickets[token]
	if ok {
		delete(l.tickets, token)
	}
	l.mu.Unlock()
	if ok {
		return grant.Bind()
	}
	target, ok := l.bindings.MachineCredential(token)
	if !ok {
		return gateway.BindingContext{}, nil, identity.ErrUnauthorized
	}
	return (access.Grant{Target: target, Role: gateway.RoleDaemon, Valid: func() bool {
		if l.ctx.Err() != nil {
			return false
		}
		current, ok := l.bindings.MachineCredential(token)
		return ok && current == target
	}}).Bind()
}
