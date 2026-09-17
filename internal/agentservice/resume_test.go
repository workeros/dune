package agentservice

import (
	"testing"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
)

func TestResumeProfileOnlyAcceptsRecognizedTransportLaunches(t *testing.T) {
	session := agents.Session{Launch: agents.LaunchSnapshot{Profile: api.Profile{Adapter: "acp", ManagedACP: true, Start: api.Command{Argv: []string{"/opt/agent", "--acp"}}, Env: map[string]string{"SAVED": "exact"}}, Recovery: agents.RecoveryAdapter{ID: "acp-load", Version: 1}}}
	session.Launch.Profile.Setup.Steps = []api.Command{{Argv: []string{"one-time-setup"}}}
	profile, err := resumeProfile(session)
	if err != nil || len(profile.Setup.Steps) != 0 || len(session.Launch.Profile.Setup.Steps) != 1 || profile.Env["SAVED"] != "exact" {
		t.Fatal("resume changed the saved launch or reran setup", err)
	}
	session.Launch.Profile.Start.Argv = append(session.Launch.Profile.Start.Argv, "--prompt", "one-time task")
	if _, err := resumeProfile(session); err == nil {
		t.Fatal("resumed a launch containing a one-time task")
	}
	session.Launch.Profile.Start = api.Command{Run: "agent --acp", Shell: "/bin/sh"}
	if _, err := resumeProfile(session); err == nil {
		t.Fatal("guessed semantics of an opaque shell command")
	}
}
