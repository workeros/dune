package api

import "testing"

func TestProfileValidateKinds(t *testing.T) {
	environment := Profile{Version: 1, Kind: "environment", WorkingDirectory: "/workspace"}
	environment.Setup.Steps = []Command{{Argv: []string{"/bin/true"}}}
	if err := environment.Validate(); err != nil {
		t.Fatal("valid environment Profile rejected", err)
	}

	withStart := environment
	withStart.Start = Command{Argv: []string{"/bin/true"}}
	if err := withStart.Validate(); err == nil {
		t.Fatal("environment Profile accepted an Agent start command")
	}

	withAdapter := environment
	withAdapter.Adapter = "pty"
	if err := withAdapter.Validate(); err == nil {
		t.Fatal("environment Profile accepted an Agent adapter")
	}

	agent := Profile{Version: 1, Kind: "agent", WorkingDirectory: "/workspace", Adapter: "pty", Start: Command{Argv: []string{"/bin/sh"}}}
	if err := agent.Validate(); err != nil {
		t.Fatal("valid agent Profile rejected", err)
	}
	agent.Start = Command{}
	if err := agent.Validate(); err == nil {
		t.Fatal("agent Profile accepted a missing start command")
	}
}
