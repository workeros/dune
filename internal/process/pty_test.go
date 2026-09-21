package process

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "_guard" {
		os.Exit(Guard(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "_pty_guard" {
		os.Exit(PTYGuard(os.Args[2:]))
	}
	os.Exit(m.Run())
}

type terminalOutput struct {
	sync.Mutex
	bytes.Buffer
}

func (b *terminalOutput) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}

func (b *terminalOutput) text() string {
	b.Lock()
	defer b.Unlock()
	return b.String()
}

type timeoutTerminal struct {
	cmd    *exec.Cmd
	tty    *os.File
	dir    string
	done   chan struct{}
	output terminalOutput
}

func startTimeoutTerminal(t *testing.T, executable string, timeout time.Duration, argv ...string) *timeoutTerminal {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "timeout")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	command, err := PTYCommand(dir, "runtime", "incarnation", timeout, argv)
	if err != nil {
		t.Fatal(err)
	}
	if executable != "" {
		command[0] = executable
	}
	h := &timeoutTerminal{cmd: exec.Command(command[0], command[1:]...), dir: dir, done: make(chan struct{})}
	h.cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	h.tty, err = pty.Start(h.cmd)
	if err != nil {
		t.Fatal(err)
	}
	fd := int(h.tty.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	// Avoid a blocking Darwin master read holding the terminal open after
	// Close. The helper must observe a real last-master close in the HUP test.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := syscall.Read(fd, buf)
			if n > 0 {
				_, _ = h.output.Write(buf[:n])
			}
			if err != nil && err != syscall.EAGAIN && err != syscall.EINTR {
				return
			}
			if n == 0 && err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	go func() { _ = h.cmd.Wait(); close(h.done) }()
	t.Cleanup(func() {
		// Closing the PTY tests the same HUP lifecycle as tmux kill-session.
		_ = h.tty.Close()
		select {
		case <-h.done:
		case <-time.After(4 * time.Second):
			_ = h.cmd.Process.Kill()
			<-h.done
		}
	})
	return h
}

func waitPTYTest(t *testing.T, check func() bool) {
	t.Helper()
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("PTY condition did not settle")
}

func (h *timeoutTerminal) state(t *testing.T) *PTYState {
	t.Helper()
	state, err := ReadPTYState(h.dir, "runtime", "incarnation")
	if err != nil {
		t.Fatalf("state: %v; terminal: %s", err, h.output.text())
	}
	return state
}

func (h *timeoutTerminal) awaitState(t *testing.T, exited bool) *PTYState {
	t.Helper()
	waitPTYTest(t, func() bool {
		state, err := ReadPTYState(h.dir, "runtime", "incarnation")
		if err == nil && state.Error != "" {
			t.Fatalf("helper failed: %+v; output: %s", state, h.output.text())
		}
		return err == nil && (!exited || state.ExitCode != nil)
	})
	return h.state(t)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitPTYTest(t, func() bool {
		data, _ := os.ReadFile(path)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		return pid > 0
	})
	return pid
}

func TestPTYNaturalExitDoesNotInferTimeoutFromExitCode(t *testing.T) {
	h := startTimeoutTerminal(t, "", time.Second, "/bin/sh", "-c", "printf NATURAL; exit 124")
	state := h.awaitState(t, true)
	if state.StopReason != "exited" || *state.ExitCode != 124 || state.DeadlineAt.Sub(state.StartedAt) != time.Second {
		t.Fatalf("natural exit mislabeled: %+v", state)
	}
	info, err := os.Stat(filepath.Join(h.dir, "state.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("timeout state is not private", info, err)
	}
}

func TestPTYTimeoutTERMThenKILL(t *testing.T) {
	for _, tc := range []struct {
		name, trap string
		exit       int
	}{
		{name: "accepts TERM", trap: "printf TERM_ACCEPTED; exit 42", exit: 42},
		{name: "ignores TERM", trap: "printf TERM_IGNORED", exit: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			script := `trap '` + tc.trap + `' TERM; printf '%s' "$$" > "$1"; while :; do sleep 30 & wait $!; done`
			h := startTimeoutTerminal(t, "", 250*time.Millisecond, "/bin/sh", "-c", script, "test", pidFile)
			pid := readPID(t, pidFile)
			initial := h.awaitState(t, false)
			waitPTYTest(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
			if elapsed := time.Since(initial.DeadlineAt); elapsed > ptyTermGrace+400*time.Millisecond {
				t.Fatalf("target remained alive too long after deadline: %s", elapsed)
			}
			state := h.awaitState(t, true)
			if state.StopReason != "timed_out" || *state.ExitCode != tc.exit {
				t.Fatalf("timeout termination: %+v; output: %s", state, h.output.text())
			}
			if !strings.Contains(h.output.text(), "TERM_") {
				t.Fatal("TERM was not delivered before KILL", h.output.text())
			}
		})
	}
}

func TestPTYForegroundJobControlAndCtrlC(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "shell")
	h := startTimeoutTerminal(t, "", 2*time.Second, "/bin/sh", "-i")
	h.awaitState(t, false)
	_, _ = fmt.Fprintf(h.tty, "printf '%%s' \"$$\" > '%s'\n", pidFile)
	shell := readPID(t, pidFile)
	_, _ = h.tty.Write([]byte("sleep 30\n"))
	var foreground int
	waitPTYTest(t, func() bool {
		foreground, _ = unix.IoctlGetInt(int(h.tty.Fd()), unix.TIOCGPGRP)
		return foreground > 0 && foreground != shell && foreground != h.cmd.Process.Pid
	})
	_, _ = h.tty.Write([]byte{3})
	_, _ = h.tty.Write([]byte("printf 'CTRL_%s\\n' C_WORKS\n"))
	waitPTYTest(t, func() bool { return strings.Contains(h.output.text(), "CTRL_C_WORKS") })
	if h.state(t).ExitCode != nil {
		t.Fatal("Ctrl-C killed the helper/shell")
	}
	_, _ = h.tty.Write([]byte("sleep 30\n"))
	waitPTYTest(t, func() bool {
		foreground, _ = unix.IoctlGetInt(int(h.tty.Fd()), unix.TIOCGPGRP)
		return foreground > 0 && foreground != shell && foreground != h.cmd.Process.Pid
	})
	state := h.awaitState(t, true)
	if state.StopReason != "timed_out" {
		t.Fatalf("interactive timeout: %+v; terminal: %s", state, h.output.text())
	}
	if syscall.Kill(shell, 0) != syscall.ESRCH || syscall.Kill(foreground, 0) != syscall.ESRCH {
		t.Fatal("timeout retained foreground work", shell, foreground, h.output.text())
	}
}

func TestPTYTimeoutSurvivesDeletedReleaseExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(t.TempDir(), "old-release")
	if err := os.Mkdir(release, 0700); err != nil {
		t.Fatal(err)
	}
	copy := filepath.Join(release, "dune")
	if err := os.WriteFile(copy, data, 0700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	h := startTimeoutTerminal(t, copy, 300*time.Millisecond, "/bin/sh", "-c", `trap '' TERM; echo $$ > "$1"; exec sleep 30`, "test", pidFile)
	pid := readPID(t, pidFile)
	h.awaitState(t, false)
	if err := os.RemoveAll(release); err != nil {
		t.Fatal(err)
	}
	state := h.awaitState(t, true)
	if state.StopReason != "timed_out" || syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatalf("deleted release disabled timeout: %+v", state)
	}
}

func TestPTYStopDoesNotRecreateRemovedState(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	h := startTimeoutTerminal(t, "", time.Hour, "/bin/sh", "-c", `trap '' HUP TERM; echo $$ > "$1"; exec sleep 30`, "test", pidFile)
	pid := readPID(t, pidFile)
	h.awaitState(t, false)
	if err := h.tty.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RemovePTYState(h.dir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.done:
	case <-time.After(4 * time.Second):
		t.Fatal("closing terminal did not terminate helper")
	}
	if _, err := os.Stat(h.dir); !os.IsNotExist(err) {
		t.Fatal("helper recreated removed state", err)
	}
	if syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal("explicit stop retained the target")
	}
}
