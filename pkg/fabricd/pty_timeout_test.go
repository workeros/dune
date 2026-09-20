package fabricd

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/gateway"
)

func timeoutClient(t *testing.T, engine *Engine) (*client.Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	g := gateway.New()
	var wg sync.WaitGroup
	left, right := net.Pipe()
	binding, handler, err := (access.Grant{Target: "timeout-test", Role: gateway.RoleDaemon}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	wg.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
	wg.Go(func() { _ = engine.ServeConn(ctx, right, "timeout-test") })
	for !g.Online("timeout-test") {
		select {
		case <-ctx.Done():
			t.Fatal("fabricd did not connect")
		case <-time.After(time.Millisecond):
		}
	}
	left, right = net.Pipe()
	binding, handler, err = (access.Grant{Target: "timeout-test", Role: gateway.RoleSDK}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	wg.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
	c, err := client.Connect(ctx, right, "timeout-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); c.Close(); g.Close(); wg.Wait() })
	return c, ctx
}

func waitTimeoutTest(t *testing.T, check func() bool) {
	t.Helper()
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout state did not settle")
}

func timeoutPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitTimeoutTest(t, func() bool {
		b, _ := os.ReadFile(path)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return pid > 0
	})
	return pid
}

func assertNoTimeoutRecords(t *testing.T, stateDir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, "pty-timeouts"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("session cleanup retained timeout records", entries)
	}
}

func TestPTYTimeoutWhileFabricdClosedRetainsHistoryAndReason(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	engine, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close(); _ = engine.tmux.Close() })
	c, ctx := timeoutClient(t, engine)
	cwd := t.TempDir()
	pidFile := filepath.Join(cwd, "pid")
	r, stream, err := testStartProfile(c, ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: cwd,
		Start: api.Command{TimeoutSeconds: 1, Argv: []string{"/bin/sh", "-c", `trap '' TERM; echo $$ > "$1"; i=0; while [ "$i" -lt 100 ]; do echo "history-$i"; i=$((i+1)); done; printf 'RETAINED_HISTORY\n'; exec sleep 30`, "test", pidFile}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.StartedAt == nil || r.DeadlineAt == nil || r.DeadlineAt.Sub(*r.StartedAt) != time.Second {
		t.Fatal("start response lacks helper deadline", r)
	}
	pid := timeoutPID(t, pidFile)
	engine.mu.Lock()
	session := engine.runtimes[r.ID].tmux
	engine.mu.Unlock()
	stream.Close()
	c.Close()
	engine.Close()
	waitTimeoutTest(t, func() bool {
		p, e := session.Inspect()
		return e == nil && p.Dead
	})
	if syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal("target still running while fabricd is closed")
	}
	capture, err := session.Capture()
	if err != nil || capture.HistoryLines == 0 || !strings.Contains(capture.Content, "RETAINED_HISTORY") {
		t.Fatal("timeout destroyed terminal history", capture, err)
	}
	restored, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restored.Close() })
	current, ctx := timeoutClient(t, restored)
	got, err := current.Get(ctx, r)
	if err != nil || got.State != "exited" || got.StopReason != "timed_out" || got.ExitCode == nil || *got.ExitCode != -1 {
		t.Fatal("restart did not restore timeout outcome", got, err)
	}
	if got.StartedAt == nil || got.DeadlineAt == nil || !got.StartedAt.Equal(*r.StartedAt) || !got.DeadlineAt.Equal(*r.DeadlineAt) {
		t.Fatal("restart changed the original deadline", r, got)
	}
	if err := current.CallID(ctx, "runtime.forget", wire.ID(), struct{}{}, nil, &r); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Capture(); err == nil {
		t.Fatal("forget retained history")
	}
	assertNoTimeoutRecords(t, dir)
}

func TestPTYExplicitStopDestroysHistoryAndTimeoutRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	engine, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close(); _ = engine.tmux.Close() })
	c, ctx := timeoutClient(t, engine)
	cwd := t.TempDir()
	pidFile := filepath.Join(cwd, "pid")
	r, stream, err := testStartProfile(c, ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: cwd,
		Start: api.Command{TimeoutSeconds: 60, Argv: []string{"/bin/sh", "-c", `trap '' TERM HUP; echo $$ > "$1"; exec sleep 30`, "test", pidFile}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	pid := timeoutPID(t, pidFile)
	if err := c.Stop(ctx, r); err != nil {
		t.Fatal(err)
	}
	waitTimeoutTest(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	if _, err := c.Get(ctx, r); err == nil {
		t.Fatal("stopped runtime remains visible")
	}
	sessions, err := engine.tmux.Restore()
	if err != nil || len(sessions) != 0 {
		t.Fatal("stop retained native sessions", sessions, err)
	}
	assertNoTimeoutRecords(t, dir)
}

func TestPTYWithoutTimeoutRunsWithoutHelper(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	engine, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close(); _ = engine.tmux.Close() })
	c, ctx := timeoutClient(t, engine)
	cwd := t.TempDir()
	pidFile := filepath.Join(cwd, "pid")
	r, stream, err := testStartProfile(c, ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: cwd,
		Start: api.Command{Argv: []string{"/bin/sh", "-c", `echo $$ > "$1"; exec sleep 30`, "test", pidFile}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	pid := timeoutPID(t, pidFile)
	engine.mu.Lock()
	session := engine.runtimes[r.ID].tmux
	engine.mu.Unlock()
	pane, err := session.Inspect()
	if err != nil || pane.PID != pid || r.StartedAt != nil || r.DeadlineAt != nil {
		t.Fatal("untimed PTY used a helper", pane, r, err)
	}
	if err := c.Stop(ctx, r); err != nil {
		t.Fatal(err)
	}
	assertNoTimeoutRecords(t, dir)
}
