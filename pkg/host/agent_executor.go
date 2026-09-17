package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/identity"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

// AgentScope is supplied by trusted host code, not by an IM sender. Principal
// is the authorized embedding-host actor for this Owner. The Runner's current
// binding is resolved inside Dune and checked again by BackgroundRunner.
type AgentScope struct {
	Principal identity.User
	OwnerID   string
	RunnerID  string
}

// AgentAction is intentionally narrower than fabricd's complete ACP action
// set. Bot execution cannot approve permissions or cancel an unrelated turn.
type AgentAction struct {
	Action    string `json:"action"`
	Text      string `json:"text,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

type AgentState struct {
	Revision   uint64 `json:"revision"`
	Ready      bool   `json:"ready"`
	Busy       string `json:"busy"`
	SessionID  string `json:"session_id"`
	Cwd        string `json:"cwd"`
	CanLoad    bool   `json:"can_load"`
	Error      string `json:"error"`
	StopReason string `json:"stop_reason"`
}

// AgentConnection is an authorized in-process SDK connection. It still uses
// the Gateway, Dune access policy and fabricd's Runtime identity checks.
// Close must be called to release host draining and network resources.
type AgentConnection interface {
	AgentConfig(context.Context, string) (api.AgentConfig, error)
	Start(context.Context, api.Profile) (api.Runtime, error)
	Get(context.Context, api.Runtime) (api.Runtime, error)
	Observe(context.Context, api.Runtime) (AgentSubscription, error)
	State(context.Context, api.Runtime) (AgentState, error)
	Action(context.Context, api.Runtime, AgentAction) error
	Stop(context.Context, api.Runtime) error
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
	if scope.OwnerID == "" || scope.RunnerID == "" {
		finish()
		return nil, errors.New("IM Agent scope requires Tenant and Runner IDs")
	}
	resource, err := e.app.store.RunnerResource(ctx, scope.RunnerID)
	if err != nil {
		finish()
		return nil, err
	}
	if resource.OwnerID != scope.OwnerID || resource.Runner.Binding == nil {
		finish()
		return nil, authorization.ErrNotFound
	}
	sdk, closeClient, err := (&runnerExecutor{app: e.app}).connect(ctx, scope.Principal, scope.OwnerID, *resource.Runner.Binding, "acp.action")
	if err != nil {
		finish()
		return nil, err
	}
	for _, required := range []string{"agent.config", "profile.start", "runtime.get", "runtime.attach", "runtime.stop", "acp.state"} {
		if !slices.Contains(sdk.Binding.Capabilities, required) {
			closeClient()
			finish()
			return nil, &api.Error{Code: "UNSUPPORTED", Detail: required + " is not supported by the current fabricd binding"}
		}
	}
	return &agentConnection{sdk: sdk, closeClient: closeClient, finish: finish}, nil
}

type agentConnection struct {
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

func (c *agentConnection) AgentConfig(ctx context.Context, id string) (api.AgentConfig, error) {
	if id == "" {
		return api.AgentConfig{}, errors.New("AgentConfig ID is required")
	}
	var configs []api.AgentConfig
	if err := c.sdk.Call(ctx, "agent.config", api.AgentConfigRequest{Action: "list"}, &configs); err != nil {
		return api.AgentConfig{}, err
	}
	for _, config := range configs {
		if config.ID == id {
			return config, nil
		}
	}
	return api.AgentConfig{}, fmt.Errorf("AgentConfig %q is not present on this Runner", id)
}

func (c *agentConnection) Start(ctx context.Context, profile api.Profile) (api.Runtime, error) {
	if profile.Kind != "agent" || profile.Adapter != "acp" || !profile.ManagedACP {
		return api.Runtime{}, errors.New("IM Agent requires a managed ACP Profile")
	}
	if err := profile.Validate(); err != nil {
		return api.Runtime{}, err
	}
	runtime, stream, err := c.sdk.Start(ctx, profile)
	if stream != nil {
		_ = stream.Close()
	}
	return runtime, err
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

func (c *agentConnection) Action(ctx context.Context, runtime api.Runtime, action AgentAction) error {
	if action.Action != "new" && action.Action != "load" && action.Action != "prompt" {
		return errors.New("IM Agent action must be new, load or prompt")
	}
	if action.Action == "prompt" && action.Text == "" {
		return errors.New("IM Agent prompt text is required")
	}
	if action.Action == "load" && action.SessionID == "" {
		return errors.New("IM Agent session ID is required for load")
	}
	var accepted struct {
		Accepted bool `json:"accepted"`
	}
	if err := c.sdk.CallID(ctx, "acp.action", wire.ID(), action, &accepted, &runtime); err != nil {
		return err
	}
	if !accepted.Accepted {
		return errors.New("ACP action was not accepted")
	}
	return nil
}

func (c *agentConnection) Stop(ctx context.Context, runtime api.Runtime) error {
	return c.sdk.Stop(ctx, runtime)
}
