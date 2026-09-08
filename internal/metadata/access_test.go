package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	public "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/storage"
)

func TestDurableAccessCredentials(t *testing.T) {
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
			defer func() { s.Close() }()
			local := identity.NewLocal(s, true)
			user, cookie, err := local.Register(ctx, "access@example.test", "access-test-password")
			if err != nil {
				t.Fatal(err)
			}
			enrollment, _, err := s.IssueEnrollment(ctx, user.ID, "access machine")
			if err != nil {
				t.Fatal(err)
			}
			machine, credential, err := s.Enroll(ctx, enrollment, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			issuerCtx, stopIssuer := context.WithCancel(ctx)
			defer stopIssuer()
			issuer := authorization.NewLocal(issuerCtx, local, s)
			grant, err := issuer.Client(ctx, cookie, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, records := range snapshotRecords(t, s) {
				if strings.Contains(records, grant.Token()) || strings.Contains(records, cookie) {
					t.Fatal("raw credential persisted")
				}
			}
			stopIssuer()
			if backend == "sqlite" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				local = identity.NewLocal(s, true)
			}
			other := s
			if backend == "postgres" {
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			receiver := authorization.NewLocal(ctx, identity.NewLocal(other, true), other)
			foreign, err := identity.NewExternal(other, public.Options{Provider: &testIdentityProvider{}})
			if err != nil {
				t.Fatal(err)
			}
			wrongNamespace := authorization.NewLocal(ctx, foreign, other)
			if _, _, err := wrongNamespace.Authorize(grant.Token()); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("wrong identity mode consumed ticket", err)
			}
			var wins atomic.Int32
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					binding, handler, err := receiver.Authorize(grant.Token())
					if err == nil {
						if binding.Target != machine.ID || binding.Role != gateway.RoleSDK || handler == nil {
							t.Error("ticket changed target or role")
						}
						wins.Add(1)
					} else if !errors.Is(err, identity.ErrUnauthorized) {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatal("ticket consumption is not unique", wins.Load())
			}
			if _, _, err := receiver.Authorize(cookie); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("browser cookie authenticated tunnel", err)
			}
			if binding, _, err := receiver.Authorize(credential); err != nil || binding.Role != gateway.RoleDaemon || binding.Online == nil {
				t.Fatal("machine identity broken", err)
			} else if err := binding.Online(ctx, api.Binding{Version: api.Version, Target: machine.ID, Incarnation: "attached-test", Generation: 1}); err != nil {
				t.Fatal("attached machine online callback changed behavior", err)
			}
			if _, err := receiver.Client(ctx, credential, machine.ID); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("machine authenticated as user", err)
			}
			issuer = authorization.NewLocal(ctx, local, s)
			expired, err := issuer.Client(ctx, cookie, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE dune_access_tickets SET expires_at=0 WHERE hash=$1`, tokenHash(expired.Token())); err != nil {
				t.Fatal(err)
			}
			if _, _, err := receiver.Authorize(expired.Token()); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("expired ticket accepted", err)
			}
			active, err := issuer.Client(ctx, cookie, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := receiver.Authorize(active.Token()); err != nil {
				t.Fatal(err)
			}
			active.Close()
			if !active.Valid() {
				t.Fatal("ticket cleanup revoked established access")
			}
			stale, err := issuer.Client(ctx, cookie, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := other.db.Exec(`UPDATE dune_runners SET binding_revision=binding_revision+1 WHERE id=$1`, machine.RunnerID); err != nil {
				t.Fatal(err)
			}
			if active.Valid() {
				t.Fatal("old access followed changed binding")
			}
			if _, _, err := receiver.Authorize(stale.Token()); err == nil {
				t.Fatal("pending ticket followed changed binding")
			}
			current, err := issuer.Client(ctx, cookie, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := receiver.Authorize(current.Token()); err != nil {
				t.Fatal(err)
			}
			pending, err := issuer.Client(ctx, cookie, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			for range 8 {
				wg.Go(func() {
					if _, err := issuer.Client(ctx, cookie, machine.ID); err != nil && !errors.Is(err, identity.ErrUnauthorized) {
						t.Error("issuance racing logout", err)
					}
				})
			}
			if err := local.Logout(ctx, cookie); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if current.Valid() {
				t.Fatal("logout did not revoke live access")
			}
			if _, _, err := receiver.Authorize(pending.Token()); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("logout did not revoke pending ticket", err)
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_access_tickets`).Scan(&count); err != nil || count != 0 {
				t.Fatal("session deletion left access tickets", err, count)
			}
			_, cookie, err = local.Login(ctx, user.Email, "access-test-password")
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 64; i++ {
				if _, err := issuer.Client(ctx, cookie, machine.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := issuer.Client(ctx, cookie, machine.ID); !errors.Is(err, identity.ErrLoginLimit) {
				t.Fatal("unbounded pending tickets", err)
			}
			if err := s.SetPrincipalEnabled(ctx, user.ID, false); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_access_tickets`).Scan(&count); err != nil || count != 0 {
				t.Fatal("suspension left pending tickets", err, count)
			}
			if err := s.SetPrincipalEnabled(ctx, user.ID, true); err != nil {
				t.Fatal(err)
			}
			_, cookie, err = local.Login(ctx, user.Email, "access-test-password")
			if err != nil {
				t.Fatal(err)
			}
			last, err := issuer.Client(ctx, cookie, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			for range 8 {
				wg.Go(func() {
					if _, err := issuer.Client(ctx, cookie, machine.ID); err != nil && !errors.Is(err, authorization.ErrNotFound) {
						t.Error("issuance racing machine revocation", err)
					}
				})
			}
			if err := other.Revoke(ctx, user.ID, machine.ID); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if last.Valid() {
				t.Fatal("machine revocation left live access")
			}
			if _, _, err := receiver.Authorize(last.Token()); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("machine revocation left pending ticket", err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_access_tickets WHERE expires_at>$1`, time.Now().Unix()).Scan(&count); err != nil || count != 0 {
				t.Fatal("machine deletion left tickets", err)
			}
		})
	}
}
