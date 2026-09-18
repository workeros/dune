package agentservice

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/workbench"
)

func validWait(timeout int) error {
	if timeout < 0 || timeout > 30000 {
		return invalid("wait duration must be 0..30000 milliseconds")
	}
	return nil
}

func (s *Service) Prompt(ctx context.Context, scope agents.Scope, request agents.PromptRequest) (agents.Operation, error) {
	if err := validWait(request.WaitMS); err != nil {
		return agents.Operation{}, err
	}
	if request.Text == "" || len(request.Text) > 64*1024 || !utf8.ValidString(request.Text) {
		return agents.Operation{}, invalid("prompt requires 1..65536 UTF-8 bytes")
	}
	ref, err := parseAgentRef(request.AgentRef)
	if err != nil {
		return agents.Operation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(request.WaitMS)*time.Millisecond+10*time.Second)
	defer cancel()
	operation := "acp.action"
	if ref.Target.Runtime.Adapter == "pty" {
		operation = "pty.prompt"
	}
	_, connection, closeConnection, err := s.connect(ctx, scope, ref.Target, operation)
	if err != nil {
		return agents.Operation{}, err
	}
	defer closeConnection()
	runtime, err := currentRuntime(ctx, connection, ref)
	if err != nil {
		return agents.Operation{}, err
	}
	var accepted api.AgentOperation
	if runtime.Adapter == "acp" {
		if runtime.NativeSession == nil {
			return agents.Operation{}, &api.Error{Code: "SESSION_REQUIRED", Detail: "create or load a native ACP session before prompting"}
		}
		accepted, err = connection.ACPSubmit(ctx, runtime, api.ACPAction{Action: "prompt", Text: request.Text, SessionID: runtime.NativeSession.ID, Cwd: runtime.NativeSession.Cwd})
	} else {
		agent := ""
		if runtime.Activity != nil {
			agent = runtime.Activity.Agent
		}
		nativeID, nativeCwd := nativePTYTarget(runtime)
		accepted, err = connection.PTYPrompt(ctx, runtime, api.PTYPrompt{Text: request.Text, Agent: agent, SessionID: nativeID, Cwd: nativeCwd})
	}
	if err != nil {
		return agents.Operation{}, submissionError(err)
	}
	result := describeOperation(ref.Target, accepted)
	if request.WaitMS > 0 && !accepted.Terminal() {
		completed, waitErr := connection.WaitAgentOperation(ctx, runtime, api.AgentOperationWait{Ref: accepted.Ref, TimeoutMS: request.WaitMS})
		if waitErr != nil {
			// The accepted reference remains usable after a failed optional wait.
			return result, waitErr
		}
		result = describeOperation(ref.Target, completed)
	}
	return result, nil
}

func (s *Service) SendKeys(ctx context.Context, scope agents.Scope, request agents.KeysRequest) (agents.Operation, error) {
	ref, err := parseAgentRef(request.AgentRef)
	if err != nil {
		return agents.Operation{}, err
	}
	if ref.Target.Runtime.Adapter != "pty" {
		return agents.Operation{}, &api.Error{Code: "UNSUPPORTED", Detail: "send_keys requires a PTY Agent"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, connection, closeConnection, err := s.connect(ctx, scope, ref.Target, "pty.keys")
	if err != nil {
		return agents.Operation{}, err
	}
	defer closeConnection()
	runtime, err := currentRuntime(ctx, connection, ref)
	if err != nil {
		return agents.Operation{}, err
	}
	agent := ""
	if runtime.Activity != nil {
		agent = runtime.Activity.Agent
	}
	nativeID, nativeCwd := nativePTYTarget(runtime)
	accepted, err := connection.PTYSendKeys(ctx, runtime, api.PTYKeys{Keys: request.Keys, Agent: agent, SessionID: nativeID, Cwd: nativeCwd})
	if err != nil {
		return agents.Operation{}, submissionError(err)
	}
	return describeOperation(ref.Target, accepted), nil
}

func nativePTYTarget(runtime api.Runtime) (string, string) {
	if runtime.NativeSession == nil {
		return "", ""
	}
	return runtime.NativeSession.ID, runtime.NativeSession.Cwd
}

// Transport errors after submission do not prove rejection. Only a structured
// fabricd/Gateway response can be reported as a known failure; never retry here.
func submissionError(err error) error {
	var failure *api.Error
	if errors.As(err, &failure) && failure.Code != "STREAM_INTERRUPTED" {
		return err
	}
	return &api.Error{Code: "RESULT_UNKNOWN", Detail: "Agent submission result is unknown; the request was not replayed"}
}

func (s *Service) Wait(ctx context.Context, scope agents.Scope, request agents.WaitRequest) (agents.WaitResult, error) {
	if err := validWait(request.TimeoutMS); err != nil {
		return agents.WaitResult{}, err
	}
	if (request.OperationRef == "") == (request.AgentRef == "") {
		return agents.WaitResult{}, invalid("choose exactly one operation_ref or agent_ref")
	}
	if request.AgentRef != "" {
		return s.waitActivity(ctx, scope, request)
	}
	if request.Until != "" {
		return agents.WaitResult{}, invalid("until applies only to Agent activity")
	}
	ref, err := parseOperationRef(request.OperationRef)
	if err != nil {
		return agents.WaitResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(request.TimeoutMS)*time.Millisecond+10*time.Second)
	defer cancel()
	_, connection, closeConnection, err := s.connect(ctx, scope, ref.Target, "agent.operation.wait")
	if err != nil {
		return agents.WaitResult{}, err
	}
	defer closeConnection()
	operation, err := connection.WaitAgentOperation(ctx, runtimeFor(ref.Target), api.AgentOperationWait{Ref: ref.ID, TimeoutMS: request.TimeoutMS})
	if err != nil {
		return agents.WaitResult{}, err
	}
	result := describeOperation(ref.Target, operation)
	return agents.WaitResult{Operation: &result, TimedOut: !operation.Terminal()}, nil
}

func (s *Service) waitActivity(ctx context.Context, scope agents.Scope, request agents.WaitRequest) (agents.WaitResult, error) {
	switch request.Until {
	case "", "attention", "idle", "blocked", "exited":
	default:
		return agents.WaitResult{}, invalid("until must be attention, idle, blocked or exited")
	}
	ref, err := parseAgentRef(request.AgentRef)
	if err != nil {
		return agents.WaitResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(request.TimeoutMS)*time.Millisecond+10*time.Second)
	defer cancel()
	resource, connection, closeConnection, err := s.connect(ctx, scope, ref.Target, "runtime.get")
	if err != nil {
		return agents.WaitResult{}, err
	}
	defer closeConnection()
	timer := time.NewTimer(time.Duration(request.TimeoutMS) * time.Millisecond)
	defer timer.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		runtime, err := currentRuntime(ctx, connection, ref)
		if err != nil {
			return agents.WaitResult{}, err
		}
		if activityMatches(runtime, request.Until) {
			agent := describe(resource.Runner, runtime)
			return agents.WaitResult{Agent: &agent}, nil
		}
		select {
		case <-ctx.Done():
			return agents.WaitResult{}, ctx.Err()
		case <-timer.C:
			agent := describe(resource.Runner, runtime)
			return agents.WaitResult{Agent: &agent, TimedOut: true}, nil
		case <-ticker.C:
		}
	}
}

func activityMatches(runtime api.Runtime, until string) bool {
	state := "unknown"
	if runtime.State == "exited" {
		state = "exited"
	} else if runtime.Activity != nil {
		state = runtime.Activity.State
	}
	if until == "" || until == "attention" {
		return state == "idle" || state == "blocked" || state == "exited"
	}
	return state == until
}

func (s *Service) Read(ctx context.Context, scope agents.Scope, request agents.ReadRequest) (agents.ReadResult, error) {
	if (request.OperationRef == "") == (request.AgentRef == "") {
		return agents.ReadResult{}, invalid("choose exactly one operation_ref or agent_ref")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if request.AgentRef != "" {
		return s.readSnapshot(ctx, scope, request)
	}
	ref, err := parseOperationRef(request.OperationRef)
	if err != nil {
		return agents.ReadResult{}, err
	}
	if ref.Target.Runtime.Adapter != "acp" {
		return agents.ReadResult{}, &api.Error{Code: "UNSUPPORTED", Detail: "PTY output is a session snapshot; use agent_ref to read it"}
	}
	_, connection, closeConnection, err := s.connect(ctx, scope, ref.Target, "agent.operation.read")
	if err != nil {
		return agents.ReadResult{}, err
	}
	defer closeConnection()
	output, err := connection.ReadAgentOperation(ctx, runtimeFor(ref.Target), api.AgentOperationRead{Ref: ref.ID, Position: request.Position, Limit: request.Limit})
	if err != nil {
		return agents.ReadResult{}, err
	}
	operation := describeOperation(ref.Target, output.AgentOperation)
	output.AgentOperation = operation.AgentOperation
	return agents.ReadResult{Operation: &agents.OperationOutput{AgentOperationOutput: output}}, nil
}

func (s *Service) readSnapshot(ctx context.Context, scope agents.Scope, request agents.ReadRequest) (agents.ReadResult, error) {
	if request.Position != 0 || request.Limit != 0 {
		return agents.ReadResult{}, invalid("position and limit apply only to operation output")
	}
	ref, err := parseAgentRef(request.AgentRef)
	if err != nil {
		return agents.ReadResult{}, err
	}
	if ref.Target.Runtime.Adapter != "pty" {
		return agents.ReadResult{}, &api.Error{Code: "UNSUPPORTED", Detail: "read ACP output with its operation_ref"}
	}
	resource, connection, closeConnection, err := s.connect(ctx, scope, ref.Target, "runtime.capture")
	if err != nil {
		return agents.ReadResult{}, err
	}
	defer closeConnection()
	runtime, err := currentRuntime(ctx, connection, ref)
	if err != nil {
		return agents.ReadResult{}, err
	}
	terminal, err := connection.CaptureTerminal(ctx, runtime)
	if err != nil {
		return agents.ReadResult{}, err
	}
	return agents.ReadResult{Snapshot: &agents.Snapshot{Agent: describe(resource.Runner, runtime), Terminal: terminal}}, nil
}

func describeOperation(target workbench.AgentTarget, operation api.AgentOperation) agents.Operation {
	operation.Ref = operationRef(target, operation.Ref)
	return agents.Operation{AgentOperation: operation}
}
