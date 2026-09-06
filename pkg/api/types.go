// Package api defines the JSON payload schemas carried by Dune protobuf messages.
package api

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const Version = "dune-mvp/1"

type Hello struct {
	Version string `json:"version"`
	Role    string `json:"role"`
}
type Binding struct {
	Version      string         `json:"version"`
	Target       string         `json:"target"`
	Incarnation  string         `json:"incarnation"`
	Generation   uint64         `json:"generation"`
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
	if p.Version != 1 || p.Kind != "agent" || (p.Adapter != "pty" && p.Adapter != "acp") {
		return fmt.Errorf("version: 1, kind: agent and adapter: pty/acp required")
	}
	if p.ManagedACP && p.Adapter != "acp" {
		return fmt.Errorf("managed_acp requires ACP adapter")
	}
	if p.HistoryLines < 0 || p.HistoryLines > 200000 || (p.Adapter != "pty" && p.HistoryLines != 0) {
		return fmt.Errorf("history_lines must be 0 (default) or 1..200000, PTY only")
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
	for _, c := range append(p.Setup.Steps, p.Start) {
		if _, e := c.Args(); e != nil {
			return e
		}
	}
	return nil
}

type Exec struct {
	Command
	WorkingDirectory string            `json:"working_directory"`
	Env              map[string]string `json:"env,omitempty"`
}
type ExecResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	TimedOut  bool   `json:"timed_out"`
	Truncated bool   `json:"truncated"`
}
type Runtime struct {
	Title            string `json:"title,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
	ID               string `json:"id"`
	Incarnation      string `json:"incarnation"`
	Generation       uint64 `json:"generation"`
	Adapter          string `json:"adapter"`
	State            string `json:"state"`
	ExitCode         *int   `json:"exit_code,omitempty"`
}
type Attach struct {
	Observe bool `json:"observe"`
}

// AgentConfig is stored only on the developer machine. Saving it validates the
// command shape, not whether the executable is installed or logged in.
type AgentConfig struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env"`
	Adapter      string            `json:"adapter"`
	HistoryLines int               `json:"history_lines,omitempty"`
}
type AgentConfigRequest struct {
	Action string       `json:"action"`
	ID     string       `json:"id,omitempty"`
	Config *AgentConfig `json:"config,omitempty"`
}

func (a AgentConfig) Profile(cwd string) Profile {
	return Profile{Version: 1, Kind: "agent", WorkingDirectory: cwd, Env: a.Env, Adapter: a.Adapter,
		HistoryLines: a.HistoryLines, Start: Command{Argv: append([]string{a.Command}, a.Args...)}}
}

type Resize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}
type File struct {
	Action      string `json:"action"`
	Path        string `json:"path"`
	Destination string `json:"destination,omitempty"`
	Data        []byte `json:"data,omitempty"`
	Offset      int64  `json:"offset,omitempty"`
	Length      int    `json:"length,omitempty"`
	Overwrite   bool   `json:"overwrite,omitempty"`
	Recursive   bool   `json:"recursive,omitempty"`
}
type FileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"is_dir"`
}
type Upload struct {
	Action      string `json:"action"`
	ID          string `json:"id,omitempty"`
	Path        string `json:"path,omitempty"`
	Size        int64  `json:"size,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Offset      int64  `json:"offset,omitempty"`
	Data        []byte `json:"data,omitempty"`
	ChunkSHA256 string `json:"chunk_sha256,omitempty"`
	Overwrite   bool   `json:"overwrite,omitempty"`
	TTLSeconds  int    `json:"ttl_seconds,omitempty"`
}
type UploadState struct {
	ID          string `json:"id"`
	Incarnation string `json:"incarnation"`
	Offset      int64  `json:"offset"`
	Size        int64  `json:"size"`
	ExpiresAt   string `json:"expires_at"`
	Committed   bool   `json:"committed"`
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
type GitResult struct {
	ExecResult
	Entries   []GitEntry `json:"entries,omitempty"`
	Conflicts []string   `json:"conflicts,omitempty"`
}
type Port struct {
	Port int `json:"port"`
}
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string { return e.Code + ": " + e.Detail }
func Payload(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
