package agentservice

import (
	"context"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
)

// The Runtime receipt is already saved before issuing a usable credential.
// Credentials never enter the immutable launch snapshot or a public result.
func (s *Service) configureMCP(ctx context.Context, scope agents.Scope, connection *client.Client, runtime api.Runtime, session agents.Summary) error {
	token, err := s.Store.IssueAgentCredential(ctx, scope, session.ID, session.Attempt.ID, time.Now().Add(metadata.AgentCredentialLifetime))
	if err != nil {
		return &api.Error{Code: "MCP_CONFIGURATION_FAILED", Detail: "Runtime started but its Agent credential could not be issued; no native session was submitted"}
	}
	_, err = connection.ConfigureAgentMCP(ctx, runtime, api.AgentMCP{URL: s.MCPURL, Token: token})
	if err != nil {
		// A lost configure reply can follow acceptance. Do not rotate the token
		// or repeat configuration; the known Runtime remains in the result.
		return &api.Error{Code: "MCP_CONFIGURATION_FAILED", Detail: "Runtime started but MCP configuration was not confirmed; inspect this Runtime before another start"}
	}
	return nil
}
