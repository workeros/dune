package agents

import (
	"context"

	"github.com/aiomni/dune/pkg/api"
)

type PromptRequest struct {
	SubmissionID           string `json:"submission_id" jsonschema:"Caller-owned ID saved before first send; required for managed ACP."`
	ExpectedConversationID string `json:"expected_conversation_id,omitempty" jsonschema:"Caller-observed conversation_id; required for managed ACP. Never refresh automatically on retry."`
	AgentRef               string `json:"agent_ref"`
	Text                   string `json:"text"`
	WaitMS                 int    `json:"wait_ms,omitempty"`
}

type KeysRequest struct {
	AgentRef string   `json:"agent_ref"`
	Keys     []string `json:"keys"`
}

// Operation references include the execution target so another host can route
// directly to fabricd. They grant no authority and do not imply persistence.
type Operation struct {
	api.AgentOperation
}

type OperationOutput struct {
	api.AgentOperationOutput
}

// Select exactly one reference. Until applies only to Agent activity: idle,
// blocked, exited, or attention (the default: any of those three states).
type WaitRequest struct {
	OperationRef string `json:"operation_ref,omitempty"`
	AgentRef     string `json:"agent_ref,omitempty"`
	TimeoutMS    int    `json:"timeout_ms"`
	Until        string `json:"until,omitempty"`
}

type WaitResult struct {
	Operation *Operation `json:"operation,omitempty"`
	Agent     *Agent     `json:"agent,omitempty"`
	TimedOut  bool       `json:"timed_out"`
}

// AgentRef explicitly selects a PTY screen snapshot, not a prompt's output.
// OperationRef selects ACP output. Position/Limit apply only to an operation.
type ReadRequest struct {
	OperationRef string `json:"operation_ref,omitempty"`
	AgentRef     string `json:"agent_ref,omitempty"`
	Position     int64  `json:"position,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type Snapshot struct {
	Agent    Agent                `json:"agent"`
	Terminal api.TerminalSnapshot `json:"terminal"`
}

type ReadResult struct {
	Operation *OperationOutput `json:"operation,omitempty"`
	Snapshot  *Snapshot        `json:"snapshot,omitempty"`
}

// Messenger owns neither queues nor output buffers. Every call reauthorizes
// the scope and uses SDK/Gateway to reach the selected fabricd Runtime.
type Messenger interface {
	Submit(context.Context, Scope, SubmissionRequest) (api.SubmissionReceipt, error)
	QuerySubmission(context.Context, Scope, SubmissionQuery) (api.SubmissionReceipt, error)
	Prompt(context.Context, Scope, PromptRequest) (Operation, error)
	SendKeys(context.Context, Scope, KeysRequest) (Operation, error)
	Wait(context.Context, Scope, WaitRequest) (WaitResult, error)
	Read(context.Context, Scope, ReadRequest) (ReadResult, error)
}
