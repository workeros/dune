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
	service := agentservice.Service{Store: l.app.store, Access: l.app.authorizer, Environment: l.app.agentEnvironment,
		Dial: func(ctx context.Context, scope agents.Scope, binding runner.Binding) (*client.Client, func(), error) {
			return (&runnerExecutor{app: l.app}).connect(ctx, scope.Principal, scope.OwnerID, binding, "profile.start")
		},
	}
	return service.Start(ctx, scope, request)
}
