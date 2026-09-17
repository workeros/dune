package tmux

import (
	"context"

	"github.com/aiomni/dune/internal/agentintegration"
	"github.com/aiomni/dune/pkg/api"
)

func (r *Session) ConfigureMCP(ctx context.Context, config api.AgentMCP) (api.AgentMCPStatus, error) {
	var result api.AgentMCPStatus
	if !r.native {
		return result, &api.Error{Code: "UNSUPPORTED", Detail: "PTY Runtime has no native Agent integration"}
	}
	pane, err := r.Inspect()
	if err != nil || pane.Dead {
		return result, &api.Error{Code: "STALE_RUNTIME", Detail: "PTY Runtime is no longer running"}
	}
	if err := agentintegration.ConfigureMCP(ctx, r.nativeDir(), r.Runtime.ID, r.Runtime.Incarnation, config); err != nil {
		if failure, ok := err.(*api.Error); ok {
			return result, failure
		}
		return result, &api.Error{Code: "MCP_CONFIGURATION_FAILED", Detail: "Runner could not confirm native MCP configuration"}
	}
	return api.AgentMCPStatus{Transport: "stdio"}, nil
}
