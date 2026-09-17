package tmux

import (
	"github.com/aiomni/dune/pkg/api"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func server(t *testing.T) *Server {
	t.Helper()
	binary, err := filepath.Abs("../../bin/tmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUNE_TMUX", binary)
	s, err := Open(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func session(t *testing.T, s *Server, id, script string, env []string) *Session {
	t.Helper()
	r, e := s.Create(api.Runtime{ID: strings.Repeat(id, 32), Incarnation: "test", Generation: 1, Adapter: "pty", WorkingDirectory: t.TempDir(), Title: "test"}, []string{"/bin/sh", "-c", script}, env, CreateOptions{HistoryLines: 50000})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func await(t *testing.T, check func() bool) {
	t.Helper()
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("tmux state did not settle")
}
func TestPersistenceAndEnvironmentIsolation(t *testing.T) {
	s := server(t)
	value := "a ' quote; $(touch SHOULD_NOT_EXIST)\n中文"
	r := session(t, s, "a", `printf '%s\n' "$DUNE_TEST"; echo PID=$$; printf "TERM=%s\n" "$TERM"; sleep 120`, []string{"PATH=/usr/bin:/bin", "DUNE_TEST=" + value, "TERM=dumb"})
	await(t, func() bool { c, e := r.Capture(); return e == nil && strings.Contains(c.Content, "PID=") })
	c, _ := r.Capture()
	if !strings.Contains(c.Content, "SHOULD_NOT_EXIST") || !strings.Contains(c.Content, "TERM=xterm-256color") {
		t.Fatal(c.Content)
	}
	if _, err := os.Stat(filepath.Join(r.Runtime.WorkingDirectory, "SHOULD_NOT_EXIST")); !os.IsNotExist(err) {
		t.Fatal("shell expanded env")
	}
	pane, _ := r.Inspect()
	viewer, e := r.Attach(false)
	if e != nil {
		t.Fatal(e)
	}
	viewer.Close()
	restored, e := s.Restore()
	if e != nil || len(restored) != 1 {
		t.Fatal(restored, e)
	}
	after, e := restored[0].Inspect()
	if e != nil || after.Dead || after.PID != pane.PID {
		t.Fatal("detach changed process", after, e)
	}
	other := session(t, s, "b", `printf 'ISOLATED:%s\n' "${DUNE_TEST-unset}"; sleep 120`, []string{"PATH=/usr/bin:/bin"})
	await(t, func() bool { c, e := other.Capture(); return e == nil && strings.Contains(c.Content, "ISOLATED:unset") })
	if e = r.Destroy(); e != nil {
		t.Fatal(e)
	}
	after, e = other.Inspect()
	if e != nil || after.Dead {
		t.Fatal("destroy affected another session")
	}
}

func TestPaneDiscoveryIncludesForegroundCommand(t *testing.T) {
	s := server(t)
	r := session(t, s, "g", "exec sleep 120", []string{"PATH=/usr/bin:/bin"})
	await(t, func() bool {
		panes, err := s.Panes()
		pane, found := panes[r.Runtime.ID]
		return err == nil && found && !pane.Dead && pane.PID > 0 && pane.Command == "sleep"
	})
}

func TestLiveScreenIgnoresHistoryAndRemovesStyling(t *testing.T) {
	s := server(t)
	r := session(t, s, "h", `i=0; while [ "$i" -lt 80 ]; do printf 'HISTORICAL_APPROVAL_%s\n' "$i"; i=$((i+1)); done; printf '\033[32mLIVE_PROMPT\033[0m\n'; sleep 120`, []string{"PATH=/usr/bin:/bin"})
	await(t, func() bool {
		content, err := r.LiveScreen()
		return err == nil && strings.Contains(content, "LIVE_PROMPT")
	})
	before, err := r.LiveScreen()
	if err != nil || strings.Contains(before, "\x1b") {
		t.Fatal("screen retained ANSI styling", before, err)
	}
	if err := r.History("older"); err != nil {
		t.Fatal(err)
	}
	after, err := r.LiveScreen()
	if err != nil || after != before {
		t.Fatal("copy mode changed live detection screen", before, after, err)
	}
	pane, err := r.Inspect()
	if err != nil || !pane.InMode {
		t.Fatal("capture left copy mode", pane, err)
	}
}

func TestRunningServerSurvivesRemovedRelease(t *testing.T) {
	s := server(t)
	currentBinary := s.Binary
	program, err := os.ReadFile(currentBinary)
	if err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(t.TempDir(), "old-release")
	if err := os.Mkdir(release, 0700); err != nil {
		t.Fatal(err)
	}
	s.Binary = filepath.Join(release, "tmux")
	if err := os.WriteFile(s.Binary, program, 0700); err != nil {
		t.Fatal(err)
	}
	r := session(t, s, "f", `printf 'OLD_SERVER_READY\n'; while IFS= read -r line; do printf 'reply:%s\n' "$line"; done`, []string{"PATH=/usr/bin:/bin"})
	await(t, func() bool { c, e := r.Capture(); return e == nil && strings.Contains(c.Content, "OLD_SERVER_READY") })
	if err := os.RemoveAll(release); err != nil {
		t.Fatal(err)
	}
	// The new release supplies the CLI; the already-running server continues
	// to own the pane even though its mapped executable has been unlinked.
	s.Binary = currentBinary
	restored, err := s.Restore()
	if err != nil || len(restored) != 1 {
		t.Fatal("new client could not restore the old server", restored, err)
	}
	view, err := restored[0].Attach(false)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	go io.Copy(io.Discard, view)
	if err := view.Write([]byte("NEW_CLIENT\n")); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		c, e := restored[0].Capture()
		return e == nil && strings.Contains(c.Content, "reply:NEW_CLIENT")
	})
}

func TestNativeHistoryAndExit(t *testing.T) {
	s := server(t)
	r := session(t, s, "c", `seq 1 50200; printf '\033[31mFINAL\033[0m\n'; exit 7`, []string{"PATH=/usr/bin:/bin"})
	await(t, func() bool { p, e := r.Inspect(); return e == nil && p.Dead })
	p, _ := r.Inspect()
	if p.ExitCode != 7 {
		t.Fatal(p)
	}
	c, e := r.Capture()
	if e != nil || c.HistoryLimit != 50000 || c.HistoryLines < 45000 || c.HistoryLines > 50000 || !strings.Contains(c.Content, "FINAL") {
		t.Fatalf("%+v %v", c, e)
	}
	history, e := s.run("capture-pane", "-p", "-t", r.pane(), "-S", strconv.Itoa(-c.HistoryLines), "-E", strconv.Itoa(-c.HistoryLines+9))
	if e != nil || len(strings.Split(strings.TrimSpace(history), "\n")) != 10 {
		t.Fatal(history, e)
	}
	if strings.HasPrefix(history, "1\n") {
		t.Fatal("history did not evict oldest lines")
	}
	if e = r.History("older"); e != nil {
		t.Fatal(e)
	}
	mode, e := s.run("display-message", "-p", "-t", r.pane(), "#{pane_in_mode}")
	if e != nil || strings.TrimSpace(mode) != "1" {
		t.Fatal(mode, e)
	}
	if e = r.History("close"); e != nil {
		t.Fatal(e)
	}
}

func TestHistoryCloseIsIdempotent(t *testing.T) {
	s := server(t)
	r := session(t, s, "e", `printf 'READY\n'; while IFS= read -r line; do printf 'reply:%s\n' "$line"; done`, []string{"PATH=/usr/bin:/bin"})
	await(t, func() bool { c, e := r.Capture(); return e == nil && strings.Contains(c.Content, "READY") })

	// Closing history is a no-op when the pane is not in copy mode.
	if err := r.History("close"); err != nil {
		t.Fatal(err)
	}
	if err := r.History("older"); err != nil {
		t.Fatal(err)
	}
	mode, err := s.run("display-message", "-p", "-t", r.pane(), "#{pane_in_mode}")
	if err != nil || strings.TrimSpace(mode) != "1" {
		t.Fatal(mode, err)
	}
	if err = r.History("close"); err != nil {
		t.Fatal(err)
	}
	if err = r.History("close"); err != nil {
		t.Fatal(err)
	}
	mode, err = s.run("display-message", "-p", "-t", r.pane(), "#{pane_in_mode}")
	if err != nil || strings.TrimSpace(mode) != "0" {
		t.Fatal(mode, err)
	}

	view, err := r.Attach(false)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	go io.Copy(io.Discard, view)
	if err = view.Write([]byte("still-running\n")); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		c, e := r.Capture()
		return e == nil && strings.Contains(c.Content, "reply:still-running")
	})

	if err = r.Destroy(); err != nil {
		t.Fatal(err)
	}
	if err = r.History("close"); err == nil {
		t.Fatal("closing history for a missing session succeeded")
	}
}

func TestAlternateScreenResizeAndReattach(t *testing.T) {
	s := server(t)
	r := session(t, s, "d", `printf 'MAIN中文\n'; read -r a; printf '\033[?1049h\033[HAPP中文'; read -r a; printf '\033[?1049l'; sleep 120`, []string{"PATH=/usr/bin:/bin", "LANG=en_US.UTF-8"})
	await(t, func() bool { c, e := r.Capture(); return e == nil && strings.Contains(c.Content, "MAIN中文") })
	view, e := r.Attach(false)
	if e != nil {
		t.Fatal(e)
	}
	go io.Copy(io.Discard, view)
	if e = view.Write([]byte("\r")); e != nil {
		t.Fatal(e)
	}
	await(t, func() bool {
		c, e := r.Capture()
		return e == nil && strings.Contains(c.Content, "APP中文") && !strings.Contains(c.Content, "MAIN")
	})
	if e = view.Resize(40, 100); e != nil {
		t.Fatal(e)
	}
	await(t, func() bool { c, e := r.Capture(); return e == nil && c.Rows == 40 && c.Cols == 100 })
	view.Close()
	c, e := r.Capture()
	if e != nil || !strings.Contains(c.Content, "APP中文") {
		t.Fatal("alternate screen lost on detach", c, e)
	}
	view, e = r.Attach(false)
	if e != nil {
		t.Fatal(e)
	}
	defer view.Close()
	await(t, func() bool { c, e := r.Capture(); return e == nil && c.Rows == 40 && c.Cols == 100 })
	go io.Copy(io.Discard, view)
	if e = view.Write([]byte("\r")); e != nil {
		t.Fatal(e)
	}
	await(t, func() bool {
		c, e := r.Capture()
		return e == nil && strings.Contains(c.Content, "MAIN中文") && !strings.Contains(c.Content, "APP")
	})
}
