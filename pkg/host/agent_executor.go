package host

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

// AgentScope is supplied by trusted host code, not by an IM sender. Principal
// is the authorized embedding-host actor for this Owner. The caller persists
// the exact binding before sending; Dune rejects any replacement binding.
type AgentScope struct {
	Principal identity.User
	OwnerID   string
	Binding   runner.Binding
}

// AgentAction is intentionally narrower than fabricd's complete ACP action
// set. Bot execution cannot approve permissions or cancel an unrelated turn.
type AgentAction struct {
	SubmissionID           string `json:"submission_id"`
	ExpectedConversationID string `json:"expected_conversation_id,omitempty"`
	Action                 string `json:"action"`
	Text                   string `json:"text,omitempty"`
	SessionID              string `json:"session_id,omitempty"`
	Cwd                    string `json:"cwd,omitempty"`
}

type AgentState struct {
	Conversation *api.ACPConversation `json:"conversation"`
	Revision     uint64               `json:"revision"`
	Ready        bool                 `json:"ready"`
	Busy         string               `json:"busy"`
	SessionID    string               `json:"session_id"`
	Cwd          string               `json:"cwd"`
	CanLoad      bool                 `json:"can_load"`
	Error        string               `json:"error"`
	StopReason   string               `json:"stop_reason"`
}

// AgentConnection is an authorized in-process SDK connection. It still uses
// the Gateway, Dune access policy and fabricd's Runtime identity checks.
// Close must be called to release host draining and network resources.
type AgentConnection interface {
	Start(context.Context, string, api.Profile) (api.Runtime, error)
	Get(context.Context, api.Runtime) (api.Runtime, error)
	Observe(context.Context, api.Runtime) (AgentSubscription, error)
	State(context.Context, api.Runtime) (AgentState, error)
	Submit(context.Context, api.Runtime, AgentAction) (api.AgentOperation, error)
	WaitOperation(context.Context, api.Runtime, api.AgentOperationWait) (api.AgentOperation, error)
	ReadOperation(context.Context, api.Runtime, api.AgentOperationRead) (api.AgentOperationOutput, error)
	Stop(context.Context, api.Runtime, string) (api.SubmissionReceipt, error)
	QuerySubmission(context.Context, api.Runtime, string) (api.SubmissionReceipt, error)
	Close() error
}

type AgentSubscription interface {
	Recv() (*pb.Message, error)
	Close() error
}

type AgentExecutor interface {
	Open(context.Context, AgentScope) (AgentConnection, error)
}

type agentExecutor struct{ app *App }

func (a *App) AgentExecutor() AgentExecutor { return &agentExecutor{app: a} }

func (e *agentExecutor) Open(ctx context.Context, scope AgentScope) (AgentConnection, error) {
	ctx, finish, err := e.app.adminContext(ctx)
	if err != nil {
		return nil, err
	}
	if scope.OwnerID == "" || scope.Binding.RunnerID == "" || scope.Binding.FabricID == "" || scope.Binding.MachineID == "" || scope.Binding.Revision < 1 {
		finish()
		return nil, errors.New("IM Agent scope requires an Owner and exact Runner binding")
	}
	resource, err := e.app.store.RunnerResource(ctx, scope.Binding.RunnerID)
	if err != nil {
		finish()
		return nil, err
	}
	if resource.OwnerID != scope.OwnerID || resource.Runner.Binding == nil {
		finish()
		return nil, authorization.ErrNotFound
	}
	if *resource.Runner.Binding != scope.Binding {
		finish()
		return nil, runner.ErrBindingChanged
	}
	sdk, closeClient, err := (&runnerExecutor{app: e.app}).connect(ctx, scope.Principal, scope.OwnerID, *resource.Runner.Binding, "submission.acp")
	if err != nil {
		finish()
		return nil, err
	}
	for _, required := range []string{"profile.start", "runtime.get", "runtime.attach", "runtime.stop", "acp.state", "agent.operation.wait", "agent.operation.read"} {
		if !slices.Contains(sdk.Binding.Capabilities, required) {
			closeClient()
			finish()
			return nil, &api.Error{Code: "UNSUPPORTED", Detail: required + " is not supported by the current fabricd binding"}
		}
	}
	return &agentConnection{target: api.SubmissionTarget{OwnerID: scope.OwnerID, RunnerID: resource.Runner.Binding.RunnerID, FabricID: resource.Runner.Binding.FabricID, MachineID: resource.Runner.Binding.MachineID, BindingRevision: resource.Runner.Binding.Revision}, sdk: sdk, closeClient: closeClient, finish: finish}, nil
}

type agentConnection struct {
	target      api.SubmissionTarget
	sdk         *client.Client
	closeClient func()
	finish      func()
	once        sync.Once
}

func (c *agentConnection) Close() error {
	c.once.Do(func() {
		c.closeClient()
		c.finish()
	})
	return nil
}

func (c *agentConnection) Start(ctx context.Context, submissionID string, profile api.Profile) (out api.Runtime, err error) {
	key := api.SubmissionKey{SubmissionID: submissionID, Target: c.target}
	defer func() {
		if err != nil {
			err = &api.SubmissionError{Key: key, Cause: err}
		}
	}()
	if profile.Kind != "agent" || profile.Adapter != "acp" || !profile.ManagedACP {
		return api.Runtime{}, errors.New("IM Agent requires a managed ACP Profile")
	}
	if err := profile.Validate(); err != nil {
		return api.Runtime{}, err
	}
	result, stream, err := c.sdk.Start(ctx, api.StartRequest{SubmissionKey: key, Profile: profile})
	if stream != nil {
		_ = stream.Close()
	}
	if result.Stage == "started" && result.Runtime != nil {
		return *result.Runtime, err
	}
	return api.Runtime{}, err
}

func (c *agentConnection) Get(ctx context.Context, runtime api.Runtime) (api.Runtime, error) {
	return c.sdk.Get(ctx, runtime)
}

func (c *agentConnection) Observe(ctx context.Context, runtime api.Runtime) (AgentSubscription, error) {
	if runtime.Adapter != "acp" {
		return nil, errors.New("IM observation requires an ACP Runtime")
	}
	return c.sdk.Attach(ctx, runtime, true)
}

func (c *agentConnection) State(ctx context.Context, runtime api.Runtime) (AgentState, error) {
	var state AgentState
	err := c.sdk.CallID(ctx, "acp.state", wire.ID(), struct{}{}, &state, &runtime)
	return state, err
}

func (c *agentConnection) Submit(ctx context.Context, runtime api.Runtime, action AgentAction) (api.AgentOperation, error) {
	target := c.target
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	key := api.SubmissionKey{SubmissionID: action.SubmissionID, Target: target}
	if action.Action != "new" && action.Action != "load" && action.Action != "prompt" {
		return api.AgentOperation{Submission: &api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}}, &api.SubmissionError{Key: key, Cause: errors.New("IM Agent action must be new, load or prompt")}
	}
	return c.sdk.ACPSubmit(ctx, key, api.ACPAction{ExpectedConversationID: action.ExpectedConversationID, Action: action.Action, Text: action.Text, SessionID: action.SessionID, Cwd: action.Cwd})
}

func (c *agentConnection) WaitOperation(ctx context.Context, runtime api.Runtime, request api.AgentOperationWait) (api.AgentOperation, error) {
	return c.sdk.WaitAgentOperation(ctx, runtime, request)
}

func (c *agentConnection) ReadOperation(ctx context.Context, runtime api.Runtime, request api.AgentOperationRead) (api.AgentOperationOutput, error) {
	return c.sdk.ReadAgentOperation(ctx, runtime, request)
}

func (c *agentConnection) Stop(ctx context.Context, runtime api.Runtime, submissionID string) (api.SubmissionReceipt, error) {
	target := c.target
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	return c.sdk.Stop(ctx, api.SubmissionKey{SubmissionID: submissionID, Target: target})
}

func (c *agentConnection) QuerySubmission(ctx context.Context, runtime api.Runtime, submissionID string) (api.SubmissionReceipt, error) {
	target := c.target
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	return c.sdk.QuerySubmission(ctx, api.SubmissionKey{SubmissionID: submissionID, Target: target})
}
