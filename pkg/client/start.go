package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// Start never creates or replaces a submission ID. The caller must preserve
// request.SubmissionKey before calling, including across a lost first response.
func (c *Client) Start(ctx context.Context, request api.StartRequest) (result api.StartResult, stream *Stream, err error) {
	key := request.SubmissionKey
	result.SubmissionReceipt = api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	defer func() {
		if err != nil {
			var failure *api.Error
			var observed api.StartResult
			if errors.As(err, &failure) && json.Unmarshal(failure.Payload, &observed) == nil && observed.SubmissionKey == key && validSubmissionReceipt(observed.SubmissionReceipt) {
				result = observed
			}
			err = &api.SubmissionError{Key: key, Cause: err}
		}
	}()
	if key.Validate() != nil || key.Target.RuntimeID != "" || request.Profile.Kind != "agent" {
		return result, nil, &api.Error{Code: "INVALID_ARGUMENT", Detail: "caller-owned launch key and Agent Profile required"}
	}
	if !slices.Contains(c.Binding.Capabilities, "profile.start") {
		return result, nil, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not advertise Agent startup"}
	}
	if key.Target.MachineID != c.Binding.Target {
		return result, nil, &api.Error{Code: "STALE_BINDING", Detail: "launch belongs to another machine"}
	}
	s, _, err := c.open(ctx, "profile.start", wire.ID(), request, nil)
	if err != nil {
		return result, nil, err
	}
	for {
		message, receiveErr := s.Recv()
		if receiveErr != nil {
			s.Close()
			return result, nil, receiveErr
		}
		if message.Kind == "progress" {
			continue
		}
		var observed api.StartResult
		if message.Kind != "result" || wire.Decode(message, &observed) != nil || observed.SubmissionKey != key || observed.Admission != api.SubmissionAccepted || observed.Stage != "started" || observed.Runtime == nil {
			s.Close()
			return result, nil, fmt.Errorf("launch did not return a confirmed startup for its original key")
		}
		s.terminal = false
		return observed, s, nil
	}
}
