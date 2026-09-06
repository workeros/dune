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
	r, e := s.Create(api.Runtime{ID: strings.Repeat(id, 32), Incarnation: "test", Generation: 1, Adapter: "pty", WorkingDirectory: t.TempDir(), Title: "test"}, []string{"/bin/sh", "-c", script}, env, 50000)
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
	r := session(t, s, "a", `printf '%s\n' "$DUNE_TEST"; echo PID=$$; printf "TERM=%s\n" "$TERM"; sleep 120`, []string{"PATH=/usr/bin:/bin", "DUNE_TEST=" + value})
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
