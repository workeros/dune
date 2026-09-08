package metadata

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/storage"
)

func TestLocalIdentityPolicyOnSQL(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			ctx := context.Background()
			s, err := Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			local := identity.NewLocal(s, true)
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			const password = "local-test-password"
			if _, _, err := local.Register(canceled, "owner@example.test", password); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled registration: %v", err)
			}
			user, cookie, err := local.Register(ctx, "owner@example.test", password)
			if err != nil {
				t.Fatal(err)
			}
			for _, email := range []string{user.Email, "missing@example.test"} {
				if _, _, err := local.Login(ctx, email, "wrong-password"); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal("incorrect password or nonexistent user authenticated", err)
				}
			}
			account, err := s.ReadAccount(ctx, user.Email)
			if err != nil || account.PasswordHash == password || !hexValue(account.PasswordHash, 64) || !hexValue(account.Salt, 32) {
				t.Fatal("password was not salted and hashed", err)
			}
			var hash string
			if err := s.db.QueryRowContext(ctx, `SELECT hash FROM dune_sessions WHERE principal_id=$1`, user.ID).Scan(&hash); err != nil || hash != tokenHash(cookie) {
				t.Fatal("session token not hashed", err)
			}
			enrollment, _, err := s.IssueEnrollment(ctx, user.ID, "private machine")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT hash FROM dune_enrollments WHERE principal_id=$1`, user.ID).Scan(&hash); err != nil || hash != tokenHash(enrollment) {
				t.Fatal("enrollment token not hashed", err)
			}
			machine, credential, err := s.Enroll(ctx, enrollment, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT credential_hash FROM dune_machines WHERE id=$1`, machine.ID).Scan(&hash); err != nil || hash != tokenHash(credential) {
				t.Fatal("machine credential not hashed", err)
			}
			for i := 1; i < 32; i++ {
				if err := s.CreateSession(ctx, user.ID, tokenHash(fmt.Sprint(i)), time.Now().Add(time.Hour).Unix(), 32); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := local.Login(ctx, user.Email, password); !errors.Is(err, identity.ErrSessionLimit) {
				t.Fatalf("local session limit: %v", err)
			}
			if err := local.Logout(ctx, cookie); err != nil {
				t.Fatal(err)
			}
			_, cookie, err = local.Login(ctx, user.Email, password)
			if err != nil {
				t.Fatal("logout did not free a session slot", err)
			}
			enrollment, _, err = s.IssueEnrollment(ctx, user.ID, "expired")
			if err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"dune_sessions", "dune_enrollments"} {
				if _, err := s.db.ExecContext(ctx, "UPDATE "+table+" SET expires_at=1"); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.Enroll(ctx, enrollment, "linux", "amd64"); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("expired enrollment accepted", err)
			}
			if _, err := local.Authenticate(ctx, cookie); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("expired session accepted", err)
			}
			if _, _, err := local.Login(ctx, user.Email, password); err != nil {
				t.Fatal("expired sessions retained their slots", err)
			}
		})
	}
}

func hexValue(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == size && len(value) == size
}
