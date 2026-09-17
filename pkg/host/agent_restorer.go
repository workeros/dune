package host

import (
	"context"

	"github.com/aiomni/dune/pkg/agents"
)

func (a *App) AgentRestorer() agents.Restorer { return &agentRestorer{app: a} }

type agentRestorer struct{ app *App }

func (r *agentRestorer) Resume(ctx context.Context, scope agents.Scope, request agents.ResumeRequest) (agents.ResumeResult, error) {
	ctx, finish, err := r.app.adminContext(ctx)
	if err != nil {
		return agents.ResumeResult{}, err
	}
	defer finish()
	return r.app.agentService().Resume(ctx, scope, request)
}
