package fabricd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/google/uuid"
)

// This fixture uses the actual generated Claude settings and executes the
// configured hook as a native CLI would. It does not call any model provider.
func runNativeCLIFixture() int {
	if len(os.Args) != 3 || os.Args[1] != "--settings" {
		return 2
	}
	var settings struct {
		Hooks map[string][]struct{ Hooks []struct{ Command string } }
	}
	if json.Unmarshal([]byte(os.Args[2]), &settings) != nil {
		return 3
	}
	hooks := settings.Hooks["SessionStart"]
	if len(hooks) != 1 || len(hooks[0].Hooks) != 1 {
		return 4
	}
	command := hooks[0].Hooks[0].Command
	previous := ""
	for {
		data, err := os.ReadFile(os.Getenv("DUNE_TEST_NATIVE_EVENT"))
		if err == nil && string(data) != previous {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			child := exec.CommandContext(ctx, "/bin/sh", "-c", command)
			child.Stdin = strings.NewReader(string(data))
			output, err := child.CombinedOutput()
			cancel()
			if err != nil || len(output) != 0 {
				return 5
			}
			previous = string(data)
			if err := os.WriteFile(os.Getenv("DUNE_TEST_NATIVE_EVENT")+".ack", data, 0600); err != nil {
				return 6
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPTYNativeSessionSurvivesFabricdAndTracksOfflineSwitch(t *testing.T) {
	dir := t.TempDir()
	engine, err := Open(t.Context(), filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close(); _ = engine.tmux.Close() })
	connection, ctx := timeoutClient(t, engine)
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(program)
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dir, "claude")
	if err := os.WriteFile(agent, data, 0700); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(dir, "event")
	writeEvent := func(id, cwd string) {
		t.Helper()
		data, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": id, "cwd": cwd, "transcript_path": "/private/session.jsonl"})
		if err := os.WriteFile(eventPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	first, second := uuid.NewString(), uuid.NewString()
	writeEvent(first, dir)
	runtime, stream, err := connection.Start(ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: dir, Start: api.Command{Argv: []string{agent}}, Env: map[string]string{"DUNE_TEST_NATIVE_CLI": "1", "DUNE_TEST_NATIVE_EVENT": eventPath}})
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	waitTimeoutTest(t, func() bool {
		value, err := connection.Get(ctx, runtime)
		return err == nil && value.NativeSession != nil && value.NativeSession.ID == first && value.NativeSession.Sequence == 1
	})
	connection.Close()
	engine.Close()
	writeEvent(second, "/changed-native-cwd")
	waitTimeoutTest(t, func() bool {
		data, err := os.ReadFile(eventPath + ".ack")
		return err == nil && strings.Contains(string(data), second)
	})
	restored, err := Open(t.Context(), filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restored.Close() })
	current, ctx := timeoutClient(t, restored)
	value, err := current.Get(ctx, runtime)
	if err != nil || value.NativeSession == nil || value.NativeSession.ID != second || value.NativeSession.Cwd != "/changed-native-cwd" || value.NativeSession.Sequence != 2 {
		t.Fatal(value, err)
	}
	if err := current.Stop(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "sessions", "native-agents"))
	if err != nil || len(entries) != 0 {
		t.Fatal("stop retained native hook files", entries, err)
	}
}
