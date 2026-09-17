package api

import "encoding/json"

// ACPAction is a managed request. Prompt SessionID pins the native conversation
// observed by the caller; a queued prompt fails if an earlier new/load changes it.
type ACPAction struct {
	Action       string `json:"action"`
	Text         string `json:"text,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
	PermissionID string `json:"permission_id,omitempty"`
	OptionID     string `json:"option_id,omitempty"`
}

// AgentOperation identifies one accepted submission in the original fabricd
// Runtime. A completed ACP RPC does not imply the user's task passed acceptance.
type AgentOperation struct {
	Ref        string `json:"operation_ref"`
	State      string `json:"state"`
	StopReason string `json:"stop_reason,omitempty"`
	Error      string `json:"error,omitempty"`
	// NativeSessionID is confirmed only by a successful new/load response.
	NativeSessionID string `json:"native_session_id,omitempty"`
}

func (o AgentOperation) Terminal() bool {
	return o.State != "pending" && o.State != "running"
}

type AgentOperationWait struct {
	Ref       string `json:"operation_ref"`
	TimeoutMS int    `json:"timeout_ms"`
}

type AgentOperationRead struct {
	Ref      string `json:"operation_ref"`
	Position int64  `json:"position"`
	Limit    int    `json:"limit,omitempty"`
}

// Positions count ACP update envelopes within this operation, not a session
// event stream. Incomplete is sticky after any omission; false is not completion.
type AgentOperationOutput struct {
	AgentOperation
	Position     int64             `json:"position"`
	NextPosition int64             `json:"next_position"`
	Incomplete   bool              `json:"incomplete"`
	Output       []json.RawMessage `json:"output"`
}

// PTY submissions preserve native CLI semantics: delivered confirms only ordered
// terminal input, never completion or per-prompt output attribution.
type PTYPrompt struct {
	Text  string `json:"text"`
	Agent string `json:"agent"`
}

type PTYKeys struct {
	Keys  []string `json:"keys"`
	Agent string   `json:"agent"`
}
