package agentservice

import (
	"context"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

func validateOpenSession(request agents.OpenSessionRequest) error {
	if err := validWait(request.WaitMS); err != nil {
		return err
	}
	if request.Action != "new" && request.Action != "load" {
		return invalid("native session action must be new or load")
	}
	if (request.Action == "new" && request.SessionID != "") || (request.Action == "load" && request.SessionID == "") {
		return invalid("session_id is required only for load")
	}
	if !utf8.ValidString(request.SessionID) || len(request.SessionID) > 4096 || strings.ContainsFunc(request.SessionID, unicode.IsControl) {
		return invalid("invalid native session_id")
	}
	if request.Cwd != "" && (!path.IsAbs(request.Cwd) || len(request.Cwd) > 4096 || !utf8.ValidString(request.Cwd) || strings.ContainsFunc(request.Cwd, unicode.IsControl)) {
		return invalid("cwd must be an absolute directory")
	}
	return nil
}

func (s *Service) OpenSession(ctx context.Context, scope agents.Scope, request agents.OpenSessionRequest) (result agents.Operation, err error) {
	key := api.SubmissionKey{SubmissionID: request.SubmissionID, Target: api.SubmissionTarget{OwnerID: scope.OwnerID}}
	defer func() {
		if result.Submission == nil {
			result.Submission = &api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
		}
		if err != nil {
			err = &api.SubmissionError{Key: key, Cause: err}
		}
	}()
	ref, err := parseAgentRef(request.AgentRef)
	if err != nil {
		return result, err
	}
	key = submissionKeyFor(scope.OwnerID, ref.Target, request.SubmissionID)
	if err := key.Validate(); err != nil {
		return result, invalid(err.Error())
	}

	if err := validateOpenSession(request); err != nil {
		return agents.Operation{}, err
	}
	if ref.Target.Runtime.Adapter != "acp" {
		return agents.Operation{}, &api.Error{Code: "UNSUPPORTED", Detail: "native session new/load requires managed ACP"}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(request.WaitMS)*time.Millisecond+25*time.Second)
	defer cancel()
	_, connection, closeConnection, err := s.connect(ctx, scope, ref.Target, "acp.action")
	if err != nil {
		return agents.Operation{}, err
	}
	defer closeConnection()
	runtime, err := currentRuntime(ctx, connection, ref)
	if err != nil {
		return agents.Operation{}, err
	}
	if _, err := awaitACPReady(ctx, connection, runtime); err != nil {
		return agents.Operation{}, err
	}
	action := api.ACPAction{Action: request.Action, SessionID: request.SessionID, Cwd: request.Cwd}
	return submitACP(ctx, connection, ref.Target, runtime, key, action, request.WaitMS)
}

// Queue ownership and completion stay in the original session host. A failed optional wait still
// returns the accepted reference, and a caller disconnect never resubmits it.
func submitACP(ctx context.Context, connection *client.Client, target workbench.AgentTarget, runtime api.Runtime, key api.SubmissionKey, action api.ACPAction, waitMS int) (agents.Operation, error) {
	accepted, err := connection.ACPSubmit(ctx, key, action)
	if err != nil {
		return describeOperation(target, accepted), submissionError(err)
	}
	result := describeOperation(target, accepted)
	if waitMS > 0 && !accepted.Terminal() {
		completed, err := connection.WaitAgentOperation(ctx, runtime, api.AgentOperationWait{Ref: accepted.Ref, TimeoutMS: waitMS})
		if err != nil {
			return result, err
		}
		completed.Submission = accepted.Submission
		result = describeOperation(target, completed)
	}
	return result, nil
}

func (s *Service) initializeACP(ctx context.Context, scope agents.Scope, connection *client.Client, binding runner.Binding, result agents.LaunchResult) (agents.LaunchResult, error) {
	runtime := *result.Runtime
	if _, err := awaitACPReady(ctx, connection, runtime); err != nil {
		return result, &api.Error{Code: "AGENT_NOT_READY", Detail: "Runtime started but ACP initialization was not confirmed; inspect it before another start"}
	}
	if err := s.configureMCP(ctx, scope, connection, runtime, binding); err != nil {
		return result, err
	}
	target := targetFor(binding, runtime)
	operation, err := submitACP(ctx, connection, target, runtime, submissionKeyFor(scope.OwnerID, target, result.SubmissionID), api.ACPAction{Action: "new", Cwd: runtime.WorkingDirectory}, 30000)
	if operation.Ref != "" {
		result.Operation = &operation
	}
	if operation.NativeSession != nil {
		runtime.NativeSession = operation.NativeSession
		runtime.ConversationID = operation.ConversationID
		result.Runtime = &runtime
	}
	if err != nil {
		return result, err
	}
	switch operation.State {
	case "unknown":
		return result, &api.Error{Code: "RESULT_UNKNOWN", Detail: "native session creation outcome is unknown; the operation was not replayed"}
	case "failed", "cancelled":
		return result, &api.Error{Code: "SESSION_FAILED", Detail: "Runtime started but the Agent did not create a native session; inspect the accepted operation"}
	default:
		return result, nil
	}
}

type readyACP struct {
	Ready bool   `json:"ready"`
	Error string `json:"error"`
}

func awaitACPReady(ctx context.Context, connection *client.Client, runtime api.Runtime) (readyACP, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var state readyACP
		if err := connection.CallID(ctx, "acp.state", wire.ID(), struct{}{}, &state, &runtime); err != nil {
			return state, err
		}
		if state.Error != "" {
			return state, &api.Error{Code: "AGENT_NOT_READY", Detail: "Agent initialization failed"}
		}
		if state.Ready {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case <-ticker.C:
		}
	}
}

func submissionKeyFor(owner string, target workbench.AgentTarget, id string) api.SubmissionKey {
	return api.SubmissionKey{SubmissionID: id, Target: api.SubmissionTarget{
		OwnerID: owner, RunnerID: target.Binding.RunnerID, FabricID: target.Binding.FabricID,
		MachineID: target.Binding.MachineID, BindingRevision: target.Binding.Revision,
		RuntimeID: target.Runtime.ID, RuntimeIncarnation: target.Runtime.Incarnation, RuntimeGeneration: target.Runtime.Generation,
	}}
}
