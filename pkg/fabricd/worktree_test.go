package fabricd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func worktreeRepository(t *testing.T) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", output, err)
		}
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("committed"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "initial")
	engine := newEngine(context.Background())
	t.Cleanup(engine.Close)
	return engine, root
}

func TestCreateWorktreeKeepsSourceChangesAndPinsCommit(t *testing.T) {
	engine, root := worktreeRepository(t)
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("local changes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("private draft"), 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "new worktree 中文")
	created, err := engine.createWorktree(api.WorktreeCreate{Directory: root, Path: destination, Branch: "feat/assistant"})
	if err != nil {
		t.Fatal(err)
	}
	wantPath, _ := filepath.EvalSymlinks(destination)
	if created.Path != wantPath || created.Branch != "feat/assistant" || len(created.Head) != 40 || created.Detached {
		t.Fatalf("wrong worktree: %+v", created)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "tracked.txt")); err != nil || string(data) != "committed" {
		t.Fatalf("worktree contents: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatal("untracked source file was copied")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "tracked.txt")); string(data) != "local changes" {
		t.Fatal("source changes were modified")
	}
	items, err := engine.worktrees(root)
	if err != nil || len(items) != 2 {
		t.Fatalf("listing: %+v %v", items, err)
	}
	for _, item := range items {
		if item.Path != created.Path && item.Branch != "main" {
			t.Fatal("source branch changed")
		}
	}
	// A source can itself be a worktree; it must use the same common-directory lock.
	other, err := engine.createWorktree(api.WorktreeCreate{Directory: destination, Path: filepath.Join(t.TempDir(), "other"), Branch: "feat/second", Ref: created.Head})
	if err != nil || other.Head != created.Head {
		t.Fatalf("create from worktree: %+v %v", other, err)
	}
}

func TestCreateWorktreeRefusesReusedPathsAndBranches(t *testing.T) {
	engine, root := worktreeRepository(t)
	destination := filepath.Join(t.TempDir(), "worktree")
	_, err := engine.createWorktree(api.WorktreeCreate{Directory: root, Path: destination, Branch: "feat/one"})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []api.WorktreeCreate{
		{Directory: root, Path: destination, Branch: "feat/two"},
		{Directory: root, Path: filepath.Join(t.TempDir(), "existing-branch"), Branch: "feat/one"},
		{Directory: root, Path: filepath.Join(t.TempDir(), "bad-ref"), Branch: "feat/three", Ref: "does-not-exist"},
		{Directory: root, Path: filepath.Join(t.TempDir(), "bad-option"), Branch: "--force"},
	} {
		if _, err := engine.createWorktree(request); err == nil {
			t.Fatalf("unsafe request succeeded: %+v", request)
		}
	}
	items, err := engine.worktrees(root)
	if err != nil || len(items) != 2 {
		t.Fatalf("rejected creates changed worktrees: %+v %v", items, err)
	}
}

func TestConcurrentWorktreeCreationSharesGitLock(t *testing.T) {
	engine, root := worktreeRepository(t)
	var group sync.WaitGroup
	errors := make(chan error, 2)
	for _, suffix := range []string{"first", "second"} {
		destination := filepath.Join(t.TempDir(), suffix)
		group.Go(func() {
			_, err := engine.createWorktree(api.WorktreeCreate{Directory: root, Path: destination, Branch: "feat/same"})
			errors <- err
		})
	}
	group.Wait()
	close(errors)
	success := 0
	for err := range errors {
		if err == nil {
			success++
		} else if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("unexpected failure: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("accepted %d simultaneous creations of same branch", success)
	}
}
