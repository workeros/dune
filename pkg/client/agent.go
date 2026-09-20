package client

import (
	"context"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// ACPState discovers the current model without attaching or opening a session.
func (c *Client) ACPState(ctx context.Context, runtime api.Runtime) (api.ACPState, error) {
	var state api.ACPState
	err := c.CallID(ctx, "acp.state", wire.ID(), struct{}{}, &state, &runtime)
	return state, err
}

func (c *Client) ReadACPConversation(ctx context.Context, runtime api.Runtime, request api.ACPConversationRead) (api.ACPConversationPage, error) {
	var page api.ACPConversationPage
	err := c.CallID(ctx, "acp.conversation.read", wire.ID(), request, &page, &runtime)
	return page, err
}

func (c *Client) GetACPConversationEntries(ctx context.Context, runtime api.Runtime, request api.ACPConversationGet) (api.ACPConversationEntries, error) {
	var entries api.ACPConversationEntries
	err := c.CallID(ctx, "acp.conversation.get", wire.ID(), request, &entries, &runtime)
	return entries, err
}

// ConfigureAgentMCP releases the ACP session gate or native PTY MCP bridge.
// An unconfirmed response must not trigger a new launch or token rotation.
func (c *Client) ConfigureAgentMCP(ctx context.Context, runtime api.Runtime, config api.AgentMCP) (api.AgentMCPStatus, error) {
	var status api.AgentMCPStatus
	err := c.CallID(ctx, "agent.mcp.configure", wire.ID(), config, &status, &runtime)
	return status, err
}

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

func (c *Client) PTYPrompt(ctx context.Context, runtime api.Runtime, request api.PTYPrompt) (api.AgentOperation, error) {
	var operation api.AgentOperation
	err := c.CallID(ctx, "pty.prompt", wire.ID(), request, &operation, &runtime)
	return operation, err
}

func (c *Client) PTYSendKeys(ctx context.Context, runtime api.Runtime, request api.PTYKeys) (api.AgentOperation, error) {
	var operation api.AgentOperation
	err := c.CallID(ctx, "pty.keys", wire.ID(), request, &operation, &runtime)
	return operation, err
}

func (c *Client) CaptureTerminal(ctx context.Context, runtime api.Runtime) (api.TerminalSnapshot, error) {
	var snapshot api.TerminalSnapshot
	err := c.CallID(ctx, "runtime.capture", wire.ID(), struct{}{}, &snapshot, &runtime)
	return snapshot, err
}

func (c *Client) ScrollbackTerminal(ctx context.Context, runtime api.Runtime, request api.TerminalScrollbackRequest) (api.TerminalScrollback, error) {
	var snapshot api.TerminalScrollback
	err := c.CallID(ctx, "runtime.scrollback", wire.ID(), request, &snapshot, &runtime)
	return snapshot, err
}
