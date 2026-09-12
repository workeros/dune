package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
)

type discoveryCheck func(context.Context, access.Request) (access.Decision, error)

func (f discoveryCheck) Check(ctx context.Context, r access.Request) (access.Decision, error) {
	return f(ctx, r)
}

func discoveryDecision(r access.Request, allowed bool) access.Decision {
	return access.Decision{Allowed: allowed, Reason: "TEST_POLICY", ID: r.RequestID, ValidUntil: time.Now().Add(30 * time.Second)}
}

func catalogID(n int) string { return fmt.Sprintf("%032x", n+1) }

func TestAuthorizedDiscoveryUsesStatelessCursors(t *testing.T) {
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
			user, cookie, err := local.Register(ctx, "discovery@example.test", "discovery-test-password")
			if err != nil {
				t.Fatal(err)
			}
			err = s.transaction(ctx, func(tx *sql.Tx) error {
				for i := range 132 {
					id := catalogID(i)
					owner := "other"
					if i >= 128 {
						owner = user.ID
					}
					_, err := tx.Exec(`INSERT INTO dune_runners(id,owner_id,created_by_id,created_by_namespace,created_by_subject,name,kind,fabric_id,binding_revision,machine_id,credential_hash,os,arch,created_at) VALUES($1,$2,$2,'','',$1,'attached','attached',1,$3,$1,'linux','amd64',1)`, id, owner, "machine-"+id)
					if err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			checker := discoveryCheck(func(_ context.Context, r access.Request) (access.Decision, error) {
				calls++
				allowed := r.OwnerID == user.ID && r.Binding.RunnerID != catalogID(129)
				return discoveryDecision(r, allowed), nil
			})
			service := authorization.New(ctx, local, s, checker)
			first, err := service.Discover(ctx, user, runner.Query{Limit: 2}, false)
			if err != nil || len(first.Items) != 0 || calls != 128 || first.NextCursor == "" {
				t.Fatal("bounded first scan", first, calls, err)
			}
			cursor := first.NextCursor
			other := s
			if backend == "sqlite" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				other = s
			} else {
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			local = identity.NewLocal(other, true)
			service = authorization.New(ctx, local, other, checker)
			second, err := service.Discover(ctx, user, runner.Query{Cursor: cursor, Limit: 2}, false)
			if err != nil || len(second.Items) != 2 || second.Items[0].Runner.ID != catalogID(128) || second.Items[1].Runner.ID != catalogID(130) || second.NextCursor == "" {
				t.Fatal("filtered page lost allowed rows", second, err)
			}
			last, err := service.Discover(ctx, user, runner.Query{Cursor: second.NextCursor, Limit: 2}, false)
			if err != nil || len(last.Items) != 1 || last.Items[0].Runner.ID != catalogID(131) || last.NextCursor != "" {
				t.Fatal("incorrect last page", last, err)
			}
			if _, err := other.ReadCursor(ctx, "any", "scope", "value", "not-a-cursor"); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal("malformed cursor accepted", err)
			}
			if repeated, err := other.SaveCursor(ctx, "any", "scope", "value", catalogID(127)); err != nil || repeated != cursor {
				t.Fatal("stateless cursor changed across replicas", repeated, err)
			}
			ownerService := authorization.NewLocal(ctx, local, other)
			owned, err := ownerService.Discover(ctx, user, runner.Query{}, true)
			if err != nil || len(owned.Items) != 4 {
				t.Fatal("default owner discovery", owned, err)
			}
			if _, err := ownerService.Client(ctx, cookie, "machine-"+catalogID(0)); !errors.Is(err, authorization.ErrNotFound) {
				t.Fatal("owner accepted another user's machine", err)
			}
		})
	}
}

func TestAuthorizedWritesRecheckSessionAndBinding(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	local := identity.NewLocal(s, true)
	user, cookie, err := local.Register(ctx, "writes@example.test", "writes-test-password")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.IssueEnrollmentForSession(ctx, user, "runner", tokenHash(cookie))
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := s.Enroll(ctx, token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.MachineResource(ctx, machine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE dune_runners SET binding_revision=2 WHERE id=$1`, selected.Runner.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAuthorized(ctx, user, tokenHash(cookie), selected); !errors.Is(err, runner.ErrBindingChanged) {
		t.Fatal("stale approval removed replacement", err)
	}
	selected, err = s.MachineResource(ctx, machine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserEnabled(ctx, user.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserEnabled(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IssueEnrollmentForSession(ctx, user, "stale", tokenHash(cookie)); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("stale session enrolled", err)
	}
	if err := s.RevokeAuthorized(ctx, user, tokenHash(cookie), selected); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("stale session revoked", err)
	}
}

func TestEnrollmentPolicyIsRechecked(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	local := identity.NewLocal(s, true)
	_, cookie, err := local.Register(ctx, "enroll@example.test", "enroll-test-password")
	if err != nil {
		t.Fatal(err)
	}
	allowed := true
	service := authorization.New(ctx, local, s, discoveryCheck(func(_ context.Context, r access.Request) (access.Decision, error) {
		return discoveryDecision(r, allowed), nil
	}))
	token, _, err := service.IssueEnrollment(ctx, cookie, "policy-controlled")
	if err != nil {
		t.Fatal(err)
	}
	allowed = false
	if _, err := service.EnrollmentDecision(ctx, token); !errors.Is(err, access.ErrDenied) {
		t.Fatal("enrollment reused old decision", err)
	}
	allowed = true
	if _, err := service.EnrollmentDecision(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Enroll(ctx, token, "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnrollmentDecision(ctx, token); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("consumed enrollment authorized again", err)
	}
}
