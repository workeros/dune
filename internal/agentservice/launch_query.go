package agentservice

import (
	"context"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
)

func (s *Service) QueryLaunch(ctx context.Context, scope agents.Scope, request agents.LaunchQuery) (result api.SubmissionReceipt, err error) {
	key := api.SubmissionKey{SubmissionID: request.SubmissionID, Target: api.SubmissionTarget{OwnerID: scope.OwnerID, RunnerID: request.Binding.RunnerID, FabricID: request.Binding.FabricID, MachineID: request.Binding.MachineID, BindingRevision: request.Binding.Revision}}
	result = api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	defer func() {
		if err != nil {
			err = &api.SubmissionError{Key: key, Cause: err}
		}
	}()
	if err := key.Validate(); err != nil {
		return result, invalid(err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resource, _, err := s.Access.Resource(ctx, scope.Principal, request.Binding.RunnerID, false, "submission.get")
	if err != nil {
		return result, err
	}
	if resource.OwnerID != scope.OwnerID {
		return result, metadata.ErrNotFound
	}
	if resource.Runner.Binding == nil || *resource.Runner.Binding != request.Binding {
		return result, runner.ErrBindingChanged
	}
	connection, closeConnection, err := s.Dial(ctx, scope, request.Binding, "submission.get")
	if err != nil {
		return result, err
	}
	defer closeConnection()
	return connection.QuerySubmission(ctx, key)
}
