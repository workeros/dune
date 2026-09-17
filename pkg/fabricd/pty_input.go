package fabricd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/pkg/api"
)

const maxPTYInputs = 64

type ptyInputTask struct {
	write func(*tmux.Viewer) error
	ref   string
	done  chan error
}

// All browser bytes and Agent submissions use one tmux client input stream.
// Mixing viewer writes with tmux send-keys would not preserve their order at
// the pane: the two clients can be scheduled independently by tmux.
type ptyInputQueue struct {
	mu         sync.Mutex
	r          *runtime
	ctx        context.Context
	cancel     context.CancelFunc
	tasks      chan ptyInputTask
	done       chan struct{}
	closed     bool
	operations *operationLog
}

func (r *runtime) inputQueue(parent context.Context) *ptyInputQueue {
	operations := r.operationLog()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ptyInput == nil {
		ctx, cancel := context.WithCancel(parent)
		r.ptyInput = &ptyInputQueue{r: r, ctx: ctx, cancel: cancel, tasks: make(chan ptyInputTask, maxPTYInputs), done: make(chan struct{}), operations: operations}
		go r.ptyInput.run()
	}
	return r.ptyInput
}

func (r *runtime) closePTYInput() {
	r.mu.Lock()
	queue := r.ptyInput
	r.mu.Unlock()
	if queue != nil {
		queue.cancel()
		<-queue.done
	}
}

func (q *ptyInputQueue) submit(write func(*tmux.Viewer) error, track bool) (api.AgentOperation, <-chan error, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.ctx.Err() != nil {
		return api.AgentOperation{}, nil, &api.Error{Code: "STALE_RUNTIME", Detail: "PTY input service has closed"}
	}
	if len(q.tasks) >= cap(q.tasks) {
		return api.AgentOperation{}, nil, &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "PTY input queue is full"}
	}
	var status api.AgentOperation
	var err error
	if track {
		status, err = q.operations.create()
		if err != nil {
			return status, nil, err
		}
	}
	done := make(chan error, 1)
	q.tasks <- ptyInputTask{write: write, ref: status.Ref, done: done}
	return status, done, nil
}

func (q *ptyInputQueue) run() {
	var viewer *tmux.Viewer
	defer func() {
		q.mu.Lock()
		q.closed = true
		for len(q.tasks) > 0 {
			task := <-q.tasks
			q.operations.set(task.ref, "cancelled", "", "fabricd input service closed; request was not sent")
			task.done <- &api.Error{Code: "STALE_RUNTIME", Detail: "PTY input service closed"}
		}
		q.mu.Unlock()
		if viewer != nil {
			viewer.Close()
		}
		close(q.done)
	}()
	for {
		select {
		case <-q.ctx.Done():
			return
		case task := <-q.tasks:
			if q.ctx.Err() != nil {
				q.operations.set(task.ref, "cancelled", "", "fabricd input service closed; request was not sent")
				task.done <- q.ctx.Err()
				return
			}
			q.operations.set(task.ref, "running", "", "")
			var err error
			if viewer == nil {
				viewer, err = q.attachInput()
			}
			if err == nil {
				err = task.write(viewer)
			}
			state, detail := "delivered", ""
			if err != nil {
				state, detail = "failed", err.Error()
				if failure, ok := err.(*api.Error); ok && failure.Code == "RESULT_UNKNOWN" {
					state = "unknown"
				}
			}
			q.operations.set(task.ref, state, "", detail)
			task.done <- err
		}
	}
}

func (q *ptyInputQueue) attachInput() (*tmux.Viewer, error) {
	viewer, err := q.r.tmux.Attach(false)
	if err != nil {
		return nil, err
	}
	ready := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4096)
		_, err := viewer.Read(buffer)
		ready <- err
		if err == nil {
			_, _ = io.Copy(io.Discard, viewer)
		}
	}()
	// tmux switches the client terminal to raw mode before drawing. Waiting for
	// that first output prevents its startup flush from discarding early input.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err = <-ready:
	case <-q.ctx.Done():
		err = q.ctx.Err()
	case <-timer.C:
		err = fmt.Errorf("tmux input client did not become ready")
	}
	if err != nil {
		viewer.Close()
		return nil, err
	}
	return viewer, nil
}

func (q *ptyInputQueue) write(ctx context.Context, write func(*tmux.Viewer) error) error {
	_, done, err := q.submit(write, false)
	if err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func writePTY(viewer *tmux.Viewer, data []byte) error {
	if err := viewer.Write(data); err != nil {
		return &api.Error{Code: "RESULT_UNKNOWN", Detail: "terminal write outcome unknown: " + err.Error()}
	}
	return nil
}

func supportedPTYAgent(agent string) bool {
	return agent == "claude" || agent == "codex" || agent == "opencode" || agent == "gemini"
}

func (q *ptyInputQueue) checkAgent(agent string, prompt bool) (tmux.Pane, error) {
	if !supportedPTYAgent(agent) {
		return tmux.Pane{}, &api.Error{Code: "UNSUPPORTED", Detail: "PTY Agent is not recognized"}
	}
	info := q.r.info()
	if info.State != "running" {
		return tmux.Pane{}, &api.Error{Code: "STALE_RUNTIME", Detail: "PTY Agent has exited"}
	}
	if prompt && info.Activity != nil && info.Activity.State == "blocked" {
		return tmux.Pane{}, &api.Error{Code: "AGENT_BLOCKED", Detail: "Agent requires native interaction; use send_keys explicitly"}
	}
	pane, err := q.r.tmux.Inspect()
	if err != nil {
		return pane, err
	}
	if pane.Dead || filepath.Base(pane.Command) != agent {
		return pane, &api.Error{Code: "STALE_AGENT", Detail: "foreground process is no longer the selected Agent"}
	}
	if prompt && pane.InMode {
		return pane, &api.Error{Code: "TERMINAL_IN_HISTORY", Detail: "leave terminal history before submitting a prompt"}
	}
	if prompt && !pane.BracketedPaste {
		return pane, &api.Error{Code: "AGENT_NOT_READY", Detail: "foreground Agent has not enabled bracketed paste"}
	}
	return pane, nil
}

func (q *ptyInputQueue) prompt(request api.PTYPrompt) (api.AgentOperation, error) {
	request.Text = strings.ReplaceAll(request.Text, "\r\n", "\n")
	if request.Text == "" || len(request.Text) > 64*1024 || !utf8.ValidString(request.Text) || strings.ContainsFunc(request.Text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return api.AgentOperation{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "prompt requires 1..65536 UTF-8 bytes without terminal control characters"}
	}
	if !supportedPTYAgent(request.Agent) {
		return api.AgentOperation{}, &api.Error{Code: "UNSUPPORTED", Detail: "PTY Agent is not recognized"}
	}
	status, _, err := q.submit(func(viewer *tmux.Viewer) error {
		before, err := q.checkAgent(request.Agent, true)
		if err != nil {
			return err
		}
		if err := writePTY(viewer, []byte("\x1b[200~"+request.Text+"\x1b[201~")); err != nil {
			return err
		}
		// Give the native CLI a chance to process paste before Enter. This whole
		// group occupies the queue so browser keys cannot enter between them.
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-q.ctx.Done():
			return &api.Error{Code: "RESULT_UNKNOWN", Detail: "input service closed after text; Enter was not confirmed"}
		case <-timer.C:
		}
		after, err := q.checkAgent(request.Agent, true)
		if err != nil || after.PID != before.PID {
			return &api.Error{Code: "RESULT_UNKNOWN", Detail: "foreground changed after text; Enter was not sent"}
		}
		return writePTY(viewer, []byte{'\r'})
	}, true)
	return status, err
}

var terminalKeys = map[string]string{
	"Enter": "\r", "Tab": "\t", "Escape": "\x1b", "Backspace": "\x7f", "Delete": "\x1b[3~",
	"Up": "\x1b[A", "Down": "\x1b[B", "Right": "\x1b[C", "Left": "\x1b[D",
	"Home": "\x1b[H", "End": "\x1b[F", "PageUp": "\x1b[5~", "PageDown": "\x1b[6~",
	"Ctrl+C": "\x03", "Ctrl+D": "\x04", "Ctrl+U": "\x15", "Ctrl+L": "\x0c",
}

func (q *ptyInputQueue) keys(request api.PTYKeys) (api.AgentOperation, error) {
	if len(request.Keys) == 0 || len(request.Keys) > 32 {
		return api.AgentOperation{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "1..32 named keys required"}
	}
	var data []byte
	for _, key := range request.Keys {
		value, ok := terminalKeys[key]
		if !ok {
			return api.AgentOperation{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: fmt.Sprintf("unsupported terminal key %q", key)}
		}
		data = append(data, []byte(value)...)
	}
	status, _, err := q.submit(func(viewer *tmux.Viewer) error {
		if _, err := q.checkAgent(request.Agent, false); err != nil {
			return err
		}
		return writePTY(viewer, data)
	}, true)
	return status, err
}
