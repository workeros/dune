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

func TestPersonalViews(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			s, err := Open(t.Context(), config, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			owner := ViewOwner{OwnerID: "tenant", Namespace: "directory", UserID: "one"}
			_, token, _, err := s.IssueEnrollment(t.Context(), owner.OwnerID, "machine")
			if err != nil {
				t.Fatal(err)
			}
			machine, _, err := s.Enroll(t.Context(), token, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			target := workbench.AgentTarget{Binding: runner.Binding{RunnerID: machine.RunnerID, MachineID: machine.ID, FabricID: "attached", Revision: 1}, Runtime: workbench.RuntimeRef{ID: "runtime-one", Incarnation: "boot", Generation: 1, Adapter: "pty"}}
			view := workbench.View{ID: "main", ViewSpec: workbench.ViewSpec{Root: &workbench.SplitNode{ID: "pane-one", Pane: &workbench.Pane{Target: target}}, FocusPane: "pane-one", ReviewPane: "pane-one"}}
			first, err := s.SaveView(t.Context(), owner, view)
			if err != nil || first.Revision != 1 {
				t.Fatal(first, err)
			}
			if _, err := s.SaveView(t.Context(), owner, view); !errors.Is(err, ErrConflict) {
				t.Fatal("duplicate create overwrote view", err)
			}
			for _, other := range []ViewOwner{{OwnerID: "tenant", Namespace: "directory", UserID: "two"}, {OwnerID: "tenant", Namespace: "other-directory", UserID: "one"}, {OwnerID: "other-tenant", Namespace: "directory", UserID: "one"}} {
				got, err := s.View(t.Context(), other, "main")
				if err != nil || got.Revision != 0 || got.Root != nil {
					t.Fatal("personal view leaked", got, err)
				}
			}
			peer := s
			if backend == "postgres" {
				peer, err = Open(t.Context(), config, OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer peer.Close()
			}
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for _, writer := range []*Store{s, peer} {
				wg.Go(func() { _, err := writer.SaveView(t.Context(), owner, first); results <- err })
			}
			wg.Wait()
			close(results)
			success, conflicts := 0, 0
			for err := range results {
				if err == nil {
					success++
				} else if errors.Is(err, ErrConflict) {
					conflicts++
				} else {
					t.Fatal(err)
				}
			}
			if success != 1 || conflicts != 1 {
				t.Fatal("view revision race", success, conflicts)
			}
			if err := s.Revoke(t.Context(), owner.OwnerID, machine.ID); err != nil {
				t.Fatal(err)
			}
			current, err := s.View(t.Context(), owner, "main")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SaveView(t.Context(), owner, current); err != nil {
				t.Fatal("stale pane could not remain in layout", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(t.Context(), config, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.View(t.Context(), owner, "main")
			if err != nil || got.Revision != 3 || got.Root.Pane.Target != target || got.ReviewPane != "pane-one" {
				t.Fatal("reopen lost view", got, err)
			}

		})
	}
}
