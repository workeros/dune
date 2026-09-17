package host

import (
	"context"

	"github.com/aiomni/dune/pkg/agents"
)

func (a *App) AgentMessenger() agents.Messenger { return &agentMessenger{app: a} }

type agentMessenger struct{ app *App }

func (m *agentMessenger) Prompt(ctx context.Context, scope agents.Scope, request agents.PromptRequest) (agents.Operation, error) {
	ctx, finish, err := m.app.adminContext(ctx)
	if err != nil {
		return agents.Operation{}, err
	}
	defer finish()
	return m.app.agentService().Prompt(ctx, scope, request)
}

func (m *agentMessenger) SendKeys(ctx context.Context, scope agents.Scope, request agents.KeysRequest) (agents.Operation, error) {
	ctx, finish, err := m.app.adminContext(ctx)
	if err != nil {
		return agents.Operation{}, err
	}
	defer finish()
	return m.app.agentService().SendKeys(ctx, scope, request)
}

func (m *agentMessenger) Wait(ctx context.Context, scope agents.Scope, request agents.WaitRequest) (agents.WaitResult, error) {
	ctx, finish, err := m.app.adminContext(ctx)
	if err != nil {
		return agents.WaitResult{}, err
	}
	defer finish()
	return m.app.agentService().Wait(ctx, scope, request)
}

func (m *agentMessenger) Read(ctx context.Context, scope agents.Scope, request agents.ReadRequest) (agents.ReadResult, error) {
	ctx, finish, err := m.app.adminContext(ctx)
	if err != nil {
		return agents.ReadResult{}, err
	}
	defer finish()
	return m.app.agentService().Read(ctx, scope, request)
}
