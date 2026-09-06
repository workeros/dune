package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	public "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
)

type subjectChecker struct {
	mu       sync.Mutex
	requests []access.Request
}

func (c *subjectChecker) Check(_ context.Context, r access.Request) (access.Decision, error) {
	c.mu.Lock()
	c.requests = append(c.requests, r)
	c.mu.Unlock()
	return access.Decision{Allowed: r.Namespace == "https://issuer.example.test" && r.Subject == "alpha", ID: wire.ID(), Reason: "SUBJECT", ValidUntil: time.Now().Add(access.MaxLease)}, nil
}

func TestAuthenticatedSubjectIsFixed(t *testing.T) {
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
			owner, _, err := identity.NewLocal(s, true).Register(ctx, "subject@example.test", "subject-test-password")
			if err != nil {
				t.Fatal(err)
			}
			provider := &testIdentityProvider{}
			// Two explicitly linked subjects share a Dune principal and namespace.
			for _, subject := range []string{"alpha", "beta"} {
				_, err = s.LinkIdentity(ctx, public.LinkRequest{RequestID: subject, Actor: "test-admin", PrincipalID: owner.ID, Namespace: provider.Namespace(), Subject: subject, Reason: "verified test identity"})
				if err != nil {
					t.Fatal(err)
				}
			}
			cookies := map[string]string{}
			for _, subject := range []string{"alpha", "beta"} {
				cookies[subject] = wire.ID() + wire.ID()
				user, err := s.ExternalLogin(ctx, provider.Namespace(), public.Subject{ID: subject, Email: "same@example.test"}, wire.ID(), tokenHash(cookies[subject]), time.Now().Add(time.Hour).Unix(), 32)
				if err != nil || user.ID != owner.ID || user.Subject != subject || user.Namespace != provider.Namespace() {
					t.Fatal("login identity changed", user, err)
				}
				encoded, err := json.Marshal(user)
				if err != nil || strings.Contains(string(encoded), subject) || strings.Contains(string(encoded), provider.Namespace()) {
					t.Fatal("private identity exposed in browser user", err)
				}
			}
			external, err := identity.NewExternal(s, public.Options{Provider: provider})
			if err != nil {
				t.Fatal(err)
			}
			checker := &subjectChecker{}
			service := authorization.New(ctx, external, s, checker)
			wrong := identity.User{ID: owner.ID, Namespace: provider.Namespace(), Subject: "beta"}
			if _, _, err := s.IssueEnrollmentForSession(ctx, wrong, "wrong login identity", tokenHash(cookies["alpha"])); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("write borrowed a sibling subject", err)
			}
			enrollment, _, err := service.IssueEnrollment(ctx, cookies["alpha"], "subject machine")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := service.IssueEnrollment(ctx, cookies["beta"], "denied machine"); !errors.Is(err, access.ErrDenied) {
				t.Fatal("linked subject inherited permission", err)
			}
			if _, err := authorization.New(ctx, identity.NewLocal(s, true), s, checker).EnrollmentDecision(ctx, enrollment); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("enrollment changed issuer", err)
			}
			if _, err := service.EnrollmentDecision(ctx, enrollment); err != nil {
				t.Fatal(err)
			}
			machine, _, err := s.Enroll(ctx, enrollment, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			user, err := external.Authenticate(ctx, cookies["alpha"])
			if err != nil {
				t.Fatal(err)
			}
			page, err := service.Discover(ctx, user, runner.Query{}, false)
			if err != nil || len(page.Items) != 1 {
				t.Fatal("subject discovery", err)
			}
			beta, err := external.Authenticate(ctx, cookies["beta"])
			if err != nil {
				t.Fatal(err)
			}
			page, err = service.Discover(ctx, beta, runner.Query{}, false)
			if err != nil || len(page.Items) != 0 {
				t.Fatal("discovery leaked between linked subjects", err)
			}
			if _, err := service.Client(ctx, cookies["beta"], machine.ID); !errors.Is(err, authorization.ErrNotFound) {
				t.Fatal("connection inherited linked identity", err)
			}
			grant, err := service.Client(ctx, cookies["alpha"], machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			pending, _, err := service.IssueEnrollment(ctx, cookies["alpha"], "pending after logout")
			if err != nil {
				t.Fatal(err)
			}
			proof := wire.ID() + wire.ID()
			const site = "https://subject.test/"
			request, err := s.BeginCLI(ctx, tokenHash(proof), site, provider.Namespace())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ConfirmCLI(ctx, request.ID, site, provider.Namespace(), request.Code, cookies["alpha"], owner.ID, true); err != nil {
				t.Fatal(err)
			}
			// The issuer shuts down; SQLite reopens or another PostgreSQL pool consumes.
			if backend == "sqlite" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
			}
			other := s
			if backend == "postgres" {
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			external, err = identity.NewExternal(other, public.Options{Provider: provider})
			if err != nil {
				t.Fatal(err)
			}
			receiver := authorization.New(ctx, external, other, checker)
			if _, _, err := receiver.Authorize(grant.Token()); err != nil {
				t.Fatal("durable ticket lost subject", err)
			}
			cli, err := other.ConsumeCLI(ctx, request.ID, site, provider.Namespace(), proof)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := external.AuthenticateCLI(ctx, cli.Token); err != nil || got.Subject != "alpha" {
				t.Fatal("CLI lost original subject", got, err)
			}
			cliGrant, err := receiver.ClientCLI(ctx, cli.Token, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := receiver.Authorize(cliGrant.Token()); err != nil {
				t.Fatal(err)
			}
			// Corrupted/mismatched snapshots must not borrow a sibling identity.
			record, err := other.CreateAccess(ctx, tokenHash("check-subject"), tokenHash(cookies["alpha"]), owner.ID, provider.Namespace(), machine.ID, time.Now().Add(time.Minute).Unix())
			if err != nil {
				t.Fatal(err)
			}
			record.Subject = "beta"
			if valid, err := other.CheckAccess(ctx, record, time.Now().Unix()); err != nil || valid {
				t.Fatal("access followed another subject", err)
			}
			if _, err := other.db.Exec(`UPDATE dune_sessions SET identity_subject='beta' WHERE hash=$1`, tokenHash(cli.Token)); err != nil {
				t.Fatal(err)
			}
			if _, err := external.AuthenticateCLI(ctx, cli.Token); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("CLI parent identity mismatch accepted", err)
			}
			if cliGrant.Valid() {
				t.Fatal("existing CLI access followed another subject")
			}
			if err := receiver.Revoke(ctx, cookies["beta"], machine.ID); !errors.Is(err, authorization.ErrNotFound) {
				t.Fatal("unbind inherited linked identity", err)
			}
			if err := external.Logout(ctx, cookies["alpha"]); err != nil {
				t.Fatal(err)
			}
			// Enrollment is an independent ten-minute capability, retaining its issuer.
			if _, err := receiver.EnrollmentDecision(ctx, pending); err != nil {
				t.Fatal("enrollment lost original issuer", err)
			}
			if got, err := other.EnrollmentUser(ctx, pending); err != nil || got.Subject != "alpha" || got.Namespace != provider.Namespace() {
				t.Fatal("enrollment changed subject", got, err)
			}
			checker.mu.Lock()
			defer checker.mu.Unlock()
			seen := map[string]bool{}
			for _, request := range checker.requests {
				if request.PrincipalID != owner.ID || request.Namespace != provider.Namespace() || (request.Subject != "alpha" && request.Subject != "beta") {
					t.Fatal("unverified policy identity", request.Scope)
				}
				seen[request.Operation] = true
			}
			for _, operation := range []string{"runner.create", "runner.connect", "runner.list", "runner.unbind"} {
				if !seen[operation] {
					t.Fatal("policy path not exercised", operation)
				}
			}
		})
	}
}

func TestSchemaSevenRequiresKnownLoginSubject(t *testing.T) {
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
			user, cookie, err := local.Register(ctx, "subject-upgrade@example.test", "upgrade-test-password")
			if err != nil {
				t.Fatal(err)
			}
			token, _, err := s.IssueEnrollment(ctx, user.ID, "preserved machine")
			if err != nil {
				t.Fatal(err)
			}
			machine, credential, err := s.Enroll(ctx, token, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			grant, err := authorization.NewLocal(ctx, local, s).Client(ctx, cookie, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			pending, _, err := s.IssueEnrollment(ctx, user.ID, "unknown issuer")
			if err != nil {
				t.Fatal(err)
			}
			for _, subject := range []string{"alpha", "beta"} {
				if _, err := s.ExternalLogin(ctx, "issuer", public.Subject{ID: subject}, wire.ID(), tokenHash(subject), time.Now().Add(time.Hour).Unix(), 32); err != nil {
					t.Fatal(err)
				}
			}
			// Remove only v8 columns to recreate old data with no per-login subject.
			err = s.transaction(ctx, func(tx *sql.Tx) error {
				for _, statement := range []string{
					`ALTER TABLE dune_enrollments DROP COLUMN identity_subject`,
					`ALTER TABLE dune_enrollments DROP COLUMN identity_namespace`,
					`ALTER TABLE dune_access_tickets DROP COLUMN identity_subject`,
					`ALTER TABLE dune_sessions DROP COLUMN identity_subject`,
					`UPDATE dune_schema SET version=7`,
				} {
					if _, err := tx.Exec(statement); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// A mid-upgrade failure rolls back preceding ALTERs and keeps all records.
			if _, err := s.db.Exec(`ALTER TABLE dune_enrollments ADD COLUMN identity_namespace TEXT`); err != nil {
				t.Fatal(err)
			}
			if err := s.migrate(ctx); err == nil {
				t.Fatal("partial schema accepted")
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_sessions WHERE identity_namespace='issuer'`).Scan(&count); err != nil || count != 2 {
				t.Fatal("failed upgrade revoked sessions", count, err)
			}
			if _, err := s.db.Exec(`ALTER TABLE dune_enrollments DROP COLUMN identity_namespace`); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			for _, subject := range []string{"alpha", "beta"} {
				if _, err := s.ReadSession(ctx, tokenHash(subject), time.Now().Unix()); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal("old subject inferred", err)
				}
			}
			if _, err := s.EnrollmentUser(ctx, pending); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("unknown enrollment issuer accepted", err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_external_identities`).Scan(&count); err != nil || count != 2 {
				t.Fatal("upgrade lost identity mappings", count, err)
			}
			local = identity.NewLocal(s, true)
			if got, err := local.Authenticate(ctx, cookie); err != nil || got.Subject != "" {
				t.Fatal("local login changed", err)
			}
			if _, _, err := authorization.NewLocal(ctx, local, s).Authorize(grant.Token()); err != nil {
				t.Fatal("local ticket lost", err)
			}
			if got, err := s.MachineCredential(ctx, credential); err != nil || got != machine.ID {
				t.Fatal("machine identity changed", err)
			}
		})
	}
}
