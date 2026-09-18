package agents

import (
	"context"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

// Agent is an observation of current fabricd state. Ref fixes the
// concrete Runtime and its confirmed native conversation; it grants no access.
type Agent struct {
	Ref     string                `json:"agent_ref"`
	Target  workbench.AgentTarget `json:"target"`
	Runtime api.Runtime           `json:"runtime"`
	Runner  runner.Runner         `json:"runner"`
}

type RunnerAvailability struct {
	Runner runner.Runner `json:"runner"`
	Online bool          `json:"online"`
	Ready  bool          `json:"ready"`
}

type DiscoveryIssue struct {
	RunnerID string `json:"runner_id"`
	Code     string `json:"code"`
}

// Pagination advances across Runners. An unavailable Runner is reported rather
// than making all of the Tenant's other Agents disappear.
type DirectoryPage struct {
	Items      []Agent              `json:"items"`
	Runners    []RunnerAvailability `json:"runners"`
	Issues     []DiscoveryIssue     `json:"issues"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

type Directory interface {
	List(context.Context, Scope, runner.Query) (DirectoryPage, error)
	Get(context.Context, Scope, string) (Agent, error)
}
