// Package agents describes application-managed Agent launches and communication.
// Execution and operation queues remain owned by fabricd.
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
	SubmissionID     string              `json:"submission_id"`
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

// A partial result is meaningful even with an error: a confirmed worktree or
// Runtime must not be silently discarded and recreated by the caller.
type LaunchResult struct {
	api.SubmissionKey
	Failure    *api.ProfileFailure    `json:"failure,omitempty"`
	Submission *api.SubmissionReceipt `json:"submission,omitempty"`
	AgentRef   string                 `json:"agent_ref,omitempty"`
	Operation  *Operation             `json:"operation,omitempty"`
	Runtime    *api.Runtime           `json:"runtime,omitempty"`
	Worktree   *api.Worktree          `json:"worktree,omitempty"`
}

type Launcher interface {
	Start(context.Context, Scope, StartRequest) (LaunchResult, error)
	QueryLaunch(context.Context, Scope, LaunchQuery) (api.SubmissionReceipt, error)
}

// LaunchQuery needs only the caller-saved identity. It never resolves another
// Profile revision, starts setup, configures MCP or opens an ACP session.
type LaunchQuery struct {
	SubmissionID string         `json:"submission_id"`
	Binding      runner.Binding `json:"binding"`
}

// EnvironmentResolver applies the host's existing Environment Profile defaults
// before starting the Agent.
type EnvironmentResolver func(context.Context, Scope, runner.Binding, map[string]string) (map[string]string, error)
