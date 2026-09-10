package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
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
			defer s.Close()
			local := identity.NewLocal(s, true)
			user, _, err := local.Register(ctx, "runner@example.test", "runner-test-password")
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
			rows, err := s.Runners(ctx, user.ID)
			if err != nil || len(rows) != 1 || rows[0].Binding == nil {
				t.Fatal("runner discovery", rows, err)
			}
			selected := rows[0]
			if selected.ID != machine.RunnerID || selected.Binding.MachineID != machine.ID {
				t.Fatal("incorrect runner binding", selected)
			}
			stranger, _, err := local.Register(ctx, "stranger@example.test", "stranger-test-password")
			if err != nil {
				t.Fatal(err)
			}
			if rows, err := s.Runners(ctx, stranger.ID); err != nil || len(rows) != 0 {
				t.Fatal("discovery leaked another owner", rows, err)
			}
			if _, err := s.Runner(ctx, stranger.ID, selected.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("runner lookup leaked another owner", err)
			}
			record := authorization.ConnectionAccess{PrincipalID: user.ID, OwnerID: user.ID, Target: machine.ID, RunnerID: selected.ID, FabricID: selected.Binding.FabricID, BindingRevision: selected.Binding.Revision}
			if ok, err := s.CheckRunnerAccess(ctx, record); err != nil || !ok {
				t.Fatal("selected binding refused", err)
			}
			for _, mutate := range []func(*authorization.ConnectionAccess){
				func(r *authorization.ConnectionAccess) { r.RunnerID = wire.ID() },
				func(r *authorization.ConnectionAccess) { r.FabricID = "another-fabric" },
				func(r *authorization.ConnectionAccess) { r.BindingRevision++ },
			} {
				bad := record
				mutate(&bad)
				if ok, err := s.CheckRunnerAccess(ctx, bad); err != nil || ok {
					t.Fatal("unselected binding accepted", bad, err)
				}
			}
			replacement := wire.ID()
			if _, err := s.db.Exec(`UPDATE dune_runners SET machine_id=$2,binding_revision=binding_revision+1 WHERE id=$1`, selected.ID, replacement); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.CheckRunnerAccess(ctx, record); err != nil || ok {
				t.Fatal("old access followed replacement", err)
			}
			current, err := s.Runner(ctx, user.ID, selected.ID)
			if err != nil || current.Binding == nil || current.Binding.MachineID != replacement || current.Binding.Revision != 2 {
				t.Fatal("replacement lost stable runner or revision", current, err)
			}
		})
	}
}
