package agents

import (
	"context"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
)

// Scope comes from the embedding application's authenticated context, never
// from tool arguments. Projects organize sessions but do not grant access.
type Scope struct {
	Principal identity.User
	OwnerID   string
}

type ProjectSelection struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

type StartRequest struct {
	Binding          runner.Binding      `json:"binding"`
	Project          *ProjectSelection   `json:"project,omitempty"`
	DirectoryID      string              `json:"directory_id,omitempty"`
	Profile          *profiles.Selection `json:"profile,omitempty"`
	Custom           *api.Profile        `json:"custom,omitempty"`
	WorkingDirectory string              `json:"working_directory,omitempty"`
	Worktree         *WorktreeLocation   `json:"worktree,omitempty"`
}

type WorktreeLocation struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Ref    string `json:"ref,omitempty"`
}

// Summary intentionally contains no commands, environment, credential values or
// Profile contents. A saved record does not imply its Runtime is still alive.
type Summary struct {
	ID               string              `json:"id"`
	Revision         int64               `json:"revision"`
	Binding          runner.Binding      `json:"binding"`
	ProjectID        string              `json:"project_id,omitempty"`
	DirectoryID      string              `json:"directory_id,omitempty"`
	AgentType        string              `json:"agent_type"`
	Adapter          string              `json:"adapter"`
	WorkingDirectory string              `json:"working_directory"`
	SourceProfile    *profiles.Selection `json:"source_profile,omitempty"`
	SessionState
}

func (s Session) Summary() Summary {
	return Summary{ID: s.ID, Revision: s.Revision, Binding: s.Launch.Binding,
		ProjectID: s.Launch.ProjectID, DirectoryID: s.Launch.DirectoryID,
		AgentType: s.Launch.AgentType, Adapter: s.Launch.Profile.Adapter,
		WorkingDirectory: s.Launch.Profile.WorkingDirectory, SourceProfile: s.Launch.SourceProfile,
		SessionState: s.SessionState}
}

// A partial result is meaningful even with an error: a confirmed worktree or
// Runtime must not be silently discarded and recreated by the caller.
type LaunchResult struct {
	Session  *Summary      `json:"session,omitempty"`
	Runtime  *api.Runtime  `json:"runtime,omitempty"`
	Worktree *api.Worktree `json:"worktree,omitempty"`
}

type Launcher interface {
	Start(context.Context, Scope, StartRequest) (LaunchResult, error)
}

// EnvironmentResolver applies the host's existing Environment Profile defaults
// on a new launch only. The resolved values are frozen in its launch snapshot;
// resume must not call this resolver or merge newer defaults.
type EnvironmentResolver func(context.Context, Scope, runner.Binding, map[string]string) (map[string]string, error)
