package metadata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
)

func operationFixture(t *testing.T, s *Store) lifecycle.Intent {
	t.Helper()
	ctx := context.Background()
	user, _, err := identity.NewLocal(s, true).Register(ctx, wire.ID()+"@operations.test", "operations-test-password")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.IssueEnrollment(ctx, user.ID, "coordination fixture")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := s.Enroll(ctx, token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("canonical test request"))
	// Existing Attached records exercise SQL coordination only. No provider call
	// or Managed product entry point is being simulated by this fixture.
	return lifecycle.Intent{ID: wire.ID(), RequestKey: wire.ID(), Digest: hex.EncodeToString(digest[:]), PrincipalID: user.ID, RunnerID: machine.RunnerID, FabricID: "attached", BindingRevision: 1, Action: "renew"}
}

func TestOperationClaimsAndBusinessMutex(t *testing.T) {
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
			intent := operationFixture(t, s)
			other := s
			if backend == "postgres" {
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			var wg sync.WaitGroup
			var ids sync.Map
			for n := range 8 {
				wg.Go(func() {
					candidate := intent
					candidate.ID = wire.ID()
					db := s
					if n%2 != 0 {
						db = other
					}
					op, err := db.BeginOperation(ctx, candidate)
					if err != nil {
						t.Error(err)
						return
					}
					ids.Store(op.ID, true)
				})
			}
			wg.Wait()
			count := 0
			ids.Range(func(key, value any) bool { intent.ID = key.(string); count++; return true })
			if count != 1 {
				t.Fatal("request replay created multiple operations", count)
			}
			changed := intent
			changed.Digest = hex.EncodeToString(make([]byte, 32))
			if _, err := s.BeginOperation(ctx, changed); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("request key changed intent", err)
			}
			conflicting := intent
			conflicting.ID = wire.ID()
			conflicting.RequestKey = wire.ID()
			conflicting.Action = "destroy"
			if _, err := other.BeginOperation(ctx, conflicting); !errors.Is(err, lifecycle.ErrBusy) {
				t.Fatal("runner accepted conflicting work", err)
			}
			var wins atomic.Int32
			var lease lifecycle.Operation
			var mu sync.Mutex
			for n := range 8 {
				wg.Go(func() {
					db := s
					if n%2 != 0 {
						db = other
					}
					op, err := db.ClaimOperation(ctx, intent.ID, wire.ID(), time.Minute)
					if err == nil {
						wins.Add(1)
						mu.Lock()
						lease = op
						mu.Unlock()
					} else if !errors.Is(err, lifecycle.ErrBusy) {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if wins.Load() != 1 || lease.Revision != 1 || lease.Until.IsZero() {
				t.Fatal("execution claim not exclusive", wins.Load(), lease.Revision)
			}
			if err := s.RecordOperationUncertainty(ctx, lease, "unknown"); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishOperation(ctx, lease, "unknown"); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal("unknown released business lock", err)
			}
			forged := lease
			forged.Subject = "someone-else"
			if err := s.FinishOperation(ctx, forged, "succeeded"); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("mutable snapshot changed identity", err)
			}
			if _, err := s.RenewOperationLease(ctx, lease, time.Second); err != nil {
				t.Fatal(err)
			}
			current, err := s.Operation(ctx, intent.ID)
			if err != nil || current.Until.Before(lease.Until) {
				t.Fatal("refresh shortened lease", err)
			}
			// Expiry is a database fact. No test sleep or caller-provided clock grants it.
			if _, err := s.db.Exec(`UPDATE dune_operations SET lease_until=1 WHERE id=$1`, intent.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RenewOperationLease(ctx, lease, time.Minute); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("expired claim revived", err)
			}
			if err := s.FinishOperation(ctx, lease, "failed"); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("expired claim committed", err)
			}
			if _, err := s.BeginOperation(ctx, conflicting); !errors.Is(err, lifecycle.ErrBusy) {
				t.Fatal("lease expiry released business mutex", err)
			}
			if backend == "sqlite" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				other = s
			}
			replacement, err := other.ClaimOperation(ctx, intent.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if replacement.Revision != 2 || replacement.Outcome != "unknown" || replacement.Finished {
				t.Fatal("takeover forgot uncertain work", replacement)
			}
			if _, err := other.RenewOperationLease(ctx, lease, time.Minute); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("old worker renewed new lease", err)
			}
			if err := other.RecordOperationUncertainty(ctx, lease, "timed_out"); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("old worker overwrote uncertainty", err)
			}
			if err := other.FinishOperation(ctx, lease, "succeeded"); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("old worker overwrote replacement", err)
			}
			if err := other.RecordOperationUncertainty(ctx, replacement, "timed_out"); err != nil {
				t.Fatal(err)
			}
			if got, err := other.Operation(ctx, intent.ID); err != nil || got.Outcome != "timed_out" || got.Finished {
				t.Fatal("timeout became terminal", got, err)
			}
			// Following already accepted work is independent of the browser and enabled
			// state. Maintenance policy, not this coordination store, decides its scope.
			if err := other.SetPrincipalEnabled(ctx, intent.PrincipalID, false); err != nil {
				t.Fatal(err)
			}
			if err := other.FinishOperation(ctx, replacement, "failed"); err != nil {
				t.Fatal(err)
			}
			done, err := other.Operation(ctx, intent.ID)
			if err != nil || !done.Finished || done.Outcome != "failed" || done.Worker != "" || !done.Until.IsZero() {
				t.Fatal("terminal result incomplete", done, err)
			}
			replay, err := other.BeginOperation(ctx, intent)
			if err != nil || replay.ID != done.ID || !replay.Finished {
				t.Fatal("completed request replayed", replay, err)
			}
			next, err := other.BeginOperation(ctx, conflicting)
			if err != nil {
				t.Fatal("confirmed completion retained mutex", err)
			}
			active, err := other.ClaimOperation(ctx, next.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := other.db.Exec(`UPDATE dune_runners SET binding_revision=binding_revision+1 WHERE id=$1`, intent.RunnerID); err != nil {
				t.Fatal(err)
			}
			if err := other.FinishOperation(ctx, active, "succeeded"); !errors.Is(err, runner.ErrBindingChanged) {
				t.Fatal("operation followed replacement binding", err)
			}
			if _, err := other.RenewOperationLease(ctx, active, time.Minute); !errors.Is(err, runner.ErrBindingChanged) {
				t.Fatal("changed binding refreshed lease", err)
			}
		})
	}
}

func TestOperationIntentRollbackAndValidation(t *testing.T) {
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
			intent := operationFixture(t, s)
			fail, restore := `CREATE TRIGGER operation_failure BEFORE INSERT ON dune_operations BEGIN SELECT RAISE(ABORT,'test insertion failure'); END`, `DROP TRIGGER operation_failure`
			if backend == "postgres" {
				fail = `ALTER TABLE dune_operations ADD CONSTRAINT operation_failure CHECK(action='impossible')`
				restore = `ALTER TABLE dune_operations DROP CONSTRAINT operation_failure`
			}
			if _, err := s.db.Exec(fail); err != nil {
				t.Fatal(err)
			}
			if _, err := s.BeginOperation(ctx, intent); err == nil {
				t.Fatal("injected failure accepted")
			}
			if _, err := s.db.Exec(restore); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Operation(ctx, intent.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("partial operation persisted", err)
			}
			for _, modify := range []func(*lifecycle.Intent){
				func(i *lifecycle.Intent) { i.Digest = "unbounded raw request" }, func(i *lifecycle.Intent) { i.RequestKey = "\n" }, func(i *lifecycle.Intent) { i.Namespace = "issuer" }, func(i *lifecycle.Intent) { i.BindingRevision = 0 }, func(i *lifecycle.Intent) { i.Action = "shell" },
			} {
				bad := intent
				modify(&bad)
				if _, err := s.BeginOperation(ctx, bad); !errors.Is(err, ErrInvalidArgument) {
					t.Fatal("invalid intent persisted", err)
				}
			}
			if _, err := s.BeginOperation(ctx, intent); err != nil {
				t.Fatal("failure left business lock", err)
			}
			for _, ttl := range []time.Duration{0, time.Millisecond, -time.Second, time.Minute + 1} {
				if _, err := s.ClaimOperation(ctx, intent.ID, wire.ID(), ttl); !errors.Is(err, ErrInvalidArgument) {
					t.Fatal("invalid lease duration accepted", err)
				}
			}
			if _, err := s.ClaimOperation(ctx, intent.ID, "reusable-instance-name", time.Minute); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal("display name used as incarnation", err)
			}
			if _, err := s.db.Exec(`UPDATE dune_operations SET execution_revision=9223372036854775807 WHERE id=$1`, intent.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ClaimOperation(ctx, intent.ID, wire.ID(), time.Minute); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("revision overflow accepted", err)
			}
		})
	}
}

func TestOperationWriteRechecksDatabaseLease(t *testing.T) {
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
			intent := operationFixture(t, s)
			if _, err := s.BeginOperation(ctx, intent); err != nil {
				t.Fatal(err)
			}
			var claimed lifecycle.Operation
			for _, change := range []string{"outcome", "mutex"} {
				claimed, err = s.ClaimOperation(ctx, intent.ID, wire.ID(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				err = s.withOperation(ctx, intent.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
					if !ownsOperation(current, claimed, now) {
						t.Fatal("claim expired before pause")
					}
					// Pause after reading the valid claim, while retaining the SQL row lock.
					// The final UPDATE must check database time again, not trust the old read.
					time.Sleep(1100 * time.Millisecond)
					if change == "mutex" {
						return s.updateOperationMutex(ctx, tx, claimed, false)
					}
					return s.updateOperationOutcome(ctx, tx, claimed, "succeeded", true)
				})
				if !errors.Is(err, lifecycle.ErrLeaseLost) {
					t.Fatal("paused worker committed after expiry", err)
				}
				if op, err := s.Operation(ctx, intent.ID); err != nil || op.Finished || !op.Exclusive {
					t.Fatal("late write changed outcome", err)
				}
			}
			if next, err := s.ClaimOperation(ctx, intent.ID, wire.ID(), time.Minute); err != nil || next.Revision != 3 {
				t.Fatal("expired work could not be taken over", err)
			}
		})
	}
}

func TestWaitingOperationAllowsSerializedMaintenance(t *testing.T) {
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
			intent := operationFixture(t, s)
			intent.Action = "create"
			if _, err := s.BeginOperation(ctx, intent); err != nil {
				t.Fatal(err)
			}
			create, err := s.ClaimOperation(ctx, intent.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			// The executor has confirmed create/bootstrap ended and now observes the
			// first connection. Completion of the whole create workflow is still pending.
			if err := s.ReleaseOperationMutex(ctx, create); err != nil {
				t.Fatal(err)
			}
			waiting, err := s.Operation(ctx, intent.ID)
			if err != nil || waiting.Finished || waiting.Exclusive {
				t.Fatal("readiness wait retained mutation lock", err)
			}
			renewal := intent
			renewal.ID = wire.ID()
			renewal.RequestKey = wire.ID()
			renewal.Action = "renew"
			if _, err := s.BeginOperation(ctx, renewal); err != nil {
				t.Fatal("readiness wait prevented renewal", err)
			}
			renew, err := s.ClaimOperation(ctx, renewal.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AcquireOperationMutex(ctx, create); !errors.Is(err, lifecycle.ErrBusy) {
				t.Fatal("create stole maintenance mutex", err)
			}
			if err := s.RecordOperationUncertainty(ctx, renew, "unknown"); err != nil {
				t.Fatal(err)
			}
			if err := s.ReleaseOperationMutex(ctx, renew); !errors.Is(err, lifecycle.ErrBusy) {
				t.Fatal("unknown provider call unlocked", err)
			}
			// An independent readiness observation can complete without releasing the
			// renewal operation's lock or implying its unknown provider call has ended.
			if err := s.FinishOperation(ctx, create, "succeeded"); err != nil {
				t.Fatal(err)
			}
			candidate := renewal
			candidate.ID = wire.ID()
			candidate.RequestKey = wire.ID()
			if _, err := s.BeginOperation(ctx, candidate); !errors.Is(err, lifecycle.ErrBusy) {
				t.Fatal("another workflow released unknown mutation", err)
			}
			if err := s.FinishOperation(ctx, renew, "failed"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.BeginOperation(ctx, candidate); err != nil {
				t.Fatal(err)
			}
			last, err := s.ClaimOperation(ctx, candidate.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ReleaseOperationMutex(ctx, last); err != nil {
				t.Fatal(err)
			}
			if err := s.AcquireOperationMutex(ctx, last); err != nil {
				t.Fatal("idle workflow cannot reacquire", err)
			}
		})
	}
}
