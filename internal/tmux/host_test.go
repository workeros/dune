package tmux

import (
	"testing"

	"github.com/aiomni/dune/internal/wire"
)

func TestHostCleanupUsesInstanceAndLeavesReusedNameAndNeighbors(t *testing.T) {
	s := server(t)
	id, original, replacement, neighbor := wire.ID(), wire.ID(), wire.ID(), wire.ID()
	if err := s.CreateHost(id, original, "/usr/bin/true", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateHost(neighbor, neighbor, "/usr/bin/true", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.hostSession(id, original); err != nil {
		t.Fatal(err)
	}
	if err := s.DestroyHost(id, replacement); err == nil {
		t.Fatal("foreign instance retired the host")
	}
	if session, err := s.hostSession(id, original); err != nil || session == "" {
		t.Fatal("identity mismatch destroyed original host", session, err)
	}
	if err := s.DestroyHost(id, original); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateHost(id, replacement, "/usr/bin/true", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := s.DestroyHost(id, original); err == nil {
		t.Fatal("old cleanup retired reused name")
	}
	if session, err := s.hostSession(id, replacement); err != nil || session == "" {
		t.Fatal("replacement was removed", session, err)
	}
	if session, err := s.hostSession(neighbor, neighbor); err != nil || session == "" {
		t.Fatal("neighbor was removed", session, err)
	}
	if _, err := s.run("set-environment", "-t", "=acp-"+neighbor, "-u", "DUNE_ACP_INSTANCE"); err != nil {
		t.Fatal(err)
	}
	if err := s.DestroyHost(neighbor, neighbor); err == nil {
		t.Fatal("missing marker was mistaken for an absent host")
	}
	if _, err := s.run("set-environment", "-t", "=acp-"+neighbor, "DUNE_ACP_INSTANCE", neighbor); err != nil {
		t.Fatal(err)
	}
	if err := s.DestroyHost(id, replacement); err != nil {
		t.Fatal(err)
	}
	if err := s.DestroyHost(neighbor, neighbor); err != nil {
		t.Fatal(err)
	}
	if err := s.DestroyHost(neighbor, neighbor); err != nil {
		t.Fatal("absent host cleanup is not idempotent", err)
	}
}
