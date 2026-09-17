package agentservice

import (
	"context"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/workbench"
)

// VerifyCaller checks the current Runner binding and exact live Runtime. The
// saved MCP credential supplies the target; no native session ID is inferred
// from a model argument or an untrusted process observation.
func (s *Service) VerifyCaller(ctx context.Context, scope agents.Scope, target workbench.AgentTarget) error {
	_, connection, closeConnection, err := s.connect(ctx, scope, target, "runtime.get")
	if err != nil {
		return err
	}
	defer closeConnection()
	runtime, err := connection.Get(ctx, runtimeFor(target))
	if err != nil {
		return err
	}
	if runtime.State != "running" || runtime.Adapter != target.Runtime.Adapter {
		return &api.Error{Code: "CALLER_EXITED", Detail: "calling Agent is no longer running"}
	}
	return nil
}
