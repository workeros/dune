package metadata

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	public "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/storage"
)

type testIdentityProvider struct{ calls atomic.Int32 }

func (*testIdentityProvider) Namespace() string { return "https://issuer.example.test" }
func (*testIdentityProvider) Begin(ctx context.Context, c public.Challenge) (string, error) {
	return "https://issuer.example.test/login?state=" + c.State, nil
}
func (p *testIdentityProvider) Verify(ctx context.Context, c public.Challenge, code string) (public.Subject, error) {
	p.calls.Add(1)
	if len(c.Nonce) != 64 || len(c.Verifier) != 64 || c.RedirectURL != "https://dune.example.test/tools/dune/callback" || code != "verified" {
		return public.Subject{}, identity.ErrUnauthorized
	}
	return public.Subject{ID: "stable-subject", Email: "same@example.test"}, nil
}

func TestExternalIdentityTransactions(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			s, err := Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			other := s
			if backend == "postgres" {
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			local := identity.NewLocal(s, true)
			owner, cookie, err := local.Register(ctx, "same@example.test", "existing-test-password")
			if err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(time.Hour).Unix()
			if _, err := s.ExternalLogin(ctx, "issuer", public.Subject{ID: "rollback", Email: owner.Email}, wire.ID(), tokenHash(cookie), expires, 32); !errors.Is(err, ErrConflict) {
				t.Fatal("failed session did not abort identity creation", err)
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_external_identities`).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial external identity persisted", err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_principals`).Scan(&count); err != nil || count != 1 {
				t.Fatal("partial principal persisted", err)
			}
			provider := &testIdentityProvider{}
			start, err := identity.NewExternal(s, public.Options{Provider: provider})
			if err != nil {
				t.Fatal(err)
			}
			finish, err := identity.NewExternal(other, public.Options{Provider: provider})
			if err != nil {
				t.Fatal(err)
			}
			const callback = "https://dune.example.test/tools/dune/callback"
			location, proof, err := start.Begin(ctx, callback)
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(location)
			state := u.Query().Get("state")
			for _, bad := range []struct{ proof, redirect string }{{wire.ID() + wire.ID(), callback}, {proof, "https://other.test/callback"}} {
				if _, _, err := finish.Finish(ctx, state, bad.proof, "verified", bad.redirect); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal("callback binding bypassed", err)
				}
			}
			var wg sync.WaitGroup
			var wins atomic.Int32
			var result identity.User
			var session string
			for range 8 {
				wg.Go(func() {
					user, token, err := finish.Finish(ctx, state, proof, "verified", callback)
					if err == nil {
						if wins.Add(1) == 1 {
							result, session = user, token
						}
					} else if !errors.Is(err, identity.ErrUnauthorized) {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if wins.Load() != 1 || provider.calls.Load() != 1 {
				t.Fatalf("callback replayed: wins=%d exchanges=%d", wins.Load(), provider.calls.Load())
			}
			if result.ID == owner.ID {
				t.Fatal("email merged external identity with local account")
			}
			if got, err := finish.Authenticate(ctx, session); err != nil || got.ID != result.ID {
				t.Fatal("external session missing", err)
			}
			if _, err := local.Authenticate(ctx, session); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("external session accepted by local mode", err)
			}
			if _, err := finish.Authenticate(ctx, cookie); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("old local session bypassed external mode", err)
			}
			again, err := other.ExternalLogin(ctx, provider.Namespace(), public.Subject{ID: "stable-subject", Email: "changed@example.test"}, wire.ID(), tokenHash(wire.ID()), expires, 32)
			if err != nil || again.ID != result.ID {
				t.Fatal("email change replaced stable principal", err)
			}
			separate, err := other.ExternalLogin(ctx, "different-issuer", public.Subject{ID: "stable-subject", Email: again.Email}, wire.ID(), tokenHash(wire.ID()), expires, 32)
			if err != nil || separate.ID == result.ID {
				t.Fatal("issuer namespaces merged", err)
			}
			if err := s.SetPrincipalEnabled(ctx, result.ID, false); err != nil {
				t.Fatal(err)
			}
			if _, err := other.ExternalLogin(ctx, provider.Namespace(), public.Subject{ID: "stable-subject"}, wire.ID(), tokenHash(wire.ID()), expires, 32); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("upstream login bypassed Dune suspension", err)
			}
			// Expired state cannot reach a provider exchange.
			location, proof, err = start.Begin(ctx, callback)
			if err != nil {
				t.Fatal(err)
			}
			u, _ = url.Parse(location)
			state = u.Query().Get("state")
			if _, err := s.db.Exec(`UPDATE dune_login_transactions SET expires_at=1`); err != nil {
				t.Fatal(err)
			}
			if _, _, err := finish.Finish(ctx, state, proof, "verified", callback); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("expired callback accepted", err)
			}
			if provider.calls.Load() != 1 {
				t.Fatal("invalid callback called provider")
			}
			// Separate transactions starting the same new upstream identity converge.
			var ids sync.Map
			for i := range 8 {
				store := s
				if i%2 == 1 {
					store = other
				}
				wg.Go(func() {
					user, err := store.ExternalLogin(ctx, "concurrent", public.Subject{ID: "one-subject"}, wire.ID(), tokenHash(wire.ID()), expires, 32)
					if err != nil {
						t.Error(err)
					} else {
						ids.Store(user.ID, true)
					}
				})
			}
			wg.Wait()
			n := 0
			ids.Range(func(_, _ any) bool { n++; return true })
			if n != 1 {
				t.Fatal("concurrent external identity created multiple principals")
			}
		})
	}
}
