package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
)

func openAttachedManagerStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}, OpenOptions{ExternalIdentity: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func issuePendingAttached(t *testing.T, store *Store, ownerID, runnerName string) (identity.User, runner.Runner, string) {
	t.Helper()
	creator := identity.User{ID: "email:creator@example.test", Kind: "email", Namespace: "sanddance", Subject: "creator@example.test"}
	logical, token, _, err := store.IssueTenantEnrollment(context.Background(), creator, ownerID, runnerName, "host-session")
	if err != nil {
		t.Fatal(err)
	}
	return creator, logical, token
}

func TestAttachedRunnerFactsAndPendingCancellation(t *testing.T) {
	store := openAttachedManagerStore(t)
	creator, logical, _ := issuePendingAttached(t, store, "tenant-1", "Pending Devbox")
	facts, err := store.AttachedRunner(context.Background(), "tenant-1", logical.ID)
	if err != nil || facts.OwnerID != "tenant-1" || facts.Runner.ID != logical.ID || facts.Runner.Binding != nil || facts.CreatedBy.ID != creator.ID || facts.CreatedBy.Namespace != creator.Namespace || facts.CreatedBy.Subject != creator.Subject {
		t.Fatal("pending Attached facts changed", facts, err)
	}
	if active, err := store.HasActiveAttached(context.Background(), "tenant-1"); err != nil || !active {
		t.Fatal("active Attached runner not reported", active, err)
	}
	if err := store.CancelAttachedEnrollment(context.Background(), "tenant-1", logical.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachedRunner(context.Background(), "tenant-1", logical.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cancelled Attached runner remains visible", err)
	}
	if active, err := store.HasActiveAttached(context.Background(), "tenant-1"); err != nil || active {
		t.Fatal("cancelled Attached runner still blocks owner deletion", active, err)
	}
	var enrollments int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM dune_enrollments WHERE runner_id=$1`, logical.ID).Scan(&enrollments); err != nil || enrollments != 0 {
		t.Fatal("cancelled enrollment token remains", enrollments, err)
	}
}

func TestAttachedCancellationNeverDetachesBoundRunner(t *testing.T) {
	store := openAttachedManagerStore(t)
	_, logical, token := issuePendingAttached(t, store, "tenant-1", "Bound Devbox")
	machine, credential, err := store.Enroll(context.Background(), token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CancelAttachedEnrollment(context.Background(), "tenant-1", logical.ID); !errors.Is(err, runner.ErrBindingChanged) {
		t.Fatal("bound Runner cancellation did not report binding change", err)
	}
	facts, err := store.AttachedRunner(context.Background(), "tenant-1", logical.ID)
	if err != nil || facts.Runner.Binding == nil || facts.Runner.Binding.MachineID != machine.ID {
		t.Fatal("bound Runner was changed", facts, err)
	}
	if current, err := store.MachineCredential(context.Background(), credential); err != nil || current != machine.ID {
		t.Fatal("bound machine credential was revoked", current, err)
	}
}

func TestAttachedCancellationRollsBackTokenDeletion(t *testing.T) {
	store := openAttachedManagerStore(t)
	_, logical, token := issuePendingAttached(t, store, "tenant-1", "Rollback Devbox")
	if _, err := store.db.Exec(`CREATE TRIGGER fail_attached_cancel BEFORE UPDATE ON dune_runners BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelAttachedEnrollment(context.Background(), "tenant-1", logical.ID); err == nil {
		t.Fatal("expected injected cancellation failure")
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_attached_cancel`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Enroll(context.Background(), token, "linux", "arm64"); err != nil {
		t.Fatal("failed cancellation did not preserve enrollment", err)
	}
}

func TestAttachedEnrollmentAndCancellationAreAtomic(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			store, err := Open(context.Background(), config, OpenOptions{ExternalIdentity: true})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			for i := range 12 {
				_, logical, token := issuePendingAttached(t, store, "tenant-race", "Race Devbox")
				start := make(chan struct{})
				var enrollErr, cancelErr error
				var machine Machine
				var credential string
				var wg sync.WaitGroup
				wg.Go(func() {
					<-start
					machine, credential, enrollErr = store.Enroll(context.Background(), token, "linux", "amd64")
				})
				wg.Go(func() {
					<-start
					cancelErr = store.CancelAttachedEnrollment(context.Background(), "tenant-race", logical.ID)
				})
				close(start)
				wg.Wait()
				switch {
				case enrollErr == nil:
					if !errors.Is(cancelErr, runner.ErrBindingChanged) {
						t.Fatalf("iteration %d: enrollment won without binding conflict: %v", i, cancelErr)
					}
					if current, err := store.MachineCredential(context.Background(), credential); err != nil || current != machine.ID {
						t.Fatalf("iteration %d: cancellation changed winning binding: %q %v", i, current, err)
					}
				case cancelErr == nil:
					if !errors.Is(enrollErr, identity.ErrUnauthorized) {
						t.Fatalf("iteration %d: cancellation won but token remained usable: %v", i, enrollErr)
					}
					if _, err := store.AttachedRunner(context.Background(), "tenant-race", logical.ID); !errors.Is(err, ErrNotFound) {
						t.Fatalf("iteration %d: cancelled Runner remains active: %v", i, err)
					}
				default:
					t.Fatalf("iteration %d: neither operation committed: enroll=%v cancel=%v", i, enrollErr, cancelErr)
				}
			}
		})
	}
}
