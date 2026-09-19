package api

import "encoding/json"

// ACPAction is a managed request. Prompt SessionID and Cwd pin the native
// conversation observed by the caller; omitted values bind at admission. A
// queued prompt fails if an earlier new/load changes either value.
type ACPAction struct {
	Action       string `json:"action"`
	Text         string `json:"text,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
	PermissionID string `json:"permission_id,omitempty"`
	OptionID     string `json:"option_id,omitempty"`
}

// NativeSession is a confirmed native conversation, observed from a matching
// new/load response or a supported native hook. Sequence orders confirmations
// within one Runtime; it describes native state, not an output cursor.
// A failed or pending session switch does not replace the last confirmation.
type NativeSession struct {
	ID              string `json:"id"`
	Cwd             string `json:"cwd"`
	Sequence        int64  `json:"sequence"`
	Source          string `json:"source"`
	AgentVersion    string `json:"agent_version,omitempty"`
	ResumeSupported bool   `json:"resume_supported"`
}

// AgentOperation identifies one accepted submission in the original fabricd
// Runtime. A completed ACP RPC does not imply the user's task passed acceptance.
type AgentOperation struct {
	Ref        string `json:"operation_ref"`
	State      string `json:"state"`
	StopReason string `json:"stop_reason,omitempty"`
	Error      string `json:"error,omitempty"`
	// NativeSession belongs to this operation's matching successful response,
	// even when another caller has since changed the Runtime's session.
	NativeSession *NativeSession `json:"native_session,omitempty"`
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
// SessionID/Cwd pin the last native confirmation; omit both to bind at admission.
type PTYPrompt struct {
	Text      string `json:"text"`
	Agent     string `json:"agent"`
	SessionID string `json:"session_id,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

type PTYKeys struct {
	Keys      []string `json:"keys"`
	Agent     string   `json:"agent"`
	SessionID string   `json:"session_id,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
}

// TerminalSnapshot contains the current screen only. History counts describe
// native scrollback; Content is not the history or a per-prompt transcript.
type TerminalSnapshot struct {
	Content      string `json:"content"`
	Rows         int    `json:"rows"`
	Cols         int    `json:"cols"`
	HistoryLines int    `json:"history_lines"`
	HistoryLimit int    `json:"history_limit"`
}

const (
	DefaultTerminalScrollbackLines = 5000
	MaxTerminalScrollbackLines     = 10000
	// Even worst-case JSON escaping fits within the protocol message limit.
	MaxTerminalScrollbackBytes = 512 * 1024
)

type TerminalScrollbackRequest struct {
	// Limit counts physical rows, including the current screen. Zero uses the default.
	Limit int `json:"limit,omitempty"`
}

// TerminalScrollback is a read-only snapshot of tmux's retained history and
// active screen. Content contains the newest complete physical rows, each with
// a trailing LF, without ANSI sequences. Soft wraps remain separate rows.
type TerminalScrollback struct {
	Content       string `json:"content"`
	Cols          int    `json:"cols"`
	Rows          int    `json:"rows"`
	HistoryLines  int    `json:"history_lines"`  // Retained history, excluding the screen.
	CapturedLines int    `json:"captured_lines"` // Rows actually returned, including blank rows.
	// True if rows/bytes were omitted or tmux may have evicted older history.
	// False does not guarantee a complete process transcript (for example after clear or reflow).
	Truncated bool `json:"truncated"`
}
