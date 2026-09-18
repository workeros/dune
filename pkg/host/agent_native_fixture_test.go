package host

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aiomni/dune/internal/agentintegration"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/google/uuid"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runHostNativeFixture() int {
	id := os.Getenv("DUNE_HOST_NATIVE_ID")
	args := os.Args[1:]
	if len(args) != 4 || (args[0] != "--settings" && args[0] != "-c") {
		return 2
	}
	if !exerciseHostNativeMCP(args[2:]) {
		return 9
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 3
	}
	log, err := os.OpenFile(os.Getenv("DUNE_HOST_NATIVE_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 4
	}
	err = json.NewEncoder(log).Encode(map[string]string{"id": id, "cwd": cwd, "value": os.Getenv("DUNE_HOST_NATIVE_VALUE")})
	log.Close()
	if err != nil {
		return 5
	}
	input, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": id, "cwd": cwd, "transcript_path": "/private/native.jsonl"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Getenv(agentintegration.HelperEnv), agentintegration.SessionCommand)
	command.Env = append(os.Environ(), "CODEX_THREAD_ID="+id)
	command.Stdin = strings.NewReader(string(input))
	if output, err := command.CombinedOutput(); err != nil || len(output) != 0 {
		return 8
	}
	for {
		time.Sleep(time.Hour)
	} // tmux owns termination; no model is invoked.
}

func nativeFixtureProfile(t *testing.T, f executorFixture, agent string) api.Profile {
	t.Helper()
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(program)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.workspace, agent)
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	return api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: f.workspace, Start: api.Command{Argv: []string{path}}, Env: map[string]string{"DUNE_HOST_FAKE_NATIVE": "1", "DUNE_HOST_NATIVE_ID": uuid.NewString(), "DUNE_HOST_NATIVE_LOG": filepath.Join(f.workspace, "native-launches"), "DUNE_HOST_NATIVE_VALUE": "saved"}}
}

func awaitNativeRuntime(t *testing.T, f executorFixture, id string) agents.Agent {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if item.Runtime.ID == id && item.Runtime.NativeSession != nil {
				return item
			}
		}
	}
	t.Fatal("native session was not observed")
	return agents.Agent{}
}
func errorCode(err error) string {
	var failure *api.Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}
func stopAgentRuntime(t *testing.T, connection *client.Client, runtime api.Runtime) {
	t.Helper()
	if err := connection.Stop(t.Context(), runtime); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		current, err := connection.Get(t.Context(), runtime)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == "exited" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Runtime did not exit")
		}
	}
}
