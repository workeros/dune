package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/storage"
)

func TestLocalUserSuspensionTransactions(t *testing.T) {
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
			local := identity.NewLocal(s, true)
			user, cookie, err := local.Register(ctx, "suspend@example.test", "suspend-test-password")
			if err != nil {
				t.Fatal(err)
			}
			_, otherCookie, err := local.Register(ctx, "other@example.test", "other-test-password")
			if err != nil {
				t.Fatal(err)
			}
			pending, _, err := s.IssueEnrollment(ctx, user.ID, "running machine")
			if err != nil {
				t.Fatal(err)
			}
			machine, credential, err := s.Enroll(ctx, pending, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			pending, _, err = s.IssueEnrollment(ctx, user.ID, "pending install")
			if err != nil {
				t.Fatal(err)
			}
			// Fail pending enrollment revocation after principal/session updates.
			if _, err := s.db.Exec(`ALTER TABLE dune_enrollments RENAME TO dune_enrollments_paused`); err != nil {
				t.Fatal(err)
			}
			if err := s.SetUserEnabled(ctx, user.ID, false); err == nil {
				t.Fatal("partial suspension succeeded")
			}
			var enabled bool
			var version int64
			if err := s.db.QueryRow(`SELECT enabled,auth_version FROM dune_users WHERE id=$1`, user.ID).Scan(&enabled, &version); err != nil || !enabled || version != 1 {
				t.Fatal("failed suspension changed user", err)
			}
			if _, err := s.db.Exec(`ALTER TABLE dune_enrollments_paused RENAME TO dune_enrollments`); err != nil {
				t.Fatal(err)
			}
			if _, err := local.Authenticate(ctx, cookie); err != nil {
				t.Fatal("failed suspension revoked login", err)
			}
			other := s
			if backend == "postgres" {
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					err := other.CreateSession(ctx, user.ID, tokenHash(wire.ID()), time.Now().Add(time.Hour).Unix(), 32)
					if err != nil && !errors.Is(err, identity.ErrUnauthorized) {
						t.Error(err)
					}
				})
			}
			if err := s.SetUserEnabled(ctx, user.ID, false); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if err := s.SetUserEnabled(ctx, user.ID, false); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_sessions WHERE user_id=$1`, user.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("concurrent login survived suspension", err)
			}
			if _, err := local.Authenticate(ctx, cookie); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("suspended cookie accepted", err)
			}
			if _, _, err := local.Login(ctx, user.Email, "suspend-test-password"); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("suspended password accepted", err)
			}
			if _, _, err := s.IssueEnrollmentForSession(ctx, user, "new machine", tokenHash(cookie)); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("suspended user created enrollment", err)
			}
			if id, err := other.MachineCredential(ctx, credential); err != nil || id != machine.ID {
				t.Fatal("user suspension revoked independent machine identity", err)
			}
			if _, err := local.Authenticate(ctx, otherCookie); err != nil {
				t.Fatal("suspension affected another principal", err)
			}
			for range 2 {
				if err := s.SetUserEnabled(ctx, user.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.db.QueryRow(`SELECT auth_version FROM dune_users WHERE id=$1`, user.ID).Scan(&version); err != nil || version != 3 {
				t.Fatal("state changes did not advance version exactly once", err)
			}
			if _, err := local.Authenticate(ctx, cookie); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("re-enabling revived old cookie", err)
			}
			if _, _, err := s.Enroll(ctx, pending, "linux", "amd64"); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("re-enabling revived an old installation token", err)
			}
			// Even a retained old session row cannot cross a principal version.
			if _, err := s.db.Exec(`INSERT INTO dune_sessions(hash,user_id,expires_at,auth_version) VALUES($1,$2,$3,1)`, tokenHash(cookie), user.ID, time.Now().Add(time.Hour).Unix()); err != nil {
				t.Fatal(err)
			}
			if _, err := local.Authenticate(ctx, cookie); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("stale session version accepted", err)
			}
			if _, _, err := local.Login(ctx, user.Email, "suspend-test-password"); err != nil {
				t.Fatal("fresh login after enabling failed", err)
			}
			if err := s.SetUserEnabled(ctx, "missing", false); !errors.Is(err, ErrNotFound) {
				t.Fatal("unknown principal accepted", err)
			}
		})
	}
}
