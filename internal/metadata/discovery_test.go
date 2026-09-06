package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

func TestAuthorizedDiscoveryAndWrites(t *testing.T) {
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
			user, cookie, err := local.Register(ctx, "discovery@example.test", "discovery-test-password")
			if err != nil {
				t.Fatal(err)
			}
			// A large catalog fixture exercises bounded scanning independently of
			// the personal enrollment quota. The first 128 rows belong to others.
			err = s.transaction(ctx, func(tx *sql.Tx) error {
				if _, err := tx.Exec(`INSERT INTO dune_principals(id,email) VALUES('other','other@example.test')`); err != nil {
					return err
				}
				for i := range 132 {
					id := fmt.Sprintf("runner-%03d", i)
					owner := "other"
					if i >= 128 {
						owner = user.ID
					}
					if _, err := tx.Exec(`INSERT INTO dune_runners(id,owner_id,name,kind,fabric_id,binding_revision,created_at) VALUES($1,$2,$1,'attached','attached',1,1)`, id, owner); err != nil {
						return err
					}
					if _, err := tx.Exec(`INSERT INTO dune_machines(id,runner_id,credential_hash,os,arch) VALUES($1,$2,$1,'linux','amd64')`, "machine-"+id, id); err != nil {
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
				if r.PrincipalID != user.ID || r.Namespace != user.Namespace || r.Binding.FabricID != "attached" || r.Binding.MachineID != "machine-"+r.Binding.RunnerID {
					t.Error("non-authoritative scope", r.Scope)
				}
				return discoveryDecision(r, r.OwnerID == user.ID && r.Binding.RunnerID != "runner-129"), nil
			})
			service := authorization.New(ctx, local, s, checker)
			first, err := service.Discover(ctx, user, runner.Query{Limit: 2}, false)
			if err != nil || len(first.Items) != 0 || calls != 128 || first.NextCursor == "" || strings.Contains(first.NextCursor, "runner") {
				t.Fatal("unbounded or revealing discovery", first, calls, err)
			}
			cursor := first.NextCursor
			// SQLite restart and a separate PostgreSQL pool must understand exactly
			// the same opaque cursor, without issuer-local state.
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
			if err != nil || len(second.Items) != 2 || second.Items[0].Runner.ID != "runner-128" || second.Items[1].Runner.ID != "runner-130" || second.NextCursor == "" {
				t.Fatal("filtered page lost allowed rows", second, err)
			}
			last, err := service.Discover(ctx, user, runner.Query{Cursor: second.NextCursor, Limit: 2}, false)
			if err != nil || len(last.Items) != 1 || last.Items[0].Runner.ID != "runner-131" || last.NextCursor != "" {
				t.Fatal("incorrect last page", last, err)
			}
			for _, fields := range [][3]string{{"other", "", "runner.list"}, {user.ID, "another-provider", "runner.list"}, {user.ID, "", "machine.list"}} {
				if _, err := other.ReadCursor(ctx, fields[0], fields[1], fields[2], cursor); !errors.Is(err, ErrInvalidArgument) {
					t.Fatal("cursor crossed scope", fields, err)
				}
			}
			if repeated, err := other.SaveCursor(ctx, user.ID, "", "runner.list", "runner-127"); err != nil || repeated != cursor {
				t.Fatal("refresh creates new cursor", err)
			}
			if _, err := other.db.Exec(`UPDATE dune_discovery_cursors SET expires_at=0 WHERE id=$1`, cursor); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Discover(ctx, user, runner.Query{Cursor: cursor}, false); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal("expired cursor accepted", err)
			}
			var count int
			if err := other.db.QueryRow(`SELECT COUNT(*) FROM dune_discovery_cursors WHERE principal_id=$1 AND expires_at>0`, user.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			for i := count; i < 64; i++ {
				if _, err := other.SaveCursor(ctx, user.ID, "", "runner.list", fmt.Sprintf("position-%d", i)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := other.SaveCursor(ctx, user.ID, "", "runner.list", "overflow"); !errors.Is(err, identity.ErrLoginLimit) {
				t.Fatal("cursor storage is unbounded", err)
			}
			if repeated, err := other.SaveCursor(ctx, user.ID, "", "runner.list", "runner-130"); err != nil || repeated != second.NextCursor {
				t.Fatal("full cursor budget broke refresh", err)
			}
			// Default owner mode still filters at the repository and rejects shared
			// connection attempts; installing a checker replaces it completely.
			ownerService := authorization.NewLocal(ctx, local, other)
			owned, err := ownerService.Discover(ctx, user, runner.Query{}, true)
			if err != nil || len(owned.Items) != 4 {
				t.Fatal("default owner discovery", owned, err)
			}
			if _, err := ownerService.Client(ctx, cookie, "machine-runner-000"); !errors.Is(err, authorization.ErrNotFound) {
				t.Fatal("owner accepted another principal's machine", err)
			}
			for _, failure := range []string{"deny", "error", "timeout"} {
				broken := authorization.New(ctx, local, other, discoveryCheck(func(ctx context.Context, r access.Request) (access.Decision, error) {
					if failure == "error" {
						return access.Decision{}, errors.New("private upstream error")
					}
					if failure == "timeout" {
						<-ctx.Done()
						return discoveryDecision(r, true), nil
					}
					return discoveryDecision(r, false), nil
				}))
				_, _, err := broken.Resource(ctx, user, "runner-128", false, "runner.get")
				if failure == "deny" && !errors.Is(err, authorization.ErrNotFound) || failure != "deny" && !errors.Is(err, access.ErrUnavailable) {
					t.Fatal("checker failure fell back to owner", failure, err)
				}
			}
			allow := discoveryCheck(func(_ context.Context, r access.Request) (access.Decision, error) {
				return discoveryDecision(r, true), nil
			})
			shared := authorization.New(ctx, local, other, allow)
			grant, err := shared.Client(ctx, cookie, "machine-runner-000")
			if err != nil {
				t.Fatal("explicit shared connection denied", err)
			}
			if _, _, err := shared.Authorize(grant.Token()); err != nil || !grant.Valid() {
				t.Fatal("shared ticket lost authoritative owner", err)
			}
			if _, err := other.db.Exec(`UPDATE dune_runners SET owner_id=$1 WHERE id='runner-000'`, user.ID); err != nil {
				t.Fatal(err)
			}
			if grant.Valid() {
				t.Fatal("shared connection followed ownership change")
			}
			// Approval is tied to the exact binding; a replacement between check
			// and write cannot be deleted by the earlier decision.
			selected, err := other.RunnerResource(ctx, "runner-001")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := other.db.Exec(`UPDATE dune_runners SET binding_revision=2 WHERE id='runner-001'`); err != nil {
				t.Fatal(err)
			}
			if err := other.RevokeAuthorized(ctx, user, tokenHash(cookie), selected); !errors.Is(err, runner.ErrBindingChanged) {
				t.Fatal("stale approval removed replacement", err)
			}
			selected, err = other.RunnerResource(ctx, "runner-001")
			if err != nil {
				t.Fatal(err)
			}
			// A principal may be enabled again before an already authenticated
			// request reaches storage. Its old browser session remains revoked.
			if err := other.SetPrincipalEnabled(ctx, user.ID, false); err != nil {
				t.Fatal(err)
			}
			if err := other.SetPrincipalEnabled(ctx, user.ID, true); err != nil {
				t.Fatal(err)
			}
			if _, _, err := other.IssueEnrollmentForSession(ctx, user, "stale", tokenHash(cookie)); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("stale session enrolled", err)
			}
			if err := other.RevokeAuthorized(ctx, user, tokenHash(cookie), selected); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("stale session revoked", err)
			}
			if _, err := other.RunnerResource(ctx, selected.Runner.ID); err != nil {
				t.Fatal("rejected write changed binding", err)
			}
		})
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
		if r.Operation != "runner.create" || r.Suboperation != "attached" || r.PrincipalID != r.OwnerID {
			t.Error("incorrect enrollment check", r)
		}
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
	if _, _, err := service.IssueEnrollment(ctx, cookie, "denied"); !errors.Is(err, access.ErrDenied) {
		t.Fatal("denied owner issued enrollment", err)
	}
	allowed = true
	if _, err := service.EnrollmentDecision(ctx, token); err != nil {
		t.Fatal("policy denial consumed enrollment", err)
	}
	if _, _, err := s.Enroll(ctx, token, "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnrollmentDecision(ctx, token); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("consumed enrollment authorized again", err)
	}
}
