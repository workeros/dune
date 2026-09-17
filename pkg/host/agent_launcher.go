package host

import (
	"context"

	"github.com/aiomni/dune/internal/agentservice"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/runner"
)

// AgentLauncher shares fixed-revision startup and recovery metadata between the
// workbench and Agent tools. It only uses existing authorized Runner bindings.
func (a *App) AgentLauncher() agents.Launcher { return &agentLauncher{app: a} }

type agentLauncher struct{ app *App }

func (l *agentLauncher) Start(ctx context.Context, scope agents.Scope, request agents.StartRequest) (agents.LaunchResult, error) {
	ctx, finish, err := l.app.adminContext(ctx)
	if err != nil {
		return agents.LaunchResult{}, err
	}
	defer finish()
	return l.app.agentService().Start(ctx, scope, request)
}

func (a *App) agentService() *agentservice.Service {
	return &agentservice.Service{Store: a.store, Access: a.authorizer, Environment: a.agentEnvironment, Online: a.agentOnline, MCPURL: a.agentMCPURL,
		Dial: func(ctx context.Context, scope agents.Scope, binding runner.Binding, operation string) (*client.Client, func(), error) {
			return (&runnerExecutor{app: a}).connect(ctx, scope.Principal, scope.OwnerID, binding, operation)
		},
	}
}
