package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	publicidentity "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
)

func TestPersonalEnrollmentIdentityCancellationAndExpiry(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			store, err := Open(ctx, config, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			local := identity.NewLocal(store, true)
			user, cookie, err := local.Register(ctx, "personal@example.test", "personal-enrollment-password")
			if err != nil {
				t.Fatal(err)
			}
			service := authorization.New(ctx, local, store, nil, nil)
			logical, token, _, err := service.IssueEnrollment(ctx, cookie, "pending")
			if err != nil || logical.ID == "" || logical.Binding != nil {
				t.Fatal(logical, err)
			}
			facts, err := store.AttachedRunner(ctx, user.ID, logical.ID)
			if err != nil || facts.Runner.ID != logical.ID || facts.CreatedBy.ID != user.ID {
				t.Fatal("command has no authoritative pending identity", facts, err)
			}
			_, otherCookie, err := local.Register(ctx, "other@example.test", "other-enrollment-password")
			if err != nil {
				t.Fatal(err)
			}
			if err := service.CancelEnrollment(ctx, otherCookie, logical.ID); !errors.Is(err, authorization.ErrNotFound) {
				t.Fatal("another owner cancelled the pending command", err)
			}
			if err := service.CancelEnrollment(ctx, cookie, logical.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Enroll(ctx, token, "linux", "amd64"); !errors.Is(err, publicidentity.ErrUnauthorized) {
				t.Fatal("cancelled token was accepted", err)
			}
			logical, token, _, err = service.IssueEnrollment(ctx, cookie, "enrolled")
			if err != nil {
				t.Fatal(err)
			}
			machine, credential, err := store.Enroll(ctx, token, "linux", "amd64")
			if err != nil || machine.RunnerID != logical.ID {
				t.Fatal("enrollment changed its preallocated identity", machine, err)
			}
			if err := service.CancelEnrollment(ctx, cookie, logical.ID); !errors.Is(err, runner.ErrBindingChanged) {
				t.Fatal("pending cancellation detached a bound Runner", err)
			}
			if _, err := store.MachineCredential(ctx, credential); err != nil {
				t.Fatal("cancellation revoked a bound credential", err)
			}
			logical, token, _, err = service.IssueEnrollment(ctx, cookie, "expired")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE dune_enrollments SET expires_at=$2 WHERE hash=$1`, tokenHash(token), time.Now().Add(-time.Second).Unix()); err != nil {
				t.Fatal(err)
			}
			replacement, _, _, err := service.IssueEnrollment(ctx, cookie, "replacement")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AttachedRunner(ctx, user.ID, logical.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("expired pending command remained active", err)
			}
			if err := local.Logout(ctx, cookie); err != nil {
				t.Fatal(err)
			}
			if err := store.CancelEnrollmentForSession(ctx, user, replacement.ID, tokenHash(cookie)); !errors.Is(err, publicidentity.ErrUnauthorized) {
				t.Fatal("revoked session cancelled a pending command during commit", err)
			}
			if _, err := store.AttachedRunner(ctx, user.ID, replacement.ID); err != nil {
				t.Fatal("rejected cancellation mutated pending state", err)
			}
		})
	}
}
