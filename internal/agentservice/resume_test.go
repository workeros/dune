package agentservice

import (
	"testing"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/google/uuid"
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

func TestNativePTYResumeUsesExactIdentityAndConfirmedDirectory(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		session := agents.Session{Launch: agents.LaunchSnapshot{Profile: api.Profile{Adapter: "pty", WorkingDirectory: "/launch", Start: api.Command{Argv: []string{"/bin/" + agent}}}, Recovery: agents.RecoveryAdapter{ID: "pty-" + agent, Version: 1}}}
		session.Native = &agents.NativeSession{ID: uuid.NewString(), Cwd: "/native"}
		profile, err := resumeProfile(session)
		if err != nil || profile.WorkingDirectory != "/native" || profile.Start.Argv[2] != session.Native.ID || len(session.Launch.Profile.Start.Argv) != 1 || session.Launch.Profile.WorkingDirectory != "/launch" {
			t.Fatal(profile, err)
		}
		for _, bad := range []string{"--last", "latest", "", "../session"} {
			session.Native.ID = bad
			if _, err := resumeProfile(session); err == nil {
				t.Fatal("accepted non-specific native resume", bad)
			}
		}
	}
}
