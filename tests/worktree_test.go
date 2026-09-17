package tests

import (
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestWorktreeCreationThroughGateway(t *testing.T) {
	h := start(t)
	for _, args := range [][]string{{"git", "init", "-b", "main"}, {"git", "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "initial"}} {
		if result := h.exec(args...); result.ExitCode != 0 {
			t.Fatal(result.Stderr)
		}
	}
	items, err := h.client.Worktrees(h.ctx, h.dir)
	must(t, err)
	if len(items) != 1 || items[0].Branch != "main" {
		t.Fatalf("initial worktrees: %+v", items)
	}
	created, err := h.client.CreateWorktree(h.ctx, api.WorktreeCreate{Directory: h.dir, Path: filepath.Join(h.dir, "checkouts", "assistant"), Branch: "feat/assistant"})
	must(t, err)
	if created.Branch != "feat/assistant" || created.Head != items[0].Head {
		t.Fatalf("created worktree: %+v", created)
	}
	// The returned directory can start either PTY or ACP through the existing
	// Profile route; no environment or Runner is provisioned by this operation.
	current, err := h.client.Worktrees(h.ctx, created.Path)
	must(t, err)
	if len(current) != 2 {
		t.Fatalf("worktree listing from new checkout: %+v", current)
	}
}
