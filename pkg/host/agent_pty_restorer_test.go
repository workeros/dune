package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/agentintegration"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/google/uuid"
)

func runHostNativeFixture() int {
	mode, id := "start", os.Getenv("DUNE_HOST_NATIVE_ID")
	args := os.Args[1:]
	if len(args) == 4 && (args[0] == "--resume" || args[0] == "resume") {
		mode, id, args = "resume", args[1], args[2:]
	}
	if len(args) != 2 || (args[0] != "--settings" && args[0] != "-c") {
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 3
	}
	log, err := os.OpenFile(os.Getenv("DUNE_HOST_NATIVE_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 4
	}
	err = json.NewEncoder(log).Encode(map[string]string{"mode": mode, "id": id, "cwd": cwd, "value": os.Getenv("DUNE_HOST_NATIVE_VALUE")})
	log.Close()
	if err != nil {
		return 5
	}
	if mode == "resume" {
		for {
			behavior, _ := os.ReadFile(os.Getenv("DUNE_HOST_NATIVE_BEHAVIOR"))
			switch string(behavior) {
			case "fail":
				return 7
			case "wrong":
				id = uuid.NewString()
			case "wait":
				time.Sleep(10 * time.Millisecond)
				continue
			}
			break
		}
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

func nativeRecoveryProfile(t *testing.T, f executorFixture, agent string) api.Profile {
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
	return api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: f.workspace, Start: api.Command{Argv: []string{path}}, Env: map[string]string{"DUNE_HOST_FAKE_NATIVE": "1", "DUNE_HOST_NATIVE_ID": uuid.NewString(), "DUNE_HOST_NATIVE_LOG": filepath.Join(f.workspace, "native-launches"), "DUNE_HOST_NATIVE_VALUE": "saved", "DUNE_HOST_NATIVE_BEHAVIOR": filepath.Join(f.workspace, "native-behavior")}}
}

func awaitNativeIndex(t *testing.T, f executorFixture, sessionID string) agents.Summary {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if item.Session != nil && item.Session.ID == sessionID && item.Session.Attempt.State == "ready" && item.Session.Native != nil {
				return *item.Session
			}
		}
	}
	t.Fatal("native session was not indexed")
	return agents.Summary{}
}

func TestPTYRestorerUsesOriginalProfileAndOneConcurrentAttempt(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			f := openExecutorFixture(t)
			profile := nativeRecoveryProfile(t, f, agent)
			profile.Setup.Steps = []api.Command{{Run: `printf 'setup\n' >> setup-log`, Shell: "/bin/sh"}}
			saved, err := f.app.store.Profiles().Create(t.Context(), profiles.Record{OwnerID: f.owner, Name: "Native", Profile: profile, CreatedBy: profiles.Actor{Type: "user", Subject: f.principal.ID}})
			if err != nil {
				t.Fatal(err)
			}
			started, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Profile: &profiles.Selection{ID: saved.ID, Revision: saved.Revision}})
			if err != nil {
				t.Fatal(err)
			}
			session := awaitNativeIndex(t, f, started.Session.ID)
			request := agents.ResumeRequest{SessionID: session.ID, Revision: session.Revision}
			if _, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request); errorCode(err) != "RUNTIME_ALIVE" {
				t.Fatal("resumed an active native CLI", err)
			}
			connection, closeConnection, err := (&runnerExecutor{app: f.app}).connect(t.Context(), f.principal, f.owner, f.binding, "runtime.stop")
			if err != nil {
				t.Fatal(err)
			}
			defer closeConnection()
			if err := connection.Stop(t.Context(), *started.Runtime); err != nil {
				t.Fatal(err)
			}
			saved.Profile.Env["DUNE_HOST_NATIVE_VALUE"] = "changed"
			saved.Profile.Start.Argv = []string{"/missing/changed-command"}
			saved, err = f.app.store.Profiles().Update(t.Context(), saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.app.store.Profiles().Delete(t.Context(), f.owner, profiles.Selection{ID: saved.ID, Revision: saved.Revision}); err != nil {
				t.Fatal(err)
			}
			f.app.agentEnvironment = func(context.Context, agents.Scope, runner.Binding, map[string]string) (map[string]string, error) {
				return nil, errors.New("resume must use saved environment")
			}
			results := make([]agents.ResumeResult, 2)
			failures := make([]error, 2)
			var wg sync.WaitGroup
			for i := range 2 {
				wg.Go(func() { results[i], failures[i] = f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request) })
			}
			wg.Wait()
			for i, result := range results {
				if failures[i] != nil || result.Session == nil {
					t.Fatal(result, failures[i])
				}
			}
			if results[0].Session.Attempt.ID != results[1].Session.Attempt.ID {
				t.Fatal("created two resume attempts")
			}
			confirmed := awaitNativeIndex(t, f, session.ID)
			if confirmed.Native.ID != session.Native.ID || confirmed.Native.Cwd != session.Native.Cwd || confirmed.Status != "available" {
				t.Fatal(confirmed)
			}
			data, err := os.ReadFile(profile.Env["DUNE_HOST_NATIVE_LOG"])
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 2 {
				t.Fatal("duplicate native startup", string(data))
			}
			var resumed map[string]string
			if json.Unmarshal([]byte(lines[1]), &resumed) != nil || resumed["mode"] != "resume" || resumed["id"] != session.Native.ID || resumed["value"] != "saved" {
				t.Fatal(resumed)
			}
			data, err = os.ReadFile(filepath.Join(f.workspace, "setup-log"))
			if err != nil || string(data) != "setup\n" {
				t.Fatal("setup reran on resume", string(data), err)
			}
		})
	}
}

func TestPTYRestorerRejectsWrongOrMissingNativeConfirmation(t *testing.T) {
	for _, behavior := range []string{"fail", "wrong"} {
		t.Run(behavior, func(t *testing.T) {
			f := openExecutorFixture(t)
			profile := nativeRecoveryProfile(t, f, "claude")
			started, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Custom: &profile})
			if err != nil {
				t.Fatal(err)
			}
			session := awaitNativeIndex(t, f, started.Session.ID)
			connection, closeConnection, err := (&runnerExecutor{app: f.app}).connect(t.Context(), f.principal, f.owner, f.binding, "runtime.stop")
			if err != nil {
				t.Fatal(err)
			}
			defer closeConnection()
			if err := connection.Stop(t.Context(), *started.Runtime); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(profile.Env["DUNE_HOST_NATIVE_BEHAVIOR"], []byte(behavior), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), agents.ResumeRequest{SessionID: session.ID, Revision: session.Revision})
			if errorCode(err) != "RECOVERY_FAILED" || result.Session == nil || result.Session.Attempt.State != "failed" || result.Session.Native.ID != session.Native.ID {
				t.Fatal(result, err)
			}
		})
	}
}

func TestPTYRestorerPendingNativeInteractionCanLaterFailAndRetry(t *testing.T) {
	f := openExecutorFixture(t)
	profile := nativeRecoveryProfile(t, f, "claude")
	started, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Custom: &profile})
	if err != nil {
		t.Fatal(err)
	}
	session := awaitNativeIndex(t, f, started.Session.ID)
	connection, closeConnection, err := (&runnerExecutor{app: f.app}).connect(t.Context(), f.principal, f.owner, f.binding, "runtime.stop")
	if err != nil {
		t.Fatal(err)
	}
	defer closeConnection()
	if err := connection.Stop(t.Context(), *started.Runtime); err != nil {
		t.Fatal(err)
	}
	behavior := profile.Env["DUNE_HOST_NATIVE_BEHAVIOR"]
	if err := os.WriteFile(behavior, []byte("wait"), 0600); err != nil {
		t.Fatal(err)
	}
	request := agents.ResumeRequest{SessionID: session.ID, Revision: session.Revision}
	result, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request)
	if err != nil || result.Runtime == nil || result.Session == nil || result.Session.Attempt.State != "capturing" || result.Operation != nil {
		t.Fatal("native interaction did not remain a known pending Runtime", result, err)
	}
	duplicate, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request)
	if err != nil || duplicate.Runtime == nil || duplicate.Runtime.ID != result.Runtime.ID {
		t.Fatal("pending recovery started another CLI", duplicate, err)
	}
	if err := os.WriteFile(behavior, []byte("fail"), 0600); err != nil {
		t.Fatal(err)
	}
	var failed agents.Session
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{}); err != nil {
			t.Fatal(err)
		}
		failed, err = f.app.store.AgentSession(t.Context(), f.owner, session.ID)
		if err == nil && failed.Attempt.State == "failed" {
			break
		}
	}
	if err != nil || failed.Attempt.State != "failed" {
		t.Fatal("exited pending CLI never became retryable", failed.Attempt, err)
	}
	if err := os.WriteFile(behavior, nil, 0600); err != nil {
		t.Fatal(err)
	}
	retried, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), agents.ResumeRequest{SessionID: session.ID, Revision: failed.Revision})
	if err != nil || retried.Session == nil || retried.Session.Attempt.State != "ready" || retried.Runtime.ID == result.Runtime.ID {
		t.Fatal(retried, err)
	}
}
