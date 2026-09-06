package fabricd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestAgentConfigurationDoesNotRequireInstalledAgent(t *testing.T) {
	d := newEngine(context.Background())
	d.stateDir = t.TempDir()
	config := api.AgentConfig{Name: "My custom Agent", Command: "/missing/agent-for-configuration-test", Args: []string{"--acp"}, Adapter: "acp", Env: map[string]string{"API_KEY": "test-developer-machine-secret"}}
	result, err := d.agentConfig(api.AgentConfigRequest{Action: "save", Config: &config})
	if err != nil {
		t.Fatal("configuration must be savable without a runnable Agent", err)
	}
	stored := result.(api.AgentConfig)
	if stored.ID == "" || stored.Command != config.Command {
		t.Fatal("configuration did not persist")
	}
	info, err := os.Stat(filepath.Join(d.stateDir, "agents.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("Agent environment file must be private", err)
	}
	result, err = d.agentConfig(api.AgentConfigRequest{Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	list := result.([]api.AgentConfig)
	if len(list) != 1 || list[0].Env["API_KEY"] != config.Env["API_KEY"] {
		t.Fatal("saved config missing")
	}
	stored.Name = "Renamed"
	if _, err = d.agentConfig(api.AgentConfigRequest{Action: "save", Config: &stored}); err != nil {
		t.Fatal(err)
	}
	if _, err = d.agentConfig(api.AgentConfigRequest{Action: "delete", ID: stored.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = d.agentConfig(api.AgentConfigRequest{Action: "save", Config: &stored}); err == nil {
		t.Fatal("stale configuration silently recreated")
	}
}
