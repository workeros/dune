package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
)

func TestAgentSubmissionLookupUsesOriginalKeyAcrossNativeSessionChange(t *testing.T) {
	f := openExecutorFixture(t)
	log := filepath.Join(f.workspace, "methods")
	launched, connection := directoryACPWithEnvironment(t, f, map[string]string{"DUNE_HOST_FAKE_ACP_METHODS": log})
	service := f.app.AgentMessenger()
	request := agents.SubmissionRequest{SubmissionID: "saved-open", AgentRef: launched.AgentRef, ACPAction: api.ACPAction{Action: "new", Cwd: f.workspace}}
	query := agents.SubmissionQuery{SubmissionID: request.SubmissionID, AgentRef: request.AgentRef}
	before, err := service.QuerySubmission(t.Context(), f.agentScope(), query)
	if err != nil || before.Admission != api.SubmissionUnknown {
		t.Fatal(before, err)
	}
	opened, err := service.Submit(t.Context(), f.agentScope(), request)
	if err != nil || opened.Admission != api.SubmissionAccepted {
		t.Fatal(opened, err)
	}
	operation, err := connection.WaitAgentOperation(t.Context(), *launched.Runtime, api.AgentOperationWait{Ref: opened.OperationRef, TimeoutMS: 3000})
	if err != nil || operation.State != "completed" {
		t.Fatal(operation, err)
	}
	duplicate, err := service.Submit(t.Context(), f.agentScope(), request)
	if err != nil || duplicate.OperationRef != opened.OperationRef {
		t.Fatal(duplicate, err)
	}
	request.SessionID, request.Action, request.SubmissionID = "different-native", "load", "second-open"
	changed, err := service.Submit(t.Context(), f.agentScope(), request)
	if err != nil {
		t.Fatal(err)
	}
	operation, err = connection.WaitAgentOperation(t.Context(), *launched.Runtime, api.AgentOperationWait{Ref: changed.OperationRef, TimeoutMS: 3000})
	if err != nil || operation.State != "completed" {
		t.Fatal(operation, err)
	}
	observed, err := f.app.AgentMessenger().QuerySubmission(t.Context(), f.agentScope(), query)
	if err != nil || observed.OperationRef != opened.OperationRef || observed.SubmissionKey != opened.SubmissionKey {
		t.Fatal("native replacement changed original admission", observed, err)
	}
	data, err := os.ReadFile(log)
	if err != nil || strings.Count(string(data), "session/new:") != 2 || strings.Count(string(data), "session/load:") != 1 {
		t.Fatal("duplicate execution", string(data), err)
	}
	// Errors before service entry preserve exactly the caller's recovery selectors.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	failed, err := service.Submit(ctx, f.agentScope(), request)
	var failure *api.SubmissionError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.Key != failed.SubmissionKey || failed.SubmissionID != "second-open" || failed.Target != opened.Target {
		t.Fatal(failed, err)
	}
	wrong := f.agentScope()
	wrong.OwnerID = "other"
	if _, err := service.QuerySubmission(t.Context(), wrong, query); err == nil {
		t.Fatal("cross-owner query succeeded")
	}
	stop := agents.SubmissionRequest{SubmissionID: "saved-stop", AgentRef: launched.AgentRef, ACPAction: api.ACPAction{Action: "stop"}}
	stopped, err := service.Submit(t.Context(), f.agentScope(), stop)
	if err != nil || stopped.Admission != api.SubmissionAccepted || stopped.Stage != "stopped" {
		t.Fatal("original Runtime stop depended on its changed native session", stopped, err)
	}
	duplicate, err = service.Submit(t.Context(), f.agentScope(), stop)
	if err != nil || duplicate.OperationRef != stopped.OperationRef || duplicate.Stage != "stopped" {
		t.Fatal("duplicate Runtime stop lost original result", duplicate, err)
	}
	observed, err = service.QuerySubmission(t.Context(), f.agentScope(), agents.SubmissionQuery{SubmissionID: stop.SubmissionID, AgentRef: stop.AgentRef})
	if err != nil || observed.OperationRef != stopped.OperationRef || observed.Stage != "stopped" {
		t.Fatal("original stop could not be queried", observed, err)
	}
	forget := agents.SubmissionRequest{SubmissionID: "saved-forget", AgentRef: launched.AgentRef, ACPAction: api.ACPAction{Action: "forget"}}
	cleaned, err := service.Submit(t.Context(), f.agentScope(), forget)
	if err != nil || cleaned.Stage != "completed" {
		t.Fatal("original-reference cleanup failed", cleaned, err)
	}
	observed, err = service.QuerySubmission(t.Context(), f.agentScope(), agents.SubmissionQuery{SubmissionID: forget.SubmissionID, AgentRef: forget.AgentRef})
	if err != nil || observed.Stage != "completed" || observed.OperationRef != cleaned.OperationRef {
		t.Fatal("cleanup lost its original public query", observed, err)
	}

}
