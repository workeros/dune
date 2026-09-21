package metadata

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/workbench"
)

func TestProjectsPersistAndCompareRevision(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			store, err := Open(t.Context(), config, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			spec := workbench.ProjectSpec{Name: "Dune", Directories: []workbench.Directory{
				{ID: "local", Path: "/src/dune", Binding: runner.Binding{RunnerID: "runner-a", FabricID: "attached", MachineID: "machine-a", Revision: 1}},
				{ID: "remote", Path: "/work/dune", Binding: runner.Binding{RunnerID: "runner-b", FabricID: "attached", MachineID: "machine-b", Revision: 3}},
			}}
			project, err := store.SaveProject(t.Context(), "tenant", "", 0, spec)
			if err != nil || project.Revision != 1 {
				t.Fatal(project, err)
			}
			if _, err := store.Project(t.Context(), "other", project.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("cross-owner get", err)
			}
			if _, err := store.SaveProject(t.Context(), "other", project.ID, 1, spec); !errors.Is(err, ErrNotFound) {
				t.Fatal("cross-owner update", err)
			}
			if err := store.DeleteProject(t.Context(), "other", project.ID, 1); !errors.Is(err, ErrNotFound) {
				t.Fatal("cross-owner delete", err)
			}
			if items, err := store.Projects(t.Context(), "other", "", 10); err != nil || len(items) != 0 {
				t.Fatal("cross-owner list", items, err)
			}

			peer := store
			if backend == "postgres" {
				peer, err = Open(t.Context(), config, OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer peer.Close()
			}
			var wg sync.WaitGroup
			results := make(chan error, 2)
			for _, writer := range []*Store{store, peer} {
				wg.Go(func() { _, err := writer.SaveProject(t.Context(), "tenant", project.ID, 1, spec); results <- err })
			}
			wg.Wait()
			close(results)
			succeeded, conflicts := 0, 0
			for err := range results {
				if err == nil {
					succeeded++
				} else if errors.Is(err, ErrConflict) {
					conflicts++
				} else {
					t.Fatal(err)
				}
			}
			if succeeded != 1 || conflicts != 1 {
				t.Fatal("concurrent edits overwrote each other", succeeded, conflicts)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(t.Context(), config, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := store.Project(t.Context(), "tenant", project.ID)
			if err != nil || loaded.Revision != 2 || len(loaded.Directories) != 2 || loaded.Directories[1].Binding.Revision != 3 {
				t.Fatal("reopen lost project", loaded, err)
			}
			if err := store.DeleteProject(t.Context(), "tenant", project.ID, 1); !errors.Is(err, ErrConflict) {
				t.Fatal("stale delete", err)
			}
			if err := store.DeleteProject(t.Context(), "tenant", project.ID, 2); err != nil {
				t.Fatal(err)
			}
		})
	}
}
