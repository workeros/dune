package host

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/aiomni/dune/internal/agentmcp"
	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
)

func (a *App) newAgentMCP(namespace, origin string) http.Handler {
	return agentmcp.New(agentmcp.Options{
		Origin: origin, Directory: a.AgentDirectory(), Launcher: a.AgentLauncher(), Messenger: a.AgentMessenger(),
		Profiles: a.agentProfilePage,
		Authenticate: func(ctx context.Context, token string) (agents.Scope, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			credential, err := a.store.ReadAgentCredential(ctx, token, namespace)
			if err != nil {
				return agents.Scope{}, err
			}
			if err := a.agentService().VerifyCaller(ctx, credential.Scope, credential.Target); err != nil {
				var failure *api.Error
				if errors.Is(err, metadata.ErrNotFound) || errors.Is(err, authorization.ErrNotFound) || errors.Is(err, access.ErrDenied) || errors.Is(err, runner.ErrBindingChanged) || errors.As(err, &failure) && (failure.Code == "STALE_RUNTIME" || failure.Code == "CALLER_EXITED") {
					return agents.Scope{}, identity.ErrUnauthorized
				}
				return agents.Scope{}, err
			}
			return credential.Scope, nil
		},
	})
}

func (a *App) agentProfilePage(ctx context.Context, scope agents.Scope, query runner.Query) (agentmcp.ProfilePage, error) {
	result := agentmcp.ProfilePage{Items: []agentmcp.ProfileSummary{}}
	if _, err := a.authorizer.Check(ctx, scope.Principal, authorization.Resource{OwnerID: scope.OwnerID}, "workspace.read", ""); err != nil {
		return result, err
	}
	limit := query.Limit
	if limit == 0 {
		limit = 32
	}
	if limit < 1 || limit > 100 {
		return result, &api.Error{Code: "INVALID_ARGUMENT", Detail: "limit must be 1..100"}
	}
	items, err := a.store.Profiles().ListAgentPage(ctx, scope.OwnerID, query.Cursor, limit+1)
	if err != nil {
		return result, err
	}
	if len(items) > limit {
		items = items[:limit]
		result.NextCursor = items[len(items)-1].ID
	}
	for _, item := range items {
		result.Items = append(result.Items, agentmcp.ProfileSummary{ID: item.ID, Revision: item.Revision, Name: item.Name, Description: item.Description, Adapter: item.Profile.Adapter})
	}
	return result, nil
}
