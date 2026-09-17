package fabricd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/agentintegration"
	"github.com/google/uuid"
)

func TestNativeSessionHookProcessIsSilentAndDoesNotNeedMachineConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binding := agentintegration.Binding{RuntimeID: "runtime", Incarnation: "boot", Agent: "claude"}
	if err := agentintegration.Prepare(dir, binding); err != nil {
		t.Fatal(err)
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	data, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": id, "cwd": "/source", "transcript_path": "/private/transcript.jsonl", "prompt": "must not echo this value"})
	command := exec.CommandContext(t.Context(), program, agentintegration.SessionCommand)
	command.Env = []string{agentintegration.SessionDirEnv + "=" + dir}
	command.Stdin = bytes.NewReader(data)
	out, err := command.CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatal("hook failed or produced Agent-visible output", err, string(out))
	}
	got, err := agentintegration.Read(dir, binding.RuntimeID, binding.Incarnation)
	if err != nil || got.ID != id || got.Cwd != "/source" || got.Sequence != 1 {
		t.Fatal(got, err)
	}
}

func TestNativeSessionHookProcessBoundsUnclosedInput(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := agentintegration.Prepare(dir, agentintegration.Binding{RuntimeID: "runtime", Incarnation: "boot", Agent: "codex"}); err != nil {
		t.Fatal(err)
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, program, agentintegration.SessionCommand)
	command.Env = []string{agentintegration.SessionDirEnv + "=" + dir}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write([]byte("{")); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil || ctx.Err() != nil || output.Len() != 0 {
		t.Fatal("native hook blocked the CLI or echoed input", err, ctx.Err(), output.String())
	}
}
