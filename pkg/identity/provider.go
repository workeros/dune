// Package identity defines the trusted browser identity adapter boundary.
// Dune owns login transactions, browser proof and its opaque sessions. Providers
// verify upstream identities; they never decide access to execution targets.
package identity

import (
	"context"
	"time"
)

// Challenge is generated and persisted by Dune for one ten-minute login. An
// adapter must work across application replicas using this challenge alone.
type Challenge struct{ State, Nonce, Verifier, RedirectURL string }

// Subject is a verified stable identifier within Provider.Namespace. Email is
// a display attribute only; it never links or merges existing Dune accounts.
// Do not return upstream access/refresh tokens or arbitrary identity claims.
type Subject struct{ ID, Email string }

// Provider is trusted application code. It must verify the full upstream login
// protocol, honor context, support concurrent calls and use no process-local
// callback state. Namespace is an immutable identity namespace (OIDC issuer).
type Provider interface {
	Namespace() string
	Begin(context.Context, Challenge) (string, error)
	Verify(context.Context, Challenge, string) (Subject, error)
}

type Options struct {
	Provider Provider
	// SessionLifetime defaults to eight hours and must be 1 minute..24 hours.
	// Without upstream revocation integration this bounds Dune's knowledge of
	// upstream suspension. The host can revoke a Dune principal sooner.
	SessionLifetime time.Duration
}
