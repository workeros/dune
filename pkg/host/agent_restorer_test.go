package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

func stopRecoveryRuntime(t *testing.T, connection *client.Client, runtime api.Runtime) {
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

func TestAgentRestorerUsesSnapshotAndDeduplicatesConcurrentContinue(t *testing.T) {
	f := openExecutorFixture(t)
	profile := directoryACPProfile(t, f)
	logPath := filepath.Join(f.workspace, "launches")
	methodsPath := filepath.Join(f.workspace, "methods")
	program, err := os.ReadFile(profile.Start.Argv[0])
	if err != nil {
		t.Fatal(err)
	}
	program = []byte(strings.Replace(string(program), "exec ", "printf 'launch:%s\\n' \"$SAVED_VALUE\" >> \"$LAUNCH_LOG\"\nexec ", 1))
	if err := os.WriteFile(profile.Start.Argv[0], program, 0700); err != nil {
		t.Fatal(err)
	}
	profile.Env["LAUNCH_LOG"], profile.Env["DUNE_HOST_FAKE_ACP_METHODS"] = logPath, methodsPath
	profile.Env["SAVED_VALUE"] = "original"
	profile.Setup.Steps = []api.Command{{Run: `printf 'setup\n' >> "$LAUNCH_LOG"`, Shell: "/bin/sh"}}
	saved, err := f.app.store.Profiles().Create(t.Context(), profiles.Record{OwnerID: f.owner, Name: "Recovery source", Profile: profile, CreatedBy: profiles.Actor{Type: "user", Subject: f.principal.ID}})
	if err != nil {
		t.Fatal(err)
	}
	started, connection := startDirectoryACP(t, f, agents.StartRequest{Binding: f.binding, Profile: &profiles.Selection{ID: saved.ID, Revision: saved.Revision}})
	nativeCwd := filepath.Join(f.workspace, "native-cwd")
	if err := os.Mkdir(nativeCwd, 0700); err != nil {
		t.Fatal(err)
	}
	directoryAction(t, connection, *started.Runtime, api.ACPAction{Action: "new", Cwd: nativeCwd})
	page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Session.Status != "available" {
		t.Fatal(page, err)
	}
	session := *page.Items[0].Session
	request := agents.ResumeRequest{SessionID: session.ID, Revision: session.Revision}
	wrongScope := f.agentScope()
	wrongScope.OwnerID = "another-tenant"
	if result, err := f.app.AgentRestorer().Resume(t.Context(), wrongScope, request); err == nil || result.Session != nil {
		t.Fatal("cross-Tenant recovery exposed or started a session", result, err)
	}
	if result, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request); result.Runtime == nil || errorCode(err) != "RUNTIME_ALIVE" {
		t.Fatal("recovery started beside live Runtime", result, err)
	}
	stopRecoveryRuntime(t, connection, *started.Runtime)
	saved.Profile.Env["SAVED_VALUE"] = "changed"
	saved.Profile.Start.Argv = []string{"/missing/new-command", "--acp"}
	saved, err = f.app.store.Profiles().Update(t.Context(), saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.Profiles().Delete(t.Context(), f.owner, profiles.Selection{ID: saved.ID, Revision: saved.Revision}); err != nil {
		t.Fatal(err)
	}
	f.app.agentEnvironment = func(context.Context, agents.Scope, runner.Binding, map[string]string) (map[string]string, error) {
		return nil, errors.New("resume must not resolve new environment defaults")
	}
	results := make([]agents.ResumeResult, 2)
	errorsSeen := make([]error, 2)
	var group sync.WaitGroup
	begin := make(chan struct{})
	for i := range results {
		group.Go(func() {
			<-begin
			results[i], errorsSeen[i] = f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request)
		})
	}
	close(begin)
	group.Wait()
	for i, result := range results {
		if errorsSeen[i] != nil || result.Session == nil || result.Session.Attempt.Kind != "resume" {
			t.Fatal("concurrent recovery", i, result, errorsSeen[i])
		}
	}
	if results[0].Session.Attempt.ID != results[1].Session.Attempt.ID {
		t.Fatal("two recovery attempts claimed")
	}
	continued, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request)
	if err != nil || continued.Session.Status != "available" || continued.Session.Attempt.State != "ready" || continued.Runtime == nil || continued.Runtime.ID == started.Runtime.ID || continued.Session.LastRuntime.ID != continued.Runtime.ID {
		t.Fatal("repeat did not return confirmed attempt", continued, err)
	}
	if continued.Session.Native.ID != session.Native.ID || continued.Session.Native.Cwd != nativeCwd || continued.Runtime.WorkingDirectory != f.workspace || continued.Session.SourceProfile.Revision != 1 {
		t.Fatal("resume used a different session/configuration", continued)
	}
	waitLaunchFile(t, logPath, "setup\nlaunch:original\nlaunch:original\n")
	methods, err := os.ReadFile(methodsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(methods), "session/new:") != 2 || strings.Count(string(methods), "session/load:"+session.Native.ID+":"+nativeCwd) != 1 || strings.Contains(string(methods), "session/list:") {
		t.Fatal("recovery used new/list or loaded multiple times", string(methods))
	}
	page, err = f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 2 {
		t.Fatal("recovery directory", page, err)
	}
	for _, agent := range page.Items {
		if agent.RecoveryError != "" {
			t.Fatal("superseded Runtime treated as database failure", agent.RecoveryError)
		}
		if agent.Runtime.ID == continued.Runtime.ID && (agent.Session == nil || agent.Session.ID != session.ID) {
			t.Fatal("recovery association lost")
		}
	}
	encoded, _ := json.Marshal(continued)
	if strings.Contains(string(encoded), "SAVED_VALUE") || strings.Contains(string(encoded), "LAUNCH_LOG") {
		t.Fatal("private resume snapshot exposed")
	}
	directoryAction(t, connection, *continued.Runtime, api.ACPAction{Action: "load", SessionID: "other-after-resume", Cwd: nativeCwd})
	changed, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request)
	if errorCode(err) != "STALE_SESSION" || changed.Session.Selected || changed.Session.Native.ID != session.Native.ID {
		t.Fatal("repeated continue silently followed a different native conversation", changed, err)
	}
}

func errorCode(err error) string {
	var failure *api.Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func TestAgentRestorerKeepsFailedAndUnknownLoadsDistinct(t *testing.T) {
	for _, outcome := range []string{"failed", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			f := openExecutorFixture(t)
			started, connection := directoryACP(t, f)
			page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
			if err != nil {
				t.Fatal(err)
			}
			session := *page.Items[0].Session
			stopRecoveryRuntime(t, connection, *started.Runtime)
			stored, err := f.app.store.AgentSession(t.Context(), f.owner, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			flag := "DUNE_HOST_FAKE_ACP_FAIL_LOAD"
			if outcome == "unknown" {
				flag = "DUNE_HOST_FAKE_ACP_DROP_LOAD"
			}
			program, err := os.ReadFile(stored.Launch.Profile.Start.Argv[0])
			if err != nil {
				t.Fatal(err)
			}
			program = []byte(strings.Replace(string(program), "exec ", "export "+flag+"=1\nexec ", 1))
			if err := os.WriteFile(stored.Launch.Profile.Start.Argv[0], program, 0700); err != nil {
				t.Fatal(err)
			}
			request := agents.ResumeRequest{SessionID: session.ID, Revision: session.Revision}
			result, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request)
			code := "RECOVERY_FAILED"
			if outcome == "unknown" {
				code = "RESULT_UNKNOWN"
			}
			if errorCode(err) != code || result.Runtime == nil || result.Operation == nil || result.Session.Attempt.State != outcome || result.Session.LastRuntime.ID != started.Runtime.ID {
				t.Fatal("incorrect load outcome", result, err)
			}
			if repeated, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request); err != nil || repeated.Session.Attempt.ID != result.Session.Attempt.ID {
				t.Fatal("repeat restarted failed attempt", repeated, err)
			}
			if outcome == "unknown" {
				request.Revision = result.Session.Revision
				if _, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request); errorCode(err) != "RECOVERY_UNAVAILABLE" {
					t.Fatal("unknown outcome allowed takeover", err)
				}
			}
			runtimes, err := connection.List(t.Context())
			if err != nil || len(runtimes) != 2 {
				t.Fatal("load failure created another Runtime", len(runtimes), err)
			}
			if outcome == "failed" {
				stopRecoveryRuntime(t, connection, *result.Runtime)
				program = []byte(strings.Replace(string(program), "export "+flag+"=1\n", "", 1))
				if err := os.WriteFile(stored.Launch.Profile.Start.Argv[0], program, 0700); err != nil {
					t.Fatal(err)
				}
				request.Revision = result.Session.Revision
				retried, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request)
				if err != nil || retried.Session.Attempt.State != "ready" || retried.Session.Attempt.ID == result.Session.Attempt.ID {
					t.Fatal("explicit retry after known failure did not recover", retried, err)
				}
			}
		})
	}
}

func TestAgentRestorerRejectsUnavailableLoadCapabilitiesAndChangedVersion(t *testing.T) {
	for _, flag := range []string{"DUNE_HOST_FAKE_ACP_NO_LOAD=1", "DUNE_HOST_FAKE_ACP_VERSION=v2"} {
		t.Run(flag, func(t *testing.T) {
			f := openExecutorFixture(t)
			methods := filepath.Join(f.workspace, "methods")
			started, connection := directoryACPWithEnvironment(t, f, map[string]string{"DUNE_HOST_FAKE_ACP_VERSION": "v1", "DUNE_HOST_FAKE_ACP_METHODS": methods})
			page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
			if err != nil {
				t.Fatal(err)
			}
			session := page.Items[0].Session
			stopRecoveryRuntime(t, connection, *started.Runtime)
			program := filepath.Join(f.workspace, "fixture-agent")
			content, err := os.ReadFile(program)
			if err != nil {
				t.Fatal(err)
			}
			content = []byte(strings.Replace(string(content), "exec ", "export "+flag+"\nexec ", 1))
			if err := os.WriteFile(program, content, 0700); err != nil {
				t.Fatal(err)
			}
			result, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), agents.ResumeRequest{SessionID: session.ID, Revision: session.Revision})
			if errorCode(err) != "RECOVERY_FAILED" || result.Session.Attempt.State != "failed" || result.Operation != nil {
				t.Fatal("unavailable Agent accepted native load", result, err)
			}
			calls, err := os.ReadFile(methods)
			if err != nil || strings.Contains(string(calls), "session/load:") || strings.Count(string(calls), "session/new:") != 1 {
				t.Fatal("recovery silently tried new/load", string(calls), err)
			}
		})
	}
}

func TestAgentRestorerValidatesOriginalStorageAndBindingBeforeClaim(t *testing.T) {
	f := openExecutorFixture(t)
	profile := directoryACPProfile(t, f)
	profile.ManagedACP = true
	for _, changeBinding := range []bool{false, true} {
		launch := agents.LaunchSnapshot{Profile: profile, AgentType: "fixture", Recovery: agents.RecoveryAdapter{ID: "acp-load", Version: 1}, Storage: "uid:another-user:home:/another/home", Binding: f.binding}
		if changeBinding {
			launch.Binding.Revision++
		}
		session, err := f.app.store.CreateAgentSession(t.Context(), f.owner, launch)
		if err != nil {
			t.Fatal(err)
		}
		runtime := workbench.RuntimeRef{ID: wire.ID(), Incarnation: wire.ID(), Generation: 1, Adapter: "acp"}
		session, err = f.app.store.RecordAgentRuntime(t.Context(), f.owner, session.ID, session.Attempt.ID, runtime)
		if err != nil {
			t.Fatal(err)
		}
		session, err = f.app.store.CaptureAgentSession(t.Context(), f.owner, session.ID, session.Attempt.ID, runtime, agents.NativeSession{ID: "old-native", Cwd: f.workspace, Source: "acp-response", ResumeSupported: true})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), agents.ResumeRequest{SessionID: session.ID, Revision: session.Revision})
		if changeBinding {
			if !errors.Is(err, runner.ErrBindingChanged) {
				t.Fatal("replacement Runner accepted", err)
			}
		} else if errorCode(err) != "RECOVERY_STORAGE_CHANGED" {
			t.Fatal("different execution user/storage accepted", err)
		}
		unchanged, err := f.app.store.AgentSession(t.Context(), f.owner, session.ID)
		if err != nil || unchanged.Attempt.ID != session.Attempt.ID || unchanged.Revision != session.Revision {
			t.Fatal("invalid recovery claimed an attempt", err)
		}
	}
	page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 0 {
		t.Fatal("invalid recovery started a Runtime", page, err)
	}
}
