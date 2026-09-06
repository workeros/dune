// Package authorization binds local user or machine authentication to a fixed
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
	"github.com/aiomni/dune/pkg/gateway"
)

var ErrNotFound = errors.New("machine not found")

type Sessions interface {
	Authenticate(context.Context, string) (identity.User, error)
	Namespace() string
}

type Local struct {
	ctx      context.Context
	sessions Sessions
	bindings Repository
}

func NewLocal(ctx context.Context, sessions Sessions, bindings Repository) *Local {
	return &Local{ctx: ctx, sessions: sessions, bindings: bindings}
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
	token := ticketPrefix + wire.ID() + wire.ID()
	record, err := l.bindings.CreateAccess(ctx, credentialHash(token), credentialHash(session), user.ID, user.Namespace, target, time.Now().Add(TicketLifetime).Unix())
	if err != nil {
		return nil, err
	}
	return &ClientGrant{token: token, valid: l.validAccess(record), release: func() {
		ctx, cancel := context.WithTimeout(l.ctx, 500*time.Millisecond)
		defer cancel()
		// Cleanup is best-effort; expiry bounds an unconsumed ticket if storage
		// is unavailable. Consumed tickets have already been deleted atomically.
		_ = l.bindings.DeleteAccess(ctx, credentialHash(token))
	}}, nil
}

func (l *Local) validAccess(record ConnectionAccess) func() bool {
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
func (l *Local) Authorize(token string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
	if err := l.ctx.Err(); err != nil {
		return gateway.BindingContext{}, nil, err
	}
	ctx, cancel := context.WithTimeout(l.ctx, 500*time.Millisecond)
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
		return (access.Grant{Target: record.Target, Role: gateway.RoleSDK, Valid: l.validAccess(record)}).Bind()
	}
	target, err := l.bindings.MachineCredential(ctx, token)
	if err != nil {
		return gateway.BindingContext{}, nil, err
	}
	return (access.Grant{Target: target, Role: gateway.RoleDaemon, Valid: func() bool {
		if l.ctx.Err() != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(l.ctx, 500*time.Millisecond)
		defer cancel()
		current, err := l.bindings.MachineCredential(ctx, token)
		return err == nil && current == target
	}}).Bind()
}
