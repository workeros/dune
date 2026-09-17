package host

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/agentintegration"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/google/uuid"
)

func TestPTYNativeHookFlowsIntoHostRecoveryIndex(t *testing.T) {
	f := openExecutorFixture(t)
	id := uuid.NewString()
	event := func(id, cwd string) []byte {
		data, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": id, "cwd": cwd, "transcript_path": "/private/native.jsonl"})
		return data
	}
	agent := filepath.Join(f.workspace, "claude")
	// The renderer's settings contract is tested with a parsing CLI fixture in
	// fabricd. Here a real child drives the injected hook across SDK/Gateway.
	script := "#!/bin/sh\nprintf '%s' \"$DUNE_TEST_NATIVE_EVENT\" | \"$DUNE_AGENT_HELPER\" __agent-session\nprintf '%s' \"$DUNE_AGENT_SESSION_DIR\" > native-dir\nsleep 120\n"
	if err := os.WriteFile(agent, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	profile := api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: f.workspace, Start: api.Command{Argv: []string{agent}}, Env: map[string]string{"DUNE_TEST_NATIVE_EVENT": string(event(id, f.workspace))}}
	launched, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Custom: &profile})
	if err != nil {
		t.Fatal(err)
	}
	var current agents.Agent
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		current, err = f.app.AgentDirectory().Get(t.Context(), f.agentScope(), launched.AgentRef)
		if err == nil && current.Session != nil && current.Session.Native != nil {
			break
		}
	}
	if err != nil || current.Session == nil || current.Session.Native == nil || current.Session.Native.ID != id || current.Session.ID != launched.Session.ID || current.RecoveryError != "" {
		t.Fatal(current, err)
	}
	saved, err := f.app.store.AgentSession(t.Context(), f.owner, launched.Session.ID)
	if err != nil || saved.Native == nil || saved.Native.ID != id || !saved.Selected {
		t.Fatal("native confirmation missing from database", saved.Native, err)
	}
	if saved.Launch.Profile.Env[agentintegration.SessionDirEnv] != "" || saved.Launch.Profile.Env[agentintegration.HelperEnv] != "" || len(saved.Launch.Profile.Start.Argv) != 1 {
		t.Fatal("ephemeral injection leaked into launch snapshot")
	}
	var data []byte
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		data, err = os.ReadFile(filepath.Join(f.workspace, "native-dir"))
		if err == nil && len(data) > 0 {
			break
		}
	}
	if err != nil || len(data) == 0 {
		t.Fatal("fixture did not finish its initial hook", err)
	}
	dir := string(data)
	second := uuid.NewString()
	command := exec.CommandContext(t.Context(), filepath.Join(dir, "helper"), agentintegration.SessionCommand)
	command.Env = []string{agentintegration.SessionDirEnv + "=" + dir}
	command.Stdin = strings.NewReader(string(event(second, "/changed-cwd")))
	if output, err := command.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatal("native switch callback failed", err, string(output))
	}
	if _, err := f.app.AgentDirectory().Get(t.Context(), f.agentScope(), current.Ref); err == nil {
		t.Fatal("old reference followed new native session")
	}
	page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Session == nil || page.Items[0].Session.Native == nil || page.Items[0].Session.Native.ID != second || page.Items[0].Session.ID == saved.ID {
		t.Fatal("native switch not indexed", page, err)
	}
	old, err := f.app.store.AgentSession(t.Context(), f.owner, saved.ID)
	if err != nil || old.Selected || old.Native.ID != id {
		t.Fatal("old native identity lost", old.Native, err)
	}
}
