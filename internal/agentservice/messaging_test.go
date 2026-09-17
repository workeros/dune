package agentservice

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
)

func TestOperationReferencesAreCanonicalAndKeepRuntimeIdentity(t *testing.T) {
	target := targetFor(runner.Binding{RunnerID: "runner_test", FabricID: "f", MachineID: "m", Revision: 3}, api.Runtime{ID: "r", Incarnation: "i", Generation: 4, Adapter: "acp"})
	id := wire.ID()
	value := operationRef(target, id)
	parsed, err := parseOperationRef(value)
	if err != nil || parsed.Target != target || parsed.ID != id {
		t.Fatal("operation reference lost its execution target", parsed, err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "operation_"))
	for _, invalid := range []string{"", value + "=", "operation_" + base64.RawURLEncoding.EncodeToString(append(raw, []byte("{}")...)), operationRef(target, "not-an-operation-id"), agentRef(target, nil), strings.Repeat("x", 9000)} {
		if _, err := parseOperationRef(invalid); err == nil {
			t.Fatal("accepted malformed operation reference")
		}
	}
}

func TestMessageModesRejectAmbiguousRequestsBeforeDial(t *testing.T) {
	s := &Service{}
	for _, request := range []agents.WaitRequest{{}, {AgentRef: "a", OperationRef: "b"}, {OperationRef: "b", Until: "idle"}, {AgentRef: "a", Until: "working"}, {AgentRef: "a", TimeoutMS: 30001}, {AgentRef: "a", TimeoutMS: -1}} {
		if _, err := s.Wait(t.Context(), agents.Scope{}, request); err == nil {
			t.Fatal("accepted ambiguous wait", request)
		}
	}
	for _, request := range []agents.ReadRequest{{}, {AgentRef: "a", OperationRef: "b"}, {AgentRef: "a", Position: 1}} {
		if _, err := s.Read(t.Context(), agents.Scope{}, request); err == nil {
			t.Fatal("accepted ambiguous read", request)
		}
	}
	for _, request := range []agents.PromptRequest{{Text: "", AgentRef: "a"}, {Text: "valid", AgentRef: "a", WaitMS: 30001}, {Text: strings.Repeat("x", 65537), AgentRef: "a"}} {
		if _, err := s.Prompt(t.Context(), agents.Scope{}, request); err == nil {
			t.Fatal("accepted invalid prompt")
		}
	}
	var failure *api.Error
	if !errors.As(submissionError(io.EOF), &failure) || failure.Code != "RESULT_UNKNOWN" {
		t.Fatal("transport EOF treated as known rejection")
	}
	if !errors.As(submissionError(&api.Error{Code: "STREAM_INTERRUPTED"}), &failure) || failure.Code != "RESULT_UNKNOWN" {
		t.Fatal("lost admission acknowledgement treated as known rejection")
	}
	denied := &api.Error{Code: "ACCESS_DENIED"}
	if submissionError(denied) != denied {
		t.Fatal("known rejection lost")
	}
}

func TestActivityWaitDoesNotInferPTYCompletion(t *testing.T) {
	runtime := api.Runtime{State: "running", Activity: &api.AgentActivity{State: "unknown", Agent: "codex", Foreground: "codex"}}
	for _, until := range []string{"", "attention", "idle", "blocked", "exited"} {
		if activityMatches(runtime, until) {
			t.Fatal("unknown native state counted as completion", until)
		}
	}
	runtime.Activity.State = "blocked"
	if !activityMatches(runtime, "attention") || activityMatches(runtime, "idle") {
		t.Fatal("blocked confused with idle")
	}
	runtime.State = "exited"
	if !activityMatches(runtime, "exited") || activityMatches(runtime, "blocked") {
		t.Fatal("exit did not override stale activity")
	}
}
