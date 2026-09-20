package agents

import "context"

// OpenSessionRequest explicitly creates or loads a native conversation in an
// existing managed ACP Runtime. It does not start another Runtime.
type OpenSessionRequest struct {
	SubmissionID string `json:"submission_id" jsonschema:"Caller-owned ID saved before first send."`
	AgentRef     string `json:"agent_ref"`
	Action       string `json:"action"`
	SessionID    string `json:"session_id,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	WaitMS       int    `json:"wait_ms,omitempty"`
}

type NativeSessions interface {
	OpenSession(context.Context, Scope, OpenSessionRequest) (Operation, error)
}
