package host

import (
	"context"

	"github.com/aiomni/dune/internal/agentservice"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
)

func (a *App) AgentNativeSessions() agents.NativeSessions { return &agentNativeSessions{app: a} }

type agentNativeSessions struct{ app *App }

func (s *agentNativeSessions) OpenSession(ctx context.Context, scope agents.Scope, request agents.OpenSessionRequest) (agents.Operation, error) {
	ctx, finish, err := s.app.adminContext(ctx)
	if err != nil {
		receipt, failure := agentservice.UnsentSubmission(scope.OwnerID, request.AgentRef, request.SubmissionID, err)
		return agents.Operation{AgentOperation: api.AgentOperation{Submission: &receipt}}, failure
	}
	defer finish()
	return s.app.agentService().OpenSession(ctx, scope, request)
}
