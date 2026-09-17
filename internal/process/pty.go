package process

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const ptyTermGrace = 2 * time.Second

// PTYState is the timeout helper's record, not a second Runtime store. Wall
// times describe the deadline to users; only the helper's elapsed clock drives
// termination. The helper is its sole writer, including across fabricd restarts.
type PTYState struct {
	RuntimeID   string    `json:"runtime_id"`
	Incarnation string    `json:"incarnation"`
	StartedAt   time.Time `json:"started_at"`
	DeadlineAt  time.Time `json:"deadline_at"`
	StopReason  string    `json:"stop_reason,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	Error       string    `json:"error,omitempty"`
}

func PTYCommand(dir, id, incarnation string, timeout time.Duration, argv []string) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return append([]string{exe, "_pty_guard", dir, id, incarnation, strconv.FormatInt(int64(timeout), 10)}, argv...), nil
}

func ReadPTYState(dir, id, incarnation string) (*PTYState, error) {
	f, err := os.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var state PTYState
	if err := json.NewDecoder(io.LimitReader(f, 16*1024)).Decode(&state); err != nil {
		return nil, fmt.Errorf("invalid PTY timeout record: %w", err)
	}
	if state.RuntimeID != id || state.Incarnation != incarnation {
		return nil, fmt.Errorf("PTY timeout record identity mismatch")
	}
	return &state, nil
}

func RemovePTYState(dir string) error {
	// Rename first so an in-flight helper cannot create another temporary file
	// while RemoveAll is walking the directory. Its future writes use the now
	// absent original name and never recreate it.
	retired, err := os.MkdirTemp(filepath.Dir(dir), ".deleted-*")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer os.RemoveAll(retired)
	if err := os.Rename(dir, filepath.Join(retired, "state")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(retired)
}

func writePTYState(dir string, state PTYState) error {
	// Never recreate dir: session destruction removes it, including when a
	// terminating helper has not finished its last write yet.
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(state); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "state.json"))
}

// PTYGuard runs inside a tmux pane, independently of fabricd and its lifetime
// pipe. It never execs a program after startup, so deleting an old release does
// not disable an existing deadline. Only PTYs with a timeout use this helper.
func PTYGuard(args []string) int {
	if len(args) < 5 || !filepath.IsAbs(args[0]) {
		return 125
	}
	dir, id, incarnation := args[0], args[1], args[2]
	nanos, err := strconv.ParseInt(args[3], 10, 64)
	if err != nil || nanos <= 0 || time.Duration(nanos) > 24*time.Hour || id == "" || incarnation == "" {
		return 125
	}
	state := PTYState{RuntimeID: id, Incarnation: incarnation}
	fail := func(err error) int {
		state.Error, state.StopReason = err.Error(), "start_failed"
		_ = writePTYState(dir, state)
		fmt.Fprintln(os.Stderr, "PTY timeout helper:", err)
		return 125
	}
	// Refuse to start after session cleanup, or without tmux's controlling tty.
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fail(fmt.Errorf("PTY timeout directory is unavailable"))
	}
	if syscall.Getpgrp() != os.Getpid() {
		return fail(fmt.Errorf("PTY timeout helper must be a process group leader"))
	}
	if _, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP); err != nil {
		return fail(fmt.Errorf("PTY timeout helper needs a controlling terminal: %w", err))
	}
	program, err := exec.LookPath(args[4])
	if err != nil {
		return fail(err)
	}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(signals)
	started, err := elapsedTime()
	if err != nil {
		return fail(err)
	}
	state.StartedAt = time.Now().UTC()
	state.DeadlineAt = state.StartedAt.Add(time.Duration(nanos))
	// Give the command its own foreground group. Interactive shells can manage
	// foreground jobs normally; the helper neither consumes terminal input nor
	// receives the user's Ctrl-C. Keep the direct child unreaped during timeout
	// cleanup so its PID/process-group identity cannot be reused before KILL.
	child, err := os.StartProcess(program, args[4:], &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Sys:   &syscall.SysProcAttr{Foreground: true, Ctty: int(os.Stdin.Fd())},
	})
	if err != nil {
		return fail(err)
	}
	defer child.Release()
	if err = writePTYState(dir, state); err != nil {
		signalPTY(child.Pid, syscall.SIGKILL)
		_, _ = waitPTY(child.Pid, false)
		return fail(err)
	}
	deadline := started + time.Duration(nanos)
	var status *syscall.WaitStatus
	for status == nil {
		status, err = waitPTY(child.Pid, true)
		if err != nil {
			state.Error = err.Error()
			state.StopReason = "helper_failed"
			break
		}
		if status != nil {
			state.StopReason = "exited"
			break
		}
		if ptyClosed() {
			state.StopReason = "stopped"
			break
		}
		now, clockErr := elapsedTime()
		if clockErr != nil {
			state.Error, state.StopReason = clockErr.Error(), "helper_failed"
			break
		}
		if now >= deadline {
			state.StopReason = "timed_out"
			signalPTY(child.Pid, syscall.SIGTERM)
			// No disk IO or reaping between TERM and KILL. Both deadlines use
			// elapsed time including suspend; short polls notice a wake promptly.
			waitPTYGrace(now+ptyTermGrace, signals)
			break
		}
		select {
		case sig := <-signals:
			if sig == syscall.SIGHUP || sig == syscall.SIGTERM {
				state.StopReason = "stopped"
			}
		case <-time.After(min(deadline-now, 25*time.Millisecond)):
		}
		if state.StopReason != "" {
			break
		}
	}
	if status == nil {
		signalPTY(child.Pid, syscall.SIGKILL)
		status, err = waitPTY(child.Pid, false)
		if err != nil {
			state.Error = err.Error()
		}
	}
	code := -1
	if status != nil && status.Exited() {
		code = status.ExitStatus()
	}
	state.ExitCode = &code
	if err := writePTYState(dir, state); err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "PTY timeout state:", err)
	}
	return code
}

func waitPTY(pid int, nonblocking bool) (*syscall.WaitStatus, error) {
	options := 0
	if nonblocking {
		options = syscall.WNOHANG
	}
	for {
		var status syscall.WaitStatus
		waited, err := syscall.Wait4(pid, &status, options, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil || waited == 0 {
			return nil, err
		}
		return &status, nil
	}
}

func signalPTY(child int, sig syscall.Signal) {
	// The child started as group leader and has not been reaped. Also signal
	// the terminal's current foreground group for an interactive shell's job.
	foreground, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	if err == nil && foreground > 1 && foreground != child && foreground != syscall.Getpgrp() {
		_ = syscall.Kill(-foreground, sig)
	}
	_ = syscall.Kill(-child, sig)
	if group, err := syscall.Getpgid(child); err == nil && group != child {
		// The direct child remains ours even if it deliberately changes groups.
		// Descendants that leave the supervised groups are outside this contract.
		_ = syscall.Kill(child, sig)
	}
}

func waitPTYGrace(deadline time.Duration, signals <-chan os.Signal) {
	for {
		now, err := elapsedTime()
		if err != nil || now >= deadline || ptyClosed() {
			return
		}
		select {
		case sig := <-signals:
			if sig == syscall.SIGHUP || sig == syscall.SIGTERM {
				return
			}
		case <-time.After(min(deadline-now, 25*time.Millisecond)):
		}
	}
}

func ptyClosed() bool {
	// Some platforms send terminal hangup only to its foreground group. The
	// helper is deliberately in the background, so it also observes POLLHUP
	// without reading (and stealing) the command's terminal input.
	fds := []unix.PollFd{{Fd: int32(os.Stdin.Fd())}}
	_, err := unix.Poll(fds, 0)
	return (err != nil && err != syscall.EINTR) || fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0
}
