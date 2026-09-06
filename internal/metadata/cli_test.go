package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/login"
	"github.com/aiomni/dune/pkg/storage"
)

func TestCLILoginTransactions(t *testing.T) {
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
			user, cookie, err := local.Register(ctx, "cli@example.test", "cli-test-password")
			if err != nil {
				t.Fatal(err)
			}
			second, otherCookie, err := local.Register(ctx, "other@example.test", "other-test-password")
			if err != nil {
				t.Fatal(err)
			}
			const site = "https://dune.example.test/tools/dune/"
			proof := wire.ID() + wire.ID()
			request, err := s.BeginCLI(ctx, tokenHash(proof), site, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ConsumeCLI(ctx, request.ID, site, "", proof); !errors.Is(err, identity.ErrPending) {
				t.Fatal("unconfirmed login consumed", err)
			}
			for _, bad := range []struct{ site, namespace, proof string }{{site + "wrong/", "", proof}, {site, "another-issuer", proof}, {site, "", wire.ID() + wire.ID()}} {
				if _, err := s.ConsumeCLI(ctx, request.ID, bad.site, bad.namespace, bad.proof); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal("CLI challenge binding bypassed", err)
				}
			}
			if err := s.ConfirmCLI(ctx, request.ID, site, "", "WRONG", cookie, user.ID, true); !errors.Is(err, identity.ErrInvalidArgument) {
				t.Fatal("wrong display code confirmed", err)
			}
			if _, err := s.db.Exec(`UPDATE dune_sessions SET auth_version=auth_version+1 WHERE hash=$1`, tokenHash(cookie)); err != nil {
				t.Fatal(err)
			}
			if err := s.ConfirmCLI(ctx, request.ID, site, "", request.Code, cookie, user.ID, true); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("mismatched browser authorization version confirmed", err)
			}
			if _, err := s.db.Exec(`UPDATE dune_sessions SET auth_version=auth_version-1 WHERE hash=$1`, tokenHash(cookie)); err != nil {
				t.Fatal(err)
			}
			if err := s.ConfirmCLI(ctx, request.ID, site, "", request.Code, cookie, user.ID, true); err != nil {
				t.Fatal(err)
			}
			if err := s.ConfirmCLI(ctx, request.ID, site, "", request.Code, otherCookie, second.ID, true); !errors.Is(err, ErrConflict) {
				t.Fatal("another browser replaced approved identity", err)
			}
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
			before := snapshotRecords(t, s)
			fail, restore := `CREATE TRIGGER cli_failure BEFORE UPDATE OF kind ON dune_sessions WHEN NEW.kind='cli' BEGIN SELECT RAISE(ABORT,'cli failure'); END`, `DROP TRIGGER cli_failure`
			if backend == "postgres" {
				fail = `ALTER TABLE dune_sessions ADD CONSTRAINT cli_failure CHECK(kind='browser')`
				restore = `ALTER TABLE dune_sessions DROP CONSTRAINT cli_failure`
			}
			if _, err := s.db.Exec(fail); err != nil {
				t.Fatal(err)
			}
			if _, err := other.ConsumeCLI(ctx, request.ID, site, "", proof); err == nil {
				t.Fatal("failed CLI session creation accepted")
			}
			if !reflect.DeepEqual(before, snapshotRecords(t, s)) {
				t.Fatal("partial consumption survived failed session creation")
			}
			if _, err := s.db.Exec(restore); err != nil {
				t.Fatal(err)
			}
			var wins atomic.Int32
			var session login.Session
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					value, err := other.ConsumeCLI(ctx, request.ID, site, "", proof)
					if err == nil {
						if wins.Add(1) == 1 {
							session = value
						}
					} else if !errors.Is(err, identity.ErrUnauthorized) {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if wins.Load() != 1 || session.PrincipalID != user.ID || session.Site != site || session.ExpiresAt > time.Now().Add(identity.CLISessionLifetime).Unix() {
				t.Fatal("CLI result is not single-use/bounded", wins.Load())
			}
			for _, data := range snapshotRecords(t, s) {
				if strings.Contains(data, proof) || strings.Contains(data, session.Token) || strings.Contains(data, cookie) {
					t.Fatal("raw CLI/browser proof persisted")
				}
			}
			if got, err := local.AuthenticateCLI(ctx, session.Token); err != nil || got.ID != user.ID {
				t.Fatal("CLI session invalid", err)
			}
			if _, err := local.Authenticate(ctx, session.Token); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("CLI session became browser cookie", err)
			}
			if _, err := local.AuthenticateCLI(ctx, cookie); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("browser session became CLI bearer", err)
			}
			if _, err := s.MachineCredential(ctx, session.Token); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("CLI session became machine credential", err)
			}
			if err := local.Logout(ctx, session.Token); err != nil {
				t.Fatal(err)
			}
			if _, err := local.Authenticate(ctx, cookie); err != nil {
				t.Fatal("CLI logout revoked browser", err)
			}
			makeSession := func() (login.Session, login.Request) {
				t.Helper()
				request, err := s.BeginCLI(ctx, tokenHash(proof), site, "")
				if err != nil {
					t.Fatal(err)
				}
				if err := s.ConfirmCLI(ctx, request.ID, site, "", request.Code, cookie, user.ID, true); err != nil {
					t.Fatal(err)
				}
				session, err := other.ConsumeCLI(ctx, request.ID, site, "", proof)
				if err != nil {
					t.Fatal(err)
				}
				return session, request
			}
			session, _ = makeSession()
			if _, err := s.db.Exec(`UPDATE dune_sessions SET expires_at=0 WHERE hash=$1`, tokenHash(cookie)); err != nil {
				t.Fatal(err)
			}
			if _, err := local.AuthenticateCLI(ctx, session.Token); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("child outlived shortened parent", err)
			}
			if _, err := s.db.Exec(`UPDATE dune_sessions SET expires_at=$2 WHERE hash=$1`, tokenHash(cookie), time.Now().Add(time.Hour).Unix()); err != nil {
				t.Fatal(err)
			}
			pending, err := s.BeginCLI(ctx, tokenHash(proof), site, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ConfirmCLI(ctx, pending.ID, site, "", pending.Code, cookie, user.ID, true); err != nil {
				t.Fatal(err)
			}
			if err := local.Logout(ctx, cookie); err != nil {
				t.Fatal(err)
			}
			if _, err := local.AuthenticateCLI(ctx, session.Token); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("child survived parent logout", err)
			}
			if _, err := other.ConsumeCLI(ctx, pending.ID, site, "", proof); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("confirmed request survived parent logout", err)
			}
			_, cookie, err = local.Login(ctx, user.Email, "cli-test-password")
			if err != nil {
				t.Fatal(err)
			}
			denied, err := s.BeginCLI(ctx, tokenHash(proof), site, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ConfirmCLI(ctx, denied.ID, site, "", denied.Code, cookie, user.ID, false); err != nil {
				t.Fatal(err)
			}
			if _, err := other.ConsumeCLI(ctx, denied.ID, site, "", proof); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("denied CLI login consumed", err)
			}
			expired, err := s.BeginCLI(ctx, tokenHash(proof), site, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE dune_cli_logins SET expires_at=0 WHERE id=$1`, expired.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.ConfirmCLI(ctx, expired.ID, site, "", expired.Code, cookie, user.ID, true); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("expired CLI login confirmed", err)
			}
			session, _ = makeSession()
			if err := s.SetPrincipalEnabled(ctx, user.ID, false); err != nil {
				t.Fatal(err)
			}
			if _, err := local.AuthenticateCLI(ctx, session.Token); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("disabled user kept CLI access", err)
			}
		})
	}
}
