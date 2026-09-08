package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/storage"
)

func managedMachineAwaitingOnline(t *testing.T, s *Store, user identity.User, sessionHash string) (lifecycle.Creation, lifecycle.Operation, Machine) {
	t.Helper()
	ctx := context.Background()
	created, claimed := confirmedManagedCreate(t, s, user, sessionHash)
	grant, dispatch, err := s.BeginManagedBootstrap(ctx, claimed, tokenHash("online bootstrap inputs"), time.Minute)
	if err != nil || !dispatch {
		t.Fatal("bootstrap was not reserved", dispatch, err)
	}
	machine, _, err := s.Enroll(ctx, grant.Token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	return created, claimed, machine
}

func onlineBinding(machine Machine) api.Binding {
	return api.Binding{Version: api.Version, Target: machine.ID, Incarnation: wire.ID(), Generation: 1, Capabilities: []string{"runtime.list"}, Limits: map[string]int{"streams": 8}}
}

func publishOnlineRoute(t *testing.T, s *Store, binding api.Binding) (gateway.Directory, gateway.Route) {
	t.Helper()
	ctx := context.Background()
	recovery := wire.ID()
	directory, err := s.ConnectionDirectory(ctx, recovery)
	if err != nil {
		t.Fatal(err)
	}
	claim := gateway.RouteClaim{Target: binding.Target, RecoveryGeneration: recovery, OwnerBootID: wire.ID(), OwnerAddress: "https://managed-owner.test/peer", Binding: binding}
	lease, err := directory.Acquire(ctx, claim, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Publish(ctx, lease.Route); err != nil {
		t.Fatal(err)
	}
	return directory, lease.Route
}

func completeBootstrap(t *testing.T, s *Store, claimed lifecycle.Operation) {
	t.Helper()
	action, err := s.ProviderAction(context.Background(), claimed.ID, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProviderAction(context.Background(), claimed, action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: action.ResourceRef}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedCreateFinishesOnlyAfterFirstUsableConnection(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, sessionHash := managedFixture(t, backend)
			created, claimed, machine := managedMachineAwaitingOnline(t, s, user, sessionHash)
			binding := onlineBinding(machine)
			if err := s.ConfirmMachineOnline(ctx, binding); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("connection finished create before bootstrap evidence", err)
			}
			completeBootstrap(t, s, claimed)
			if op, err := s.Operation(ctx, claimed.ID); err != nil || op.Finished || op.Exclusive || op.Outcome != "" {
				t.Fatal("bootstrap did not enter connection wait", op, err)
			}

			var directory gateway.Directory
			var route gateway.Route
			if backend == "postgres" {
				directory, route = publishOnlineRoute(t, s, binding)
				if err := s.ConfirmMachineOnline(ctx, binding); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal("cluster connection without an ownership term confirmed create", err)
				}
				binding.RouteRecovery, binding.RouteEpoch = route.RecoveryGeneration, route.Epoch
				different := binding
				different.Capabilities = []string{"runtime.get"}
				if err := s.ConfirmMachineOnline(ctx, different); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal("different execution binding confirmed create", err)
				}
				stale := binding
				stale.RouteEpoch++
				if err := s.ConfirmMachineOnline(ctx, stale); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal("different route term confirmed create", err)
				}
				if err := directory.Release(ctx, route); err != nil {
					t.Fatal(err)
				}
				if err := s.ConfirmMachineOnline(ctx, binding); !errors.Is(err, identity.ErrUnauthorized) {
					t.Fatal("released route confirmed create", err)
				}
				found, err := directory.Resolve(ctx, binding.Target)
				if err != nil {
					t.Fatal(err)
				}
				replacement, err := directory.Acquire(ctx, route.RouteClaim, found.Epoch)
				if err != nil {
					t.Fatal(err)
				}
				route = replacement.Route
				if err := directory.Publish(ctx, route); err != nil {
					t.Fatal(err)
				}
				binding.RouteEpoch = route.Epoch
			}

			if _, err := s.db.ExecContext(ctx, `UPDATE dune_managed_resources SET expires_at=`+s.databaseClock()+`-1 WHERE runner_id=$1`, created.Runner.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.ConfirmMachineOnline(ctx, binding); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("expired resource confirmed create", err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE dune_managed_resources SET expires_at=`+s.databaseClock()+`+60000 WHERE runner_id=$1`, created.Runner.ID); err != nil {
				t.Fatal(err)
			}

			interrupted, commits := lostCommitStore(t, config)
			if err := interrupted.ConfirmMachineOnline(ctx, binding); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 1 {
				t.Fatal("lost confirmation acknowledgement was hidden or replayed", commits.Load(), err)
			}
			op, err := s.Operation(ctx, claimed.ID)
			if err != nil || !op.Finished || op.FinishedAt.IsZero() || op.FinishedAt.Before(op.CreatedAt) || op.Outcome != "succeeded" || op.Exclusive || op.Worker != "" || !op.Until.IsZero() {
				t.Fatal("usable connection did not finish create", op, err)
			}
			if err := s.ConfirmMachineOnline(ctx, binding); err != nil {
				t.Fatal("reconnect could not reconcile committed confirmation", err)
			}
		})
	}
}

func TestAttachedMachineOnlineNeedsNoManagedLifecycle(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _, err := identity.NewLocal(s, true).Register(ctx, "attached-online@example.test", "attached-online-password")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.IssueEnrollment(ctx, user.ID, "attached online")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := s.Enroll(ctx, token, "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmMachineOnline(ctx, onlineBinding(machine)); err != nil {
		t.Fatal(err)
	}
}
