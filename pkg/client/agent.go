package client

import (
	"context"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// ACPSubmit admits a new/load/list/prompt to the Runtime queue. Closing this
// connection never cancels it. Unknown submissions must not be replayed.
func (c *Client) ACPSubmit(ctx context.Context, runtime api.Runtime, action api.ACPAction) (api.AgentOperation, error) {
	var operation api.AgentOperation
	if action.Action != "new" && action.Action != "load" && action.Action != "list" && action.Action != "prompt" {
		return operation, &api.Error{Code: "INVALID_ARGUMENT", Detail: "ACPSubmit requires new, load, list or prompt"}
	}
	err := c.CallID(ctx, "acp.action", wire.ID(), action, &operation, &runtime)
	return operation, err
}

func (c *Client) WaitAgentOperation(ctx context.Context, runtime api.Runtime, request api.AgentOperationWait) (api.AgentOperation, error) {
	var operation api.AgentOperation
	err := c.CallID(ctx, "agent.operation.wait", wire.ID(), request, &operation, &runtime)
	return operation, err
}

func (c *Client) ReadAgentOperation(ctx context.Context, runtime api.Runtime, request api.AgentOperationRead) (api.AgentOperationOutput, error) {
	var output api.AgentOperationOutput
	err := c.CallID(ctx, "agent.operation.read", wire.ID(), request, &output, &runtime)
	return output, err
}
