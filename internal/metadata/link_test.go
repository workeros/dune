package metadata

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	public "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/storage"
)

func TestIdentityLinkTransactions(t *testing.T) {
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
			other := s
			if backend == "postgres" {
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			local := identity.NewLocal(s, true)
			user, cookie, err := local.Register(ctx, "link@example.test", "link-test-password")
			if err != nil {
				t.Fatal(err)
			}
			second, otherCookie, err := local.Register(ctx, "other@example.test", "other-test-password")
			if err != nil {
				t.Fatal(err)
			}
			enrollment, _, err := s.IssueEnrollment(ctx, user.ID, "existing machine")
			if err != nil {
				t.Fatal(err)
			}
			machine, credential, err := s.Enroll(ctx, enrollment, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			pending, _, err := s.IssueEnrollment(ctx, user.ID, "pending machine")
			if err != nil {
				t.Fatal(err)
			}
			r := public.LinkRequest{RequestID: wire.ID(), Actor: "admin:review", PrincipalID: user.ID, Namespace: "https://identity.example.test", Subject: "stable-subject", Reason: "verified ownership; approval review-42"}
			before := snapshotRecords(t, s)
			for _, change := range []func(*public.LinkRequest){
				func(r *public.LinkRequest) { r.Actor = "" },
				func(r *public.LinkRequest) { r.Reason = "" },
				func(r *public.LinkRequest) { r.Subject = "wrong\nsubject" },
				func(r *public.LinkRequest) { r.Namespace = "" },
			} {
				invalid := r
				change(&invalid)
				if _, err := s.LinkIdentity(ctx, invalid); err == nil {
					t.Fatal("incomplete or ambiguous link accepted")
				}
			}
			if !reflect.DeepEqual(before, snapshotRecords(t, s)) {
				t.Fatal("invalid request changed metadata")
			}
			fail, restore := `CREATE TRIGGER audit_failure BEFORE INSERT ON dune_identity_links BEGIN SELECT RAISE(ABORT,'audit failure'); END`, `DROP TRIGGER audit_failure`
			if backend == "postgres" {
				fail = `ALTER TABLE dune_identity_links ADD CONSTRAINT audit_failure CHECK(actor='impossible')`
				restore = `ALTER TABLE dune_identity_links DROP CONSTRAINT audit_failure`
			}
			if _, err := s.db.Exec(fail); err != nil {
				t.Fatal(err)
			}
			if _, err := s.LinkIdentity(ctx, r); err == nil {
				t.Fatal("failed audit accepted")
			}
			if !reflect.DeepEqual(before, snapshotRecords(t, s)) {
				t.Fatal("audit failure retained a partial link or revocation")
			}
			if _, err := s.db.Exec(restore); err != nil {
				t.Fatal(err)
			}
			if _, err := s.IdentityLink(ctx, r.RequestID); !errors.Is(err, public.ErrLinkNotFound) {
				t.Fatal("failed link has audit record", err)
			}
			var wg sync.WaitGroup
			for i := range 8 {
				store := s
				if i%2 == 1 {
					store = other
				}
				wg.Go(func() {
					record, err := store.LinkIdentity(ctx, r)
					if err != nil || record.LinkRequest != r || record.CreatedAt.IsZero() {
						t.Error("concurrent identical link failed", err)
					}
				})
			}
			wg.Wait()
			var version, count int
			if err := s.db.QueryRow(`SELECT auth_version FROM dune_principals WHERE id=$1`, user.ID).Scan(&version); err != nil || version != 2 {
				t.Fatal("link revoked more than once", err, version)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM dune_identity_links`).Scan(&count); err != nil || count != 1 {
				t.Fatal("audit is not unique", err, count)
			}
			if _, err := local.Authenticate(ctx, cookie); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("old session survived link", err)
			}
			if _, _, err := s.Enroll(ctx, pending, "linux", "amd64"); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("pending enrollment survived link", err)
			}
			if _, err := local.Authenticate(ctx, otherCookie); err != nil {
				t.Fatal("link affected another user", err)
			}
			if id, err := s.MachineCredential(ctx, credential); err != nil || id != machine.ID {
				t.Fatal("machine identity changed", err)
			}
			if owns, err := s.Owns(ctx, user.ID, machine.ID); err != nil || !owns {
				t.Fatal("Runner ownership changed", err)
			}
			_, fresh, err := local.Login(ctx, user.Email, "link-test-password")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := other.LinkIdentity(ctx, r); err != nil {
				t.Fatal(err)
			}
			if _, err := local.Authenticate(ctx, fresh); err != nil {
				t.Fatal("replay revoked fresh session", err)
			}
			before = snapshotRecords(t, s)
			for _, changed := range []public.LinkRequest{
				{RequestID: r.RequestID, Actor: r.Actor, PrincipalID: r.PrincipalID, Namespace: r.Namespace, Subject: "different", Reason: r.Reason},
				{RequestID: wire.ID(), Actor: r.Actor, PrincipalID: second.ID, Namespace: r.Namespace, Subject: r.Subject, Reason: r.Reason},
			} {
				if _, err := other.LinkIdentity(ctx, changed); !errors.Is(err, public.ErrLinkConflict) {
					t.Fatal("identity/request conflict accepted", err)
				}
			}
			if !reflect.DeepEqual(before, snapshotRecords(t, s)) {
				t.Fatal("conflict changed metadata")
			}
			linked, err := other.ExternalLogin(ctx, r.Namespace, public.Subject{ID: r.Subject, Email: "changed@example.test"}, wire.ID(), tokenHash(wire.ID()), time.Now().Add(time.Hour).Unix(), 32)
			if err != nil || linked.ID != user.ID {
				t.Fatal("external login lost linked identity", err)
			}
			for i := range 4 {
				candidate := r
				candidate.RequestID, candidate.Subject = wire.ID(), fmt.Sprintf("racing-%d", i)
				var loginUser identity.User
				var loginErr, linkErr error
				wg.Go(func() {
					loginUser, loginErr = other.ExternalLogin(ctx, candidate.Namespace, public.Subject{ID: candidate.Subject}, wire.ID(), tokenHash(wire.ID()), time.Now().Add(time.Hour).Unix(), 32)
				})
				wg.Go(func() { _, linkErr = s.LinkIdentity(ctx, candidate) })
				wg.Wait()
				if loginErr != nil {
					t.Fatal(loginErr)
				}
				if linkErr == nil && loginUser.ID != user.ID {
					t.Fatal("successful link raced into different principal")
				}
				if linkErr != nil && (!errors.Is(linkErr, public.ErrLinkConflict) || loginUser.ID == user.ID) {
					t.Fatal("unexpected first-login conflict", linkErr)
				}
			}
			if err := s.SetPrincipalEnabled(ctx, user.ID, false); err != nil {
				t.Fatal(err)
			}
			disabled := r
			disabled.RequestID, disabled.Subject = wire.ID(), "disabled"
			if _, err := s.LinkIdentity(ctx, disabled); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("disabled principal linked", err)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := s.LinkIdentity(cancelled, r); !errors.Is(err, context.Canceled) {
				t.Fatal("cancelled link accepted", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			if record, err := s.IdentityLink(ctx, r.RequestID); err != nil || record.LinkRequest != r {
				t.Fatal("audit lost on reopen", err)
			}
		})
	}
}
