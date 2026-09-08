package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/fabric"
	public "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/storage"
)

func managedSpec() fabric.CreateRequest {
	return fabric.CreateRequest{Name: "Development", FabricID: "sandbox", TemplateID: "small", TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"cpu": json.RawMessage(`2`), "label": json.RawMessage(`"test"`)}}
}

func managedFixture(t *testing.T, backend string) (*Store, storage.Config, identity.User, string) {
	t.Helper()
	config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
	if backend == "postgres" {
		config, _, _ = postgresConfig(t)
	}
	s, err := Open(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	user, cookie, err := identity.NewLocal(s, true).Register(context.Background(), "managed@example.test", "managed-test-password")
	if err != nil {
		t.Fatal(err)
	}
	return s, config, user, tokenHash(cookie)
}

func TestManagedCreationFreezesOneIntent(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, hash := managedFixture(t, backend)
			other := s
			if backend == "postgres" {
				var err error
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			spec := managedSpec()
			key := wire.ID()
			first, err := s.CreateManaged(ctx, user, hash, key, spec)
			if err != nil {
				t.Fatal(err)
			}
			if first.Runner.ID == "" || first.Runner.Kind != "managed" || first.Runner.Binding != nil || first.Operation.RunnerID != first.Runner.ID || !first.Operation.Exclusive || first.Operation.Finished || first.Operation.Worker != "" || first.Operation.Revision != 0 {
				t.Fatal("intent granted execution or lost its business lock", first)
			}
			if first.Operation.PrincipalID != user.ID || first.Operation.FabricID != spec.FabricID || first.Operation.BindingRevision != 1 || first.Operation.Action != "create" {
				t.Fatal("creation association changed")
			}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Go(func() {
					got, err := other.CreateManaged(ctx, user, hash, key, spec)
					if err != nil || !reflect.DeepEqual(got, first) {
						t.Error("duplicate allocated another intent", err)
					}
				})
			}
			wg.Wait()
			changes := []func(*fabric.CreateRequest){func(s *fabric.CreateRequest) { s.Name = "different" }, func(s *fabric.CreateRequest) { s.FabricID = "another-account" }, func(s *fabric.CreateRequest) { s.TemplateID = "large" }, func(s *fabric.CreateRequest) { s.TemplateVersion = "v2" }, func(s *fabric.CreateRequest) { s.Parameters["cpu"] = json.RawMessage(`3`) }}
			for _, change := range changes {
				changed := managedSpec()
				change(&changed)
				if _, err := other.CreateManaged(ctx, user, hash, key, changed); !errors.Is(err, lifecycle.ErrIntentConflict) {
					t.Fatal("request key changed intent", err)
				}
			}
			spec.Parameters["cpu"][0] = '9'
			if got, err := s.ManagedCreation(ctx, user.ID, key); err != nil || !reflect.DeepEqual(got, first) {
				t.Fatal("caller mutated persisted request", err)
			}
			machines, err := s.Machines(ctx, user.ID)
			if err != nil || len(machines) != 0 {
				t.Fatal("creation minted a machine identity", err)
			}
			runners, err := s.Runners(ctx, user.ID)
			if err != nil || len(runners) != 1 || runners[0].Binding != nil {
				t.Fatal("creation is absent or executable", err)
			}
			if _, err := s.ManagedCreation(ctx, wire.ID(), key); !errors.Is(err, ErrNotFound) {
				t.Fatal("recovery crossed actor scope", err)
			}
			claimed, err := s.ClaimOperation(ctx, first.Operation.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RecordOperationUncertainty(ctx, claimed, "unknown"); err != nil {
				t.Fatal(err)
			}
			if backend == "sqlite" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			recovered, err := other.ManagedCreation(ctx, user.ID, key)
			if err != nil || recovered.Operation.Outcome != "unknown" || recovered.Operation.Revision != claimed.Revision || !recovered.Operation.Exclusive || !reflect.DeepEqual(recovered.Spec, first.Spec) {
				t.Fatal("recovery lost original snapshot or uncertainty", err)
			}
			if got, err := other.CreateManaged(ctx, user, hash, key, managedSpec()); err != nil || !reflect.DeepEqual(got, recovered) {
				t.Fatal("repeat reset uncertain operation", err)
			}
		})
	}
}

func TestManagedCreationRollbackAndSessionGate(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, hash := managedFixture(t, backend)
			before := snapshotRecords(t, s)
			fail, undo := `CREATE TRIGGER creation_failure BEFORE INSERT ON dune_managed_creations BEGIN SELECT RAISE(ABORT,'creation failure'); END`, `DROP TRIGGER creation_failure`
			if backend == "postgres" {
				fail = `ALTER TABLE dune_managed_creations ADD CONSTRAINT creation_failure CHECK(specification='impossible')`
				undo = `ALTER TABLE dune_managed_creations DROP CONSTRAINT creation_failure`
			}
			if _, err := s.db.Exec(fail); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateManaged(ctx, user, hash, wire.ID(), managedSpec()); !errors.Is(err, ErrConflict) {
				t.Fatal("late insertion failure not detected", err)
			}
			if !reflect.DeepEqual(before, snapshotRecords(t, s)) {
				t.Fatal("failed snapshot insertion left runner or operation")
			}
			if _, err := s.db.Exec(undo); err != nil {
				t.Fatal(err)
			}
			for _, value := range []struct {
				user identity.User
				hash string
			}{{user, "missing-session"}, {identity.User{ID: wire.ID()}, hash}, {identity.User{ID: user.ID, Namespace: "issuer", Subject: "another-subject"}, hash}} {
				if _, err := s.CreateManaged(ctx, value.user, value.hash, wire.ID(), managedSpec()); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal("wrong identity or session accepted", err)
				}
			}
			for _, query := range []string{`UPDATE dune_sessions SET expires_at=0`, `UPDATE dune_sessions SET kind='cli'`, `UPDATE dune_sessions SET auth_version=auth_version+1`, `UPDATE dune_sessions SET parent_hash=hash`} {
				if _, err := s.db.Exec(query); err != nil {
					t.Fatal(err)
				}
				if _, err := s.CreateManaged(ctx, user, hash, wire.ID(), managedSpec()); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal("invalid browser session accepted", query, err)
				}
				if _, err := s.db.Exec(`UPDATE dune_sessions SET expires_at=$1,kind='browser',parent_hash=NULL,auth_version=1`, time.Now().Add(time.Hour).Unix()); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range []string{"", string([]byte{0xff})} {
				if _, err := s.CreateManaged(ctx, user, hash, key, managedSpec()); !errors.Is(err, ErrInvalidArgument) {
					t.Fatal("invalid key accepted", err)
				}
			}
			created, err := s.CreateManaged(ctx, user, hash, "original", managedSpec())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteSession(ctx, hash); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateManaged(ctx, user, hash, "original", managedSpec()); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("idempotency bypassed logout", err)
			}
			if got, err := s.ManagedCreation(ctx, user.ID, "original"); err != nil || got.Operation.ID != created.Operation.ID {
				t.Fatal("logout discarded background operation", err)
			}
		})
	}
}

func TestManagedCreationKeepsAuthenticatedSubject(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, owner, _ := managedFixture(t, backend)
			users := make([]identity.User, 2)
			hashes := make([]string, 2)
			for i, subject := range []string{"alpha", "beta"} {
				if _, err := s.LinkIdentity(ctx, public.LinkRequest{RequestID: wire.ID(), Actor: "test-admin", PrincipalID: owner.ID, Namespace: "issuer", Subject: subject, Reason: "verified test identity"}); err != nil {
					t.Fatal(err)
				}
				hashes[i] = tokenHash(wire.ID())
				var err error
				users[i], err = s.ExternalLogin(ctx, "issuer", public.Subject{ID: subject}, wire.ID(), hashes[i], time.Now().Add(time.Hour).Unix(), 32)
				if err != nil {
					t.Fatal(err)
				}
			}
			// Linking the second subject revokes all preceding sessions. Both fresh
			// logins still refer to one principal; only the verified subject differs.
			var err error
			users[0], err = s.ExternalLogin(ctx, "issuer", public.Subject{ID: "alpha"}, wire.ID(), hashes[0], time.Now().Add(time.Hour).Unix(), 32)
			if err != nil {
				t.Fatal(err)
			}
			first, err := s.CreateManaged(ctx, users[0], hashes[0], "same-key", managedSpec())
			if err != nil {
				t.Fatal(err)
			}
			if first.Operation.Subject != "alpha" || first.Operation.Namespace != "issuer" {
				t.Fatal("authenticated subject missing")
			}
			if _, err := s.CreateManaged(ctx, users[1], hashes[1], "same-key", managedSpec()); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("another linked subject reused original intent", err)
			}
			if _, err := s.CreateManaged(ctx, users[0], hashes[1], wire.ID(), managedSpec()); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("subject replaced on live session", err)
			}
		})
	}
}

func TestManagedAndAttachedShareRunnerLimit(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, hash := managedFixture(t, backend)
			if err := s.transaction(ctx, func(tx *sql.Tx) error {
				for i := 0; i < 31; i++ {
					if _, err := tx.Exec(`INSERT INTO dune_runners(id,owner_id,name,kind,fabric_id,binding_revision,created_at) VALUES($1,$2,'existing','attached','attached',1,0)`, wire.ID(), user.ID); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			enrollment, _, err := s.IssueEnrollment(ctx, user.ID, "last slot")
			if err != nil {
				t.Fatal(err)
			}
			other := s
			if backend == "postgres" {
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			var wins atomic.Int32
			var wg sync.WaitGroup
			wg.Go(func() {
				_, _, err := s.Enroll(ctx, enrollment, "linux", "amd64")
				if err == nil {
					wins.Add(1)
				} else if !errors.Is(err, ErrInvalidArgument) {
					t.Error(err)
				}
			})
			for range 4 {
				wg.Go(func() {
					_, err := other.CreateManaged(ctx, user, hash, wire.ID(), managedSpec())
					if err == nil {
						wins.Add(1)
					} else if !errors.Is(err, ErrInvalidArgument) {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatal("concurrent creation exceeded shared limit", wins.Load())
			}
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM dune_runners WHERE owner_id=$1`, user.ID).Scan(&count); err != nil || count != 32 {
				t.Fatal("incorrect runner count", count, err)
			}
		})
	}
}

func TestManagedCreationLostCommitCanBeReconciled(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, hash := managedFixture(t, backend)
			faulty, commits := lostCommitStore(t, config)
			key := wire.ID()
			if got, err := faulty.CreateManaged(ctx, user, hash, key, managedSpec()); !errors.Is(err, ErrCommitUnknown) || got.Operation.ID != "" || got.Runner.ID != "" || commits.Load() != 1 {
				t.Fatal("uncertain commit returned success or was replayed", err, commits.Load())
			}
			got, err := s.ManagedCreation(ctx, user.ID, key)
			if err != nil || got.Operation.ID == "" || !got.Operation.Exclusive || got.Runner.Binding != nil {
				t.Fatal("cannot reconcile original creation", err)
			}
			again, err := s.CreateManaged(ctx, user, hash, key, managedSpec())
			if err != nil || !reflect.DeepEqual(got, again) {
				t.Fatal("reconciliation allocated another runner", err)
			}
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM dune_runners WHERE owner_id=$1`, user.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal("unknown commit was duplicated", count, err)
			}
		})
	}
}
