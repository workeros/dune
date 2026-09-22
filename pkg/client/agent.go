package client

import (
	"context"
	"slices"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// ACPState discovers the current model without attaching or opening a session.
func (c *Client) ACPState(ctx context.Context, runtime api.Runtime) (api.ACPState, error) {
	var state api.ACPState
	err := c.callACPRead(ctx, "acp.state", struct{}{}, &state, runtime)
	return state, err
}

func (c *Client) ReadACPConversation(ctx context.Context, runtime api.Runtime, request api.ACPConversationRead) (api.ACPConversationPage, error) {
	var page api.ACPConversationPage
	err := c.callACPRead(ctx, "acp.conversation.read", request, &page, runtime)
	return page, err
}

func (c *Client) GetACPConversationEntries(ctx context.Context, runtime api.Runtime, request api.ACPConversationGet) (api.ACPConversationEntries, error) {
	var entries api.ACPConversationEntries
	err := c.callACPRead(ctx, "acp.conversation.get", request, &entries, runtime)
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
func (c *Client) ACPSubmit(ctx context.Context, key api.SubmissionKey, action api.ACPAction) (api.AgentOperation, error) {
	if action.Action != "new" && action.Action != "load" && action.Action != "resume" && action.Action != "list" && action.Action != "prompt" {
		return api.AgentOperation{Submission: &api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}}, &api.SubmissionError{Key: key, Cause: &api.Error{Code: "INVALID_ARGUMENT", Detail: "ACPSubmit requires new, load, list or prompt"}}
	}
	receipt, err := c.Submit(ctx, api.SubmissionRequest{SubmissionKey: key, Operation: "acp.action", Payload: api.Payload(action)})
	if err != nil {
		return api.AgentOperation{Submission: &receipt}, err
	}
	// This is the initial admitted operation, not a claim that it is still
	// pending when the caller receives it. Wait/read observes current execution.
	return api.AgentOperation{Ref: receipt.OperationRef, State: "pending", Submission: &receipt}, nil
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

// SubscribeACPConversation acknowledges an ordinary observer subscription. Read
// state/pages after this call succeeds. Notifications invalidate retained entry
// values; they contain no transcript and do not acknowledge browser progress.
func (c *Client) SubscribeACPConversation(ctx context.Context, runtime api.Runtime) (*Stream, error) {
	if !slices.Contains(c.Binding.Capabilities, "acp.conversation.changed") {
		return nil, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not advertise conversation notifications"}
	}
	stream, _, err := c.open(ctx, "runtime.attach", wire.ID(), api.Attach{Observe: true, Conversation: true}, &runtime)
	return stream, err
}

// Read cancellation has no Agent side effect and is not an unknown write.
func (c *Client) callACPRead(parent context.Context, operation string, request, response any, runtime api.Runtime) error {
	if !slices.Contains(c.Binding.Capabilities, operation) {
		return &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not advertise " + operation}
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	err := c.CallID(ctx, operation, wire.ID(), request, response, &runtime)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ACPControl admits an exact permission answer or prompt cancellation using the
// target's reserved capacity. Stage "written" confirms pipe delivery only.
// Its receipt is queried by key; it does not consume ordinary operation slots.
func (c *Client) ACPControl(ctx context.Context, key api.SubmissionKey, action api.ACPAction) (api.SubmissionReceipt, error) {
	if action.Action != "permission" && action.Action != "elicitation" && action.Action != "cancel" {
		return api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}, &api.SubmissionError{Key: key, Cause: &api.Error{Code: "INVALID_ARGUMENT", Detail: "ACPControl requires permission, elicitation or cancel"}}
	}
	return c.Submit(ctx, api.SubmissionRequest{SubmissionKey: key, Operation: "acp.action", Payload: api.Payload(action)})
}
