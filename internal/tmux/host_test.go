package tmux

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/wire"
)

func TestHostSurvivesIsolatedStdioWithoutPuttingOutputInPane(t *testing.T) {
	s := server(t)
	directory := t.TempDir()
	program := filepath.Join(directory, "host")
	script := "#!/bin/sh\n" +
		"printf 'private stdout sentinel\\n'\n" +
		"printf 'private stderr sentinel\\n' >&2\n" +
		"printf ready > \"$2/ready\"\n" +
		"while [ ! -e \"$2/release\" ]; do /bin/sleep 0.05; done\n"
	if err := os.WriteFile(program, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	id := wire.ID()
	if _, err := s.CreateHost(id, wire.ID(), program, directory); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		_, err := os.Stat(filepath.Join(directory, "ready"))
		return err == nil
	})
	if output, err := s.run("list-panes", "-t", "=acp-"+id, "-F", "#{pane_dead}"); err != nil || strings.TrimSpace(output) != "0" {
		t.Fatal("host died after isolating its standard streams", output, err)
	}
	if output, err := s.run("capture-pane", "-p", "-t", "acp-"+id+":0.0"); err != nil || strings.TrimSpace(output) != "" {
		t.Fatal("private output entered the tmux pane", output, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		output, err := s.run("list-panes", "-t", "=acp-"+id, "-F", "#{pane_dead}:#{pane_dead_status}")
		return err == nil && strings.TrimSpace(output) == "1:0"
	})
}

func TestHostCleanupUsesInstanceAndLeavesReusedNameAndNeighbors(t *testing.T) {
	s := server(t)
	id, original, replacement, neighbor := wire.ID(), wire.ID(), wire.ID(), wire.ID()
	if _, err := s.CreateHost(id, original, "/usr/bin/true", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHost(neighbor, neighbor, "/usr/bin/true", t.TempDir()); err != nil {
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
	if _, err := s.CreateHost(id, replacement, "/usr/bin/true", t.TempDir()); err != nil {
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

func TestLinuxDetachedStdioWithoutKeepaliveReproducesSIGHUP(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux terminal-close regression control")
	}
	s := server(t)
	if _, err := s.run("new-session", "-d", "-s", "old-startup", "exec /bin/sleep 2 </dev/null >/dev/null 2>/dev/null"); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		out, err := s.run("list-panes", "-t", "=old-startup", "-F", "#{pane_dead}:#{pane_dead_signal}")
		return err == nil && strings.TrimSpace(out) == "1:1"
	})
	t.Log("old command exited with SIGHUP (signal 1)")
}
