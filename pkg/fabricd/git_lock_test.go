package fabricd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGitCommonDirectoryCoordinatesWorktreesAndAliases(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	run("init")
	run("-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "initial")
	worktree := filepath.Join(t.TempDir(), "worktree")
	run("worktree", "add", "-b", "other", worktree)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(context.Background())
	defer engine.Close()
	want, err := engine.gitCommonDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{worktree, alias} {
		got, err := engine.gitCommonDirectory(path)
		if err != nil || got != want {
			t.Fatalf("lock key %s = %s want %s: %v", path, got, want, err)
		}
	}
	release := engine.gitLocks.acquire(want)
	same := make(chan struct{})
	go func() { end := engine.gitLocks.acquire(want); end(); close(same) }()
	other := make(chan struct{})
	go func() { end := engine.gitLocks.acquire("different-repository"); end(); close(other) }()
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("unrelated repository blocked")
	}
	select {
	case <-same:
		t.Fatal("shared refs ran concurrently")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-same:
	case <-time.After(time.Second):
		t.Fatal("waiting operation did not proceed")
	}
	engine.gitLocks.mu.Lock()
	defer engine.gitLocks.mu.Unlock()
	if len(engine.gitLocks.entries) != 0 {
		t.Fatal("lock entries leaked")
	}
}
