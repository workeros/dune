package agentservice

import (
	"context"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
)

// UnsentSubmission preserves caller identity for errors in the host boundary
// before a service call can be made. Parsing selectors grants no authority.
func UnsentSubmission(owner, reference, id string, cause error) (api.SubmissionReceipt, error) {
	key := api.SubmissionKey{SubmissionID: id, Target: api.SubmissionTarget{OwnerID: owner}}
	if ref, err := parseAgentRef(reference); err == nil {
		key = submissionKeyFor(owner, ref.Target, id)
	}
	receipt := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	if cause == nil {
		return receipt, nil
	}
	return receipt, &api.SubmissionError{Key: key, Cause: cause}
}

func (s *Service) Submit(ctx context.Context, scope agents.Scope, request agents.SubmissionRequest) (result api.SubmissionReceipt, err error) {
	result, _ = UnsentSubmission(scope.OwnerID, request.AgentRef, request.SubmissionID, nil)
	key := result.SubmissionKey
	defer func() {
		if err != nil {
			err = &api.SubmissionError{Key: key, Cause: err}
		}
	}()
	if err := key.Validate(); err != nil {
		return result, invalid(err.Error())
	}
	ref, err := parseAgentRef(request.AgentRef)
	if err != nil {
		return result, err
	}
	if ref.Target.Runtime.Adapter != "acp" {
		return result, &api.Error{Code: "UNSUPPORTED", Detail: "managed ACP Runtime required"}
	}
	_, connection, closeConnection, err := s.connect(ctx, scope, ref.Target, "acp.action")
	if err != nil {
		return result, err
	}
	defer closeConnection()
	return connection.Submit(ctx, api.SubmissionRequest{SubmissionKey: key, Operation: "acp.action", Payload: api.Payload(request.ACPAction)})
}

func (s *Service) QuerySubmission(ctx context.Context, scope agents.Scope, request agents.SubmissionQuery) (result api.SubmissionReceipt, err error) {
	result, _ = UnsentSubmission(scope.OwnerID, request.AgentRef, request.SubmissionID, nil)
	key := result.SubmissionKey
	defer func() {
		if err != nil {
			err = &api.SubmissionError{Key: key, Cause: err}
		}
	}()
	if err := key.Validate(); err != nil {
		return result, invalid(err.Error())
	}
	ref, err := parseAgentRef(request.AgentRef)
	if err != nil {
		return result, err
	}
	_, connection, closeConnection, err := s.connect(ctx, scope, ref.Target, "submission.get")
	if err != nil {
		return result, err
	}
	defer closeConnection()
	// No Runtime/native-session read: admission outlives both of those selectors.
	return connection.QuerySubmission(ctx, key)
}
