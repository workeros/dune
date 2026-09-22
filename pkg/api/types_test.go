package api

import (
	"encoding/json"
	"strings"
	"testing"
)

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

func TestValidateExecutionID(t *testing.T) {
	for _, valid := range []string{"attempt-1", "01J8YABCDEF0123456789ABCDE"} {
		if err := ValidateExecutionID(valid); err != nil {
			t.Fatal("valid execution ID rejected", valid, err)
		}
	}
	for _, invalid := range []string{"", "line\nbreak", string(make([]byte, 129))} {
		if err := ValidateExecutionID(invalid); err == nil {
			t.Fatal("invalid execution ID accepted", len(invalid))
		}
	}
}

func TestProfileElicitationRequiresManagedACP(t *testing.T) {
	for _, test := range []struct {
		name, kind, adapter string
		managed, valid      bool
	}{
		{name: "managed ACP", kind: "agent", adapter: "acp", managed: true, valid: true},
		{name: "raw ACP", kind: "agent", adapter: "acp"},
		{name: "PTY", kind: "agent", adapter: "pty"},
		{name: "environment", kind: "environment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := Profile{Version: 1, Kind: test.kind, Adapter: test.adapter, ManagedACP: test.managed, ACPElicitation: true, WorkingDirectory: "/workspace"}
			if test.kind == "agent" {
				profile.Start = Command{Argv: []string{"/bin/cat"}}
			}
			if err := profile.Validate(); (err == nil) != test.valid {
				t.Fatalf("valid=%v: %v", test.valid, err)
			}
		})
	}
}

func TestProfileValidateSizeLimit(t *testing.T) {
	p := Profile{Version: 1, Kind: "environment", WorkingDirectory: "/workspace", Env: map[string]string{"TOO_LARGE": strings.Repeat("x", MaxProfileBytes)}}
	if err := p.Validate(); err == nil {
		t.Fatal("oversized Profile accepted")
	}
}

func TestProfileValidateStepNameLimit(t *testing.T) {
	p := Profile{Version: 1, Kind: "environment", WorkingDirectory: "/workspace"}
	p.Setup.Steps = []Command{{Name: strings.Repeat("n", MaxProfileStepNameBytes+1), Argv: []string{"/bin/true"}}}
	if err := p.Validate(); err == nil {
		t.Fatal("oversized setup step name accepted")
	}
}

func TestErrorPayloadIsNotGenericJSON(t *testing.T) {
	encoded, err := json.Marshal(&Error{Code: "SETUP_FAILED", Detail: "failed", Payload: json.RawMessage(`{"stdout":"private"}`)})
	if err != nil || strings.Contains(string(encoded), "private") {
		t.Fatal("private protocol error payload was serialized", string(encoded), err)
	}
}
