package fabricd

import (
	"os"

	"github.com/aiomni/dune/internal/mcpbridge"
	"github.com/aiomni/dune/pkg/api"
)

// configureMCP freezes one credential before any native session is admitted.
// Every later new/load uses this same immutable config, including Web actions.
func (a *acpController) configureMCP(config api.AgentMCP) (api.AgentMCPStatus, error) {
	var result api.AgentMCPStatus
	if err := config.Validate(); err != nil {
		return result, &api.Error{Code: "INVALID_ARGUMENT", Detail: err.Error()}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.done:
		return result, &api.Error{Code: "STALE_RUNTIME", Detail: "ACP Runtime has exited"}
	default:
	}
	if !a.state.Ready {
		return result, &api.Error{Code: "AGENT_NOT_READY", Detail: "ACP initialization is not confirmed"}
	}
	if a.state.MCPTransport != "" || a.active != nil || len(a.queue) != 0 || a.nativeSequence != 0 || a.state.SessionID != "" {
		return result, &api.Error{Code: "CONFLICT", Detail: "MCP configuration is fixed before the first native session"}
	}
	var server any
	if a.mcpHTTP {
		result.Transport = "http"
		server = map[string]any{"type": "http", "name": "dune-agents", "url": config.URL,
			"headers": []map[string]string{{"name": "Authorization", "value": "Bearer " + config.Token}}}
	} else {
		executable, err := os.Executable()
		if err != nil {
			return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner cannot locate its native MCP bridge"}
		}
		result.Transport = "stdio"
		server = map[string]any{"name": "dune-agents", "command": executable, "args": []string{mcpbridge.Command},
			"env": []map[string]string{{"name": mcpbridge.URLEnv, "value": config.URL}, {"name": mcpbridge.TokenEnv, "value": config.Token}}}
	}
	a.mcpServers = []any{server}
	a.mcpSecret.Store(&config.Token)
	a.state.MCPTransport = result.Transport
	a.publishLocked()
	return result, nil
}
