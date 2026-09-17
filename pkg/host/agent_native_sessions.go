package host

import (
	"context"

	"github.com/aiomni/dune/pkg/agents"
)

func (a *App) AgentNativeSessions() agents.NativeSessions { return &agentNativeSessions{app: a} }

type agentNativeSessions struct{ app *App }

func (s *agentNativeSessions) OpenSession(ctx context.Context, scope agents.Scope, request agents.OpenSessionRequest) (agents.Operation, error) {
	ctx, finish, err := s.app.adminContext(ctx)
	if err != nil {
		return agents.Operation{}, err
	}
	defer finish()
	return s.app.agentService().OpenSession(ctx, scope, request)
}
