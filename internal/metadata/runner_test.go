package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
)

func TestRunnerBindingSnapshot(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			cfg := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				cfg, _, _ = postgresConfig(t)
			}
			s, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			local := identity.NewLocal(s, true)
			user, cookie, err := local.Register(ctx, "runner@example.test", "runner-test-password")
			if err != nil {
				t.Fatal(err)
			}
			token, _, err := s.IssueEnrollment(ctx, user.ID, "Selected environment")
			if err != nil {
				t.Fatal(err)
			}
			machine, _, err := s.Enroll(ctx, token, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			if machine.ID == machine.RunnerID {
				t.Fatal("new machine reused logical identity")
			}
			rows, err := s.Runners(ctx, user.ID)
			if err != nil || len(rows) != 1 {
				t.Fatal("runner discovery", err)
			}
			selected := rows[0]
			if selected.ID != machine.RunnerID || selected.Binding == nil || selected.Binding.MachineID != machine.ID {
				t.Fatal("incorrect runner binding")
			}
			stranger, _, err := local.Register(ctx, "stranger@example.test", "stranger-test-password")
			if err != nil {
				t.Fatal(err)
			}
			if rows, err := s.Runners(ctx, stranger.ID); err != nil || len(rows) != 0 {
				t.Fatal("discovery leaked another owner", err)
			}
			if _, err := s.Runner(ctx, stranger.ID, selected.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("runner lookup leaked another owner", err)
			}
			issue := func(binding runner.Binding) (bool, error) {
				record, err := s.CreateRunnerAccess(ctx, wire.ID(), tokenHash(cookie), user.ID, "", binding, time.Now().Add(time.Minute).Unix())
				if err != nil {
					return false, err
				}
				return s.CheckAccess(ctx, record, time.Now().Unix())
			}
			if ok, err := issue(*selected.Binding); err != nil || !ok {
				t.Fatal("selected binding refused", err)
			}
			for _, bad := range []runner.Binding{
				{RunnerID: wire.ID(), FabricID: selected.Binding.FabricID, MachineID: machine.ID, Revision: 1},
				{RunnerID: selected.ID, FabricID: "another-fabric", MachineID: machine.ID, Revision: 1},
				{RunnerID: selected.ID, FabricID: selected.Binding.FabricID, MachineID: machine.ID, Revision: 2},
			} {
				if _, err := issue(bad); !errors.Is(err, runner.ErrBindingChanged) {
					t.Fatal("unselected binding accepted", err)
				}
			}
			record, err := s.CreateRunnerAccess(ctx, wire.ID(), tokenHash(cookie), user.ID, "", *selected.Binding, time.Now().Add(time.Minute).Unix())
			if err != nil {
				t.Fatal(err)
			}
			// Simulate a committed replacement by the lifecycle module. New resources
			// get new machine identities; the logical Runner remains unchanged.
			if _, err := s.db.Exec(`DELETE FROM dune_machines WHERE id=$1`, machine.ID); err != nil {
				t.Fatal(err)
			}
			replacement := wire.ID()
			if _, err := s.db.Exec(`UPDATE dune_runners SET binding_revision=binding_revision+1 WHERE id=$1`, selected.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT INTO dune_machines(id,runner_id,credential_hash,os,arch) VALUES($1,$2,$3,'linux','amd64')`, replacement, selected.ID, wire.ID()); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.CheckAccess(ctx, record, time.Now().Unix()); err != nil || ok {
				t.Fatal("old access followed replacement", err)
			}
			if _, err := issue(*selected.Binding); err == nil {
				t.Fatal("stale snapshot reached replacement")
			}
			if backend == "sqlite" {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			current, err := s.Runner(ctx, user.ID, selected.ID)
			if err != nil || current.Binding == nil || current.Binding.MachineID != replacement || current.Binding.Revision != 2 {
				t.Fatal("replacement lost stable runner or revision", err)
			}
			if ok, err := issue(*current.Binding); err != nil || !ok {
				t.Fatal("explicit fresh selection refused", err)
			}
		})
	}
}
