package fabricd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// A native test process records terminal bytes without acting as an AI Agent.
func TestPTYInputRecorder(t *testing.T) {
	path := os.Getenv("DUNE_TEST_PTY_INPUT_RECORD")
	if path == "" {
		return
	}
	command := exec.Command("/bin/stty", "raw", "-echo")
	command.Stdin = os.Stdin
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fmt.Print("\x1b[?2004hPTY_RECORDER_READY")
	_, _ = io.Copy(file, os.Stdin)
}

func ptyInputFixture(t *testing.T) (*runtime, *ptyInputQueue, string) {
	t.Helper()
	dir := t.TempDir()
	server, err := tmux.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dir, "codex")
	if err = os.WriteFile(agent, program, 0700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "input")
	meta := api.Runtime{ID: wire.ID(), Incarnation: wire.ID(), Generation: 1, Adapter: "pty", WorkingDirectory: dir}
	session, err := server.Create(meta, []string{agent, "-test.run=^TestPTYInputRecorder$"}, []string{"PATH=/usr/bin:/bin", "DUNE_TEST_PTY_INPUT_RECORD=" + record}, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := &runtime{id: meta.ID, inc: meta.Incarnation, adapter: "pty", cwd: dir, tmux: session, done: make(chan struct{}), subs: map[*subscription]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	queue := r.inputQueue(ctx)
	t.Cleanup(func() { cancel(); r.closePTYInput(); _ = server.Close() })
	awaitPTY(t, func() bool {
		capture, err := session.Capture()
		return err == nil && strings.Contains(capture.Content, "PTY_RECORDER_READY")
	})
	return r, queue, record
}

func awaitPTY(t *testing.T, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if check() {
			return
		}
	}
	t.Fatal("PTY state did not settle")
}

func waitPTYOperation(t *testing.T, q *ptyInputQueue, operation api.AgentOperation) api.AgentOperation {
	t.Helper()
	result, err := q.operations.wait(context.Background(), api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 5000})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPTYQueueKeepsPasteEnterAndBrowserBytesTogether(t *testing.T) {
	_, queue, record := ptyInputFixture(t)
	if err := queue.write(context.Background(), func(viewer *tmux.Viewer) error { return writePTY(viewer, []byte("draft:")) }); err != nil {
		t.Fatal(err)
	}
	operation, err := queue.prompt(api.PTYPrompt{Agent: "codex", Text: "first line\n第二行"})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.write(context.Background(), func(viewer *tmux.Viewer) error { return writePTY(viewer, []byte("after:")) }); err != nil {
		t.Fatal(err)
	}
	if result := waitPTYOperation(t, queue, operation); result.State != "delivered" {
		t.Fatalf("prompt: %+v", result)
	}
	keys, err := queue.keys(api.PTYKeys{Agent: "codex", Keys: []string{"Enter", "Ctrl+C"}})
	if err != nil {
		t.Fatal(err)
	}
	if result := waitPTYOperation(t, queue, keys); result.State != "delivered" {
		t.Fatalf("keys: %+v", result)
	}
	defer func() {
		if t.Failed() {
			data, _ := os.ReadFile(record)
			t.Logf("recorded input: %q", data)
		}
	}()
	want := []byte("draft:\x1b[200~first line\n第二行\x1b[201~\rafter:\r\x03")
	awaitPTY(t, func() bool { data, _ := os.ReadFile(record); return bytes.Equal(data, want) })
}

func TestPTYQueueRejectsWrongForegroundBlockedAndHistory(t *testing.T) {
	r, queue, record := ptyInputFixture(t)
	wrong, err := queue.prompt(api.PTYPrompt{Agent: "claude", Text: "do not send"})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPTYOperation(t, queue, wrong); got.State != "failed" || !strings.Contains(got.Error, "STALE_AGENT") {
		t.Fatalf("foreground: %+v", got)
	}
	r.updateActivity("blocked", "hook", "codex", "codex")
	blocked, err := queue.prompt(api.PTYPrompt{Agent: "codex", Text: "do not approve"})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPTYOperation(t, queue, blocked); got.State != "failed" || !strings.Contains(got.Error, "AGENT_BLOCKED") {
		t.Fatalf("blocked: %+v", got)
	}
	r.updateActivity("unknown", "foreground", "codex", "codex")
	if err := r.tmux.History("older"); err != nil {
		t.Fatal(err)
	}
	history, err := queue.prompt(api.PTYPrompt{Agent: "codex", Text: "do not send to history"})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitPTYOperation(t, queue, history); got.State != "failed" || !strings.Contains(got.Error, "TERMINAL_IN_HISTORY") {
		t.Fatalf("history: %+v", got)
	}
	if data, err := os.ReadFile(record); err != nil || len(data) != 0 {
		t.Fatalf("rejected prompt wrote bytes: %q %v", data, err)
	}
	for _, text := range []string{"", "escape\x1b", "interrupt\x03", "carriage\rreturn"} {
		if _, err := queue.prompt(api.PTYPrompt{Agent: "codex", Text: text}); err == nil {
			t.Fatalf("terminal controls accepted: %q", text)
		}
	}
}

func TestPTYQueueAdmissionAndShutdownAreBounded(t *testing.T) {
	_, queue, _ := ptyInputFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	_, done, err := queue.submit(func(*tmux.Viewer) error { close(entered); <-release; return nil }, false)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	for i := 0; i < maxPTYInputs; i++ {
		if _, _, err := queue.submit(func(*tmux.Viewer) error { return nil }, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := queue.submit(func(*tmux.Viewer) error { return nil }, false); err == nil {
		t.Fatal("unbounded input admission")
	}
	queue.cancel()
	close(release)
	<-done
	<-queue.done
	if _, _, err := queue.submit(func(*tmux.Viewer) error { return nil }, false); err == nil {
		t.Fatal("closed queue accepted input")
	}
}
