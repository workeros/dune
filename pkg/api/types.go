// Package api defines the JSON payload schemas carried by Dune protobuf messages.
package api

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const Version = "dune-mvp/2"

const (
	MaxProfileBytes               = 1 << 20
	MaxProfileAttempts            = 256
	ProfileStatusRetentionSeconds = 60
	MaxExecOutputBytes            = 128 * 1024
	MaxProfileStepNameBytes       = 4 * 1024
	MaxProfileFailureDetailBytes  = 4 * 1024
)

type Hello struct {
	Version string `json:"version"`
	Role    string `json:"role"`
	// Peer identities name Gateway boots, not users or machine credentials.
	PeerSource string `json:"peer_source,omitempty"`
	PeerOwner  string `json:"peer_owner,omitempty"`
}
type Binding struct {
	Version      string         `json:"version"`
	Target       string         `json:"target"`
	Incarnation  string         `json:"incarnation"`
	Generation   uint64         `json:"generation"`
	RouteEpoch   uint64         `json:"route_epoch,omitempty"`
	Capabilities []string       `json:"capabilities"`
	Limits       map[string]int `json:"limits"`
}
type Command struct {
	Name           string   `json:"name,omitempty" yaml:"name,omitempty"`
	Argv           []string `json:"argv,omitempty" yaml:"argv,omitempty"`
	Run            string   `json:"run,omitempty" yaml:"run,omitempty"`
	Shell          string   `json:"shell,omitempty" yaml:"shell,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
}
type Profile struct {
	// Informational project labels; they confer no authority.
	ProjectID        string            `json:"project_id,omitempty" yaml:"project_id,omitempty"`
	DirectoryID      string            `json:"directory_id,omitempty" yaml:"directory_id,omitempty"`
	Version          int               `json:"version" yaml:"version"`
	Kind             string            `json:"kind" yaml:"kind"`
	WorkingDirectory string            `json:"working_directory" yaml:"working_directory"`
	Env              map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Setup            struct {
		Steps []Command `json:"steps" yaml:"steps"`
	} `json:"setup" yaml:"setup"`
	Start        Command `json:"start" yaml:"start"`
	Adapter      string  `json:"adapter" yaml:"adapter"`
	HistoryLines int     `json:"history_lines,omitempty" yaml:"history_lines,omitempty"`
	ManagedACP   bool    `json:"managed_acp,omitempty" yaml:"managed_acp,omitempty"`
	ACPV2Draft   bool    `json:"acp_v2_draft,omitempty" yaml:"acp_v2_draft,omitempty"`
	// ACPElicitation requires a host that can present and answer form/URL requests,
	// including requests received before initialization or session creation ends.
	ACPElicitation bool `json:"acp_elicitation,omitempty" yaml:"acp_elicitation,omitempty"`
	// RequireAgentMCP gates ACP new/load or the native PTY MCP bridge until configured.
	RequireAgentMCP bool `json:"require_agent_mcp,omitempty" yaml:"require_agent_mcp,omitempty"`
}

// ProfileResult reports successful completion of a non-interactive Profile.
// Step output is delivered as bounded progress messages while the Profile runs.
type ProfileResult struct {
	Kind           string `json:"kind"`
	Stage          string `json:"stage"`
	StepsCompleted int    `json:"steps_completed"`
}

// ProfileFailure is the stable, bounded failure detail for one Profile attempt.
// Step is zero-based and -1 when failure happened outside a setup step.
type ProfileFailure struct {
	Code       string      `json:"code"`
	Detail     string      `json:"detail"`
	Step       int         `json:"step"`
	StepName   string      `json:"step_name,omitempty"`
	StepResult *ExecResult `json:"step_result,omitempty"`
}

// ProfileProgress is emitted while a Profile attempt is executing. A nil
// StepResult means the named step has started but has not completed.
type ProfileProgress struct {
	ExecutionID    string          `json:"execution_id"`
	Stage          string          `json:"stage"`
	Step           int             `json:"step"`
	StepName       string          `json:"step_name,omitempty"`
	StepsCompleted int             `json:"steps_completed"`
	StepResult     *ExecResult     `json:"step_result,omitempty"`
	Failure        *ProfileFailure `json:"failure,omitempty"`
}

// ProfileStatus is an in-memory observation of one Profile execution attempt.
// State is running, succeeded, failed, or unknown.
type ProfileStatus struct {
	ExecutionID string          `json:"execution_id"`
	Kind        string          `json:"kind,omitempty"`
	State       string          `json:"state"`
	Progress    ProfileProgress `json:"progress"`
	Result      *ProfileResult  `json:"result,omitempty"`
	Failure     *ProfileFailure `json:"failure,omitempty"`
}

type ProfileStatusRequest struct {
	ExecutionID string `json:"execution_id"`
}

func ValidateExecutionID(id string) error {
	if id == "" || len(id) > 128 || strings.ContainsFunc(id, unicode.IsControl) {
		return fmt.Errorf("execution ID must be 1..128 bytes without control characters")
	}
	return nil
}

func (c Command) Args() ([]string, error) {
	if (len(c.Argv) > 0) == (c.Run != "") {
		return nil, fmt.Errorf("exactly one of argv/run required")
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 86400 {
		return nil, fmt.Errorf("timeout_seconds must be 0..86400")
	}
	if len(c.Argv) > 0 {
		if c.Shell != "" || c.Argv[0] == "" {
			return nil, fmt.Errorf("argv cannot use shell or empty executable")
		}
		return c.Argv, nil
	}
	if !filepath.IsAbs(c.Shell) {
		return nil, fmt.Errorf("run requires absolute shell")
	}
	return []string{c.Shell, "-c", c.Run}, nil
}
func (p Profile) Validate() error {
	encoded, err := json.Marshal(p)
	if err != nil || len(encoded) > MaxProfileBytes {
		return fmt.Errorf("Profile exceeds %d bytes", MaxProfileBytes)
	}
	for _, label := range []string{p.ProjectID, p.DirectoryID} {
		if len(label) > 256 || strings.ContainsFunc(label, unicode.IsControl) {
			return fmt.Errorf("invalid project label")
		}
	}
	if p.DirectoryID != "" && p.ProjectID == "" || p.Kind != "agent" && p.ProjectID != "" {
		return fmt.Errorf("project labels require an Agent and a project")
	}
	if p.Version != 1 {
		return fmt.Errorf("version: 1 required")
	}
	if p.Kind != "environment" && p.Kind != "agent" {
		return fmt.Errorf("kind must be environment or agent")
	}
	if !filepath.IsAbs(p.WorkingDirectory) {
		return fmt.Errorf("working_directory must be absolute")
	}
	for k, v := range p.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return fmt.Errorf("invalid environment")
		}
	}
	if len(p.Setup.Steps) > 64 {
		return fmt.Errorf("at most 64 setup steps")
	}
	for _, c := range p.Setup.Steps {
		if len(c.Name) > MaxProfileStepNameBytes {
			return fmt.Errorf("setup step name exceeds %d bytes", MaxProfileStepNameBytes)
		}
		if _, e := c.Args(); e != nil {
			return e
		}
	}
	if p.Kind == "environment" {
		if len(p.Start.Argv) != 0 || p.Start.Run != "" || p.Start.Shell != "" || p.Start.Name != "" || p.Start.TimeoutSeconds != 0 {
			return fmt.Errorf("environment Profile cannot include start")
		}
		if p.Adapter != "" || p.ManagedACP || p.ACPV2Draft || p.ACPElicitation || p.RequireAgentMCP || p.HistoryLines != 0 {
			return fmt.Errorf("environment Profile cannot include Agent adapter or history")
		}
		return nil
	}
	if p.Adapter != "pty" && p.Adapter != "acp" {
		return fmt.Errorf("agent Profile requires adapter: pty or acp")
	}
	if p.ACPV2Draft && !p.ManagedACP {
		return fmt.Errorf("acp_v2_draft requires managed ACP")
	}
	if p.ACPElicitation && !p.ManagedACP {
		return fmt.Errorf("acp_elicitation requires managed ACP")
	}
	if p.ManagedACP && p.Adapter != "acp" {
		return fmt.Errorf("managed_acp requires ACP adapter")
	}
	if p.RequireAgentMCP && p.Adapter == "acp" && !p.ManagedACP {
		return fmt.Errorf("require_agent_mcp requires managed ACP or an integrated PTY launch")
	}
	if p.HistoryLines < 0 || p.HistoryLines > 200000 || (p.Adapter != "pty" && p.HistoryLines != 0) {
		return fmt.Errorf("history_lines must be 0 (default) or 1..200000, PTY only")
	}
	if _, e := p.Start.Args(); e != nil {
		return e
	}
	return nil
}

type Exec struct {
	Command
	WorkingDirectory string            `json:"working_directory"`
	Env              map[string]string `json:"env,omitempty"`
}
type ExecResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out"`
	Truncated       bool   `json:"truncated"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}
type Runtime struct {
	// Availability describes the current connection, independently of State.
	// Unavailable preserves the last confirmed lifecycle fact.
	// Lost requires independent proof that the original host/group no longer exist.
	Availability     string         `json:"availability,omitempty"`
	LastConfirmedAt  *time.Time     `json:"last_confirmed_at,omitempty"`
	PersistentACP    bool           `json:"persistent_acp,omitempty"`
	ACPMode          string         `json:"acp_mode,omitempty"` // managed or raw
	ACPHost          *ACPHostInfo   `json:"acp_host,omitempty"`
	ConversationID   string         `json:"conversation_id,omitempty"`
	ProjectID        string         `json:"project_id,omitempty"`
	DirectoryID      string         `json:"directory_id,omitempty"`
	Title            string         `json:"title,omitempty"`
	WorkingDirectory string         `json:"working_directory,omitempty"`
	ID               string         `json:"id"`
	Incarnation      string         `json:"incarnation"`
	Generation       uint64         `json:"generation"`
	Adapter          string         `json:"adapter"`
	State            string         `json:"state"`
	ExitCode         *int           `json:"exit_code,omitempty"`
	StopReason       string         `json:"stop_reason,omitempty"`
	StartedAt        *time.Time     `json:"started_at,omitempty"`
	DeadlineAt       *time.Time     `json:"deadline_at,omitempty"`
	Activity         *AgentActivity `json:"activity,omitempty"`
	NativeSession    *NativeSession `json:"native_session,omitempty"`
}

// AgentActivity is a lightweight observation, separate from process liveness.
// Unknown is deliberate when a PTY has no supported activity integration.
// Epoch/Sequence identify changes for browser unread indicators, not prompt results.
type AgentActivity struct {
	State      string `json:"state"`
	Source     string `json:"source"`
	Agent      string `json:"agent,omitempty"`
	Foreground string `json:"foreground,omitempty"`
	Epoch      string `json:"epoch"`
	Sequence   int64  `json:"sequence"`
}
type Attach struct {
	// Conversation selects state and model invalidations, without raw diagnostics.
	Conversation bool `json:"conversation,omitempty"`
	Observe      bool `json:"observe"`
}

// TerminalControl is scoped to one attached stream. Acquire never preempts;
// take is an explicit user action. Observe-only streams cannot acquire control.
type TerminalControl struct {
	Action string `json:"action"`
}

type TerminalControlState struct {
	Writable  bool `json:"writable"`
	Available bool `json:"available"`
}

type Resize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}
type File struct {
	Action           string         `json:"action"`
	Path             string         `json:"path"`
	Destination      string         `json:"destination,omitempty"`
	Data             []byte         `json:"data,omitempty"`
	Offset           int64          `json:"offset,omitempty"`
	Length           int            `json:"length,omitempty"`
	Cursor           string         `json:"cursor,omitempty"`
	Limit            int            `json:"limit,omitempty"`
	Search           *SearchOptions `json:"search,omitempty"`
	Intent           string         `json:"intent,omitempty"`
	WithRevision     bool           `json:"with_revision,omitempty"`
	ExpectedRevision string         `json:"expected_revision,omitempty"`
	Overwrite        bool           `json:"overwrite,omitempty"`
	Recursive        bool           `json:"recursive,omitempty"`
}
type FileInfo struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Mode        uint32 `json:"mode"`
	IsDir       bool   `json:"is_dir"`
	ModifiedAt  string `json:"modified_at"`
	ContentHash string `json:"content_hash,omitempty"`
	Revision    string `json:"revision,omitempty"`
}
type FilePage struct {
	Items      []FileInfo `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}
type FileChunk struct {
	Data   []byte   `json:"data"`
	Offset int64    `json:"offset"`
	EOF    bool     `json:"eof"`
	Info   FileInfo `json:"info"`
}
type Upload struct {
	Action           string `json:"action"`
	ID               string `json:"id,omitempty"`
	Path             string `json:"path,omitempty"`
	Size             int64  `json:"size,omitempty"`
	SHA256           string `json:"sha256,omitempty"`
	Offset           int64  `json:"offset,omitempty"`
	Data             []byte `json:"data,omitempty"`
	ChunkSHA256      string `json:"chunk_sha256,omitempty"`
	ExpectedRevision string `json:"expected_revision,omitempty"`
	Intent           string `json:"intent,omitempty"`
	TTLSeconds       int    `json:"ttl_seconds,omitempty"`
}
type UploadState struct {
	ID          string    `json:"id"`
	Incarnation string    `json:"incarnation"`
	Offset      int64     `json:"offset"`
	Size        int64     `json:"size"`
	ExpiresAt   string    `json:"expires_at"`
	Committed   bool      `json:"committed"`
	File        *FileInfo `json:"file,omitempty"`
}
type Git struct {
	Action    string   `json:"action"`
	Directory string   `json:"directory"`
	Paths     []string `json:"paths,omitempty"`
	Patch     string   `json:"patch,omitempty"`
	Ref       string   `json:"ref,omitempty"`
	Name      string   `json:"name,omitempty"`
	Remote    string   `json:"remote,omitempty"`
	Message   string   `json:"message,omitempty"`
	Mode      string   `json:"mode,omitempty"`
	Staged    bool     `json:"staged,omitempty"`
	Create    bool     `json:"create,omitempty"`
	Limit     int      `json:"limit,omitempty"`
}
type GitEntry struct {
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
	Path     string `json:"path"`
	Original string `json:"original,omitempty"`
}
type GitBranch struct {
	Ref            string `json:"ref"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	Commit         string `json:"commit"`
	SymbolicTarget string `json:"symbolic_target,omitempty"`
	Current        bool   `json:"current"`
}
type GitHead struct {
	Ref      string `json:"ref,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Detached bool   `json:"detached"`
	Unborn   bool   `json:"unborn"`
}
type GitResult struct {
	ExecResult
	Entries   []GitEntry  `json:"entries,omitempty"`
	Branches  []GitBranch `json:"branches,omitempty"`
	Head      *GitHead    `json:"head,omitempty"`
	Remotes   []string    `json:"remotes,omitempty"`
	Operation string      `json:"operation,omitempty"`
	Conflicts []string    `json:"conflicts,omitempty"`
}
type Port struct {
	Port int `json:"port"`
}
type Error struct {
	Code    string
	Detail  string
	Payload json.RawMessage `json:"-"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Detail }
func Payload(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
