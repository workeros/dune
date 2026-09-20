// Package process starts owned process groups through a short-lived guardian.
// The guardian remains the group leader until cleanup. Persisted group numbers
// are only used for absence checks; control always uses its ownership pipe.
package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// WaitGroupExit only observes the original guardian group. Waiting for the
// guardian alone does not prove its children have exited. A surviving or reused
// group therefore stays unconfirmed; this method never signals a persisted PID.
func (p *Process) WaitGroupExit(ctx context.Context) error {
	select {
	case <-p.Done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.Cmd == nil || p.Cmd.Process == nil {
		return fmt.Errorf("guardian process identity is unavailable")
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Kill(-p.Cmd.Process.Pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("observe guardian group: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type Status struct {
	GuardianPID int    `json:"guardian_pid,omitempty"`
	PID         int    `json:"pid,omitempty"`
	Exit        *int   `json:"exit,omitempty"`
	Error       string `json:"error,omitempty"`
}
type Process struct {
	PID    int // Agent PID for diagnostics; control remains on the guardian pipe.
	Cmd    *exec.Cmd
	Input  io.WriteCloser
	Output io.ReadCloser
	Stderr io.ReadCloser
	life   *os.File
	status *os.File
	mu     sync.Mutex
	once   sync.Once
	Done   chan struct{}
	Exit   int
}

func Start(argv []string, cwd string, env []string) (*Process, error) {
	return StartRegistered(argv, cwd, env, nil)
}

// StartRegistered keeps the guardian behind a launch barrier until register
// persists its group identity. Owner death or an uncertain registration cannot
// start the Agent. The callback must not retain argv/environment in metadata.
func StartRegistered(argv []string, cwd string, env []string, register func(int) error) (*Process, error) {
	exe, e := os.Executable()
	if e != nil {
		return nil, e
	}
	lr, lw, e := os.Pipe()
	if e != nil {
		return nil, e
	}
	sr, sw, e := os.Pipe()
	if e != nil {
		lr.Close()
		lw.Close()
		return nil, e
	}
	args := append([]string{"_guard"}, argv...)
	cmd := exec.Command(exe, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{lr, sw}
	p := &Process{Cmd: cmd, life: lw, status: sr, Done: make(chan struct{}), Exit: -1}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var inputR, outputW, stderrW *os.File
	inputR, inputW, er := os.Pipe()
	e = er
	if e == nil {
		p.Input = inputW
		cmd.Stdin = inputR
	}
	if e == nil {
		var outputR *os.File
		outputR, outputW, e = os.Pipe()
		p.Output = outputR
		cmd.Stdout = outputW
	}
	if e == nil {
		var stderrR *os.File
		stderrR, stderrW, e = os.Pipe()
		p.Stderr = stderrR
		cmd.Stderr = stderrW
	}
	if e == nil {
		e = cmd.Start()
	}
	if inputR != nil {
		inputR.Close()
	}
	if outputW != nil {
		outputW.Close()
	}
	if stderrW != nil {
		stderrW.Close()
	}
	lr.Close()
	sw.Close()
	if e != nil {
		p.Close()
		sr.Close()
		if p.Output != nil {
			p.Output.Close()
		}
		if p.Stderr != nil {
			p.Stderr.Close()
		}
		return nil, e
	}
	dec := json.NewDecoder(sr)
	var st Status
	_ = sr.SetReadDeadline(time.Now().Add(5 * time.Second))
	e = dec.Decode(&st)
	if e == nil && st.GuardianPID != cmd.Process.Pid {
		e = fmt.Errorf("guardian launch identity does not match its parent")
	}
	if e == nil && register != nil {
		e = register(st.GuardianPID)
	}
	if e == nil {
		_ = lw.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, e = fmt.Fprintln(lw, "start")
	}
	if e == nil {
		_ = sr.SetReadDeadline(time.Now().Add(5 * time.Second))
		e = dec.Decode(&st)
	}
	if e != nil || st.Error != "" {
		p.Close()
		cmd.Wait()
		sr.Close()
		p.Output.Close()
		p.Stderr.Close()
		return nil, fmt.Errorf("guardian start: %v %s", e, st.Error)
	}
	_ = sr.SetReadDeadline(time.Time{})
	_ = lw.SetWriteDeadline(time.Time{})
	p.PID = st.PID
	go func() {
		var end Status
		if dec.Decode(&end) == nil && end.Exit != nil {
			p.Exit = *end.Exit
		}
		cmd.Wait()
		sr.Close()
		close(p.Done)
	}()
	return p, nil
}
func (p *Process) Write(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f, ok := p.Input.(*os.File); ok {
		_ = f.SetWriteDeadline(time.Now().Add(5 * time.Second))
	}
	_, e := p.Input.Write(b)
	return e
}
func (p *Process) Signal(sig string) error {
	n := map[string]syscall.Signal{"INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "HUP": syscall.SIGHUP, "QUIT": syscall.SIGQUIT}[sig]
	if n == 0 {
		return fmt.Errorf("unsupported signal")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.Done:
		return fmt.Errorf("process exited")
	default:
	}
	_, e := fmt.Fprintln(p.life, strconv.Itoa(int(n)))
	return e
}
func (p *Process) Close() {
	p.once.Do(func() {
		if p.life != nil {
			p.life.Close()
		}
		if p.Input != nil {
			p.Input.Close()
		}
	})
}

// Guard must run only in the dedicated child command. fd3 is an inherited
// ownership/control pipe; EOF means owner death. fd4 reports real child status.
func Guard(args []string) int {
	if len(args) < 1 {
		return 125
	}
	life := os.NewFile(3, "owner")
	status := os.NewFile(4, "status")
	if life == nil || status == nil {
		return 125
	}
	if syscall.Getpgrp() != os.Getpid() {
		return 125
	}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for range signals {
		}
	}()
	enc := json.NewEncoder(status)
	if enc.Encode(Status{GuardianPID: os.Getpid()}) != nil {
		return 125
	}
	var command string
	if _, err := fmt.Fscanln(life, &command); err != nil || command != "start" {
		return 125
	}
	go func() {
		for {
			var n int
			if _, e := fmt.Fscanln(life, &n); e != nil {
				syscall.Kill(-os.Getpid(), syscall.SIGKILL)
				return
			}
			syscall.Kill(-os.Getpid(), syscall.Signal(n))
		}
	}()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if e := cmd.Start(); e != nil {
		_ = enc.Encode(Status{Error: e.Error()})
		return 127
	}
	_ = enc.Encode(Status{PID: cmd.Process.Pid})
	e := cmd.Wait()
	code := 0
	if e != nil {
		code = cmd.ProcessState.ExitCode()
	}
	_ = enc.Encode(Status{Exit: &code})
	status.Close()
	syscall.Kill(-os.Getpid(), syscall.SIGKILL)
	return code
}
