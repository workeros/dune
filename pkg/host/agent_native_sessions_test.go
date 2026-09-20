package host

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/runner"
)

func TestAgentNativeSessionStartupIsImmediatelyUsable(t *testing.T) {
	f := openExecutorFixture(t)
	methods := filepath.Join(f.workspace, "methods")
	started, _ := directoryACPWithEnvironment(t, f, map[string]string{"DUNE_HOST_FAKE_ACP_METHODS": methods})
	if started.Operation == nil || started.Operation.State != "completed" || started.Runtime.NativeSession == nil {
		t.Fatal("start did not confirm the initial native session", started)
	}
	page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	op, err := f.app.AgentMessenger().Prompt(t.Context(), f.agentScope(), agents.PromptRequest{SubmissionID: wire.ID(), ExpectedConversationID: page.Items[0].Runtime.ConversationID, AgentRef: page.Items[0].Ref, Text: "first task", WaitMS: 3000})
	if err != nil || op.State != "completed" {
		t.Fatal("start still required manual new", op, err)
	}
	calls, err := os.ReadFile(methods)
	if err != nil || strings.Count(string(calls), "session/new:") != 1 || strings.Count(string(calls), "session/prompt:") != 1 {
		t.Fatal("startup or prompt repeated", string(calls), err)
	}
}

func TestAgentNativeSessionQueueRetainsOperationAndCapturesOnlyConfirmation(t *testing.T) {
	f := openExecutorFixture(t)
	gate := filepath.Join(f.workspace, "release-first")
	started, connection := directoryACPWithEnvironment(t, f, map[string]string{"DUNE_HOST_FAKE_ACP_GATE": gate})
	page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	agent := page.Items[0]
	messenger := f.app.AgentMessenger()
	if _, err := messenger.Prompt(t.Context(), f.agentScope(), agents.PromptRequest{SubmissionID: wire.ID(), ExpectedConversationID: agent.Runtime.ConversationID, AgentRef: agent.Ref, Text: "first", WaitMS: 1}); err != nil {
		t.Fatal(err)
	}
	sessions := f.app.AgentNativeSessions()
	load, err := sessions.OpenSession(t.Context(), f.agentScope(), agents.OpenSessionRequest{SubmissionID: wire.ID(), AgentRef: agent.Ref, Action: "load", SessionID: "saved-conversation", Cwd: "/native/work", WaitMS: 1})
	if err != nil || load.State != "pending" || load.NativeSession != nil || !strings.HasPrefix(load.Ref, "operation_") {
		t.Fatal("load failed to retain queued operation", load, err)
	}
	current, err := connection.Get(t.Context(), *started.Runtime)
	if err != nil || current.NativeSession.ID != started.Runtime.NativeSession.ID {
		t.Fatal("queued load changed current session", err)
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	completed, err := messenger.Wait(t.Context(), f.agentScope(), agents.WaitRequest{OperationRef: load.Ref, TimeoutMS: 3000})
	if err != nil || completed.Operation.State != "completed" || completed.Operation.NativeSession.ID != "saved-conversation" {
		t.Fatal("load confirmation was not captured", completed, err)
	}
	current, err = connection.Get(t.Context(), *started.Runtime)
	if err != nil || current.NativeSession.ID != "saved-conversation" || current.NativeSession.Cwd != "/native/work" {
		t.Fatal("confirmed load lost native selection", err)
	}
	if _, err := sessions.OpenSession(t.Context(), f.agentScope(), agents.OpenSessionRequest{SubmissionID: wire.ID(), AgentRef: agent.Ref, Action: "new"}); errorCode(err) != "STALE_SESSION" {
		t.Fatal("stale native reference changed a different conversation", err)
	}
	wrong := f.agentScope()
	wrong.OwnerID = "another-tenant"
	if _, err := sessions.OpenSession(t.Context(), wrong, agents.OpenSessionRequest{SubmissionID: wire.ID(), AgentRef: agent.Ref, Action: "new"}); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("cross Tenant lifecycle request accepted", err)
	}
}

func TestAgentNativeSessionStartupFailurePreservesRuntimeAndOperation(t *testing.T) {
	for _, outcome := range []string{"failed", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			f := openExecutorFixture(t)
			profile := directoryACPProfile(t, f)
			methods := filepath.Join(f.workspace, "methods")
			profile.Env["DUNE_HOST_FAKE_ACP_METHODS"] = methods
			flag, code := "DUNE_HOST_FAKE_ACP_FAIL_NEW", "SESSION_FAILED"
			if outcome == "unknown" {
				flag, code = "DUNE_HOST_FAKE_ACP_DROP_NEW", "RESULT_UNKNOWN"
			}
			profile.Env[flag] = "1"
			result, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{SubmissionID: wire.ID(), Binding: f.binding, Custom: &profile})
			if errorCode(err) != code || result.Runtime == nil || result.Operation == nil || result.Operation.State != outcome || result.Runtime.NativeSession != nil {
				t.Fatal("startup lost a partial result", result, err)
			}
			waited, err := f.app.AgentMessenger().Wait(t.Context(), f.agentScope(), agents.WaitRequest{OperationRef: result.Operation.Ref})
			if err != nil || waited.Operation.State != outcome {
				t.Fatal("operation could not be checked after startup failed", waited, err)
			}
			calls, err := os.ReadFile(methods)
			if err != nil || strings.Count(string(calls), "initialize:") != 1 || strings.Count(string(calls), "session/new:") != 1 {
				t.Fatal("startup replayed after failure", string(calls), err)
			}
		})
	}
}
