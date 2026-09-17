package host

import (
	"context"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/runner"
)

// AgentDirectory discovers Owner/Tenant Agents through the same Gateway path
// as execution, including remote Pod routes, and reconciles native confirmations.
func (a *App) AgentDirectory() agents.Directory { return &agentDirectory{app: a} }

type agentDirectory struct{ app *App }

func (d *agentDirectory) List(ctx context.Context, scope agents.Scope, query runner.Query) (agents.DirectoryPage, error) {
	ctx, finish, err := d.app.adminContext(ctx)
	if err != nil {
		return agents.DirectoryPage{}, err
	}
	defer finish()
	return d.app.agentService().List(ctx, scope, query)
}

func (d *agentDirectory) Get(ctx context.Context, scope agents.Scope, ref string) (agents.Agent, error) {
	ctx, finish, err := d.app.adminContext(ctx)
	if err != nil {
		return agents.Agent{}, err
	}
	defer finish()
	return d.app.agentService().Get(ctx, scope, ref)
}
