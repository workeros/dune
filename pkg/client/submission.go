package client

import (
	"context"
	"slices"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// QuerySubmission observes the caller's original key without submitting work,
// selecting a replacement Runtime, or requiring an active Runtime directory
// entry. Even local errors retain the original key for a later authorized read.
func (c *Client) QuerySubmission(parent context.Context, key api.SubmissionKey) (result api.SubmissionReceipt, err error) {
	result = api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	defer func() {
		if err != nil {
			err = &api.SubmissionError{Key: key, Cause: err}
		}
	}()
	if err := key.Validate(); err != nil {
		return result, &api.Error{Code: "INVALID_ARGUMENT", Detail: err.Error()}
	}
	if key.Target.MachineID != c.Binding.Target {
		return result, &api.Error{Code: "STALE_BINDING", Detail: "submission belongs to another machine"}
	}
	if !slices.Contains(c.Binding.Capabilities, "submission.get") {
		return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not advertise submission queries"}
	}
	var runtime *api.Runtime
	if key.Target.RuntimeID != "" {
		runtime = &api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration}
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	var observed api.SubmissionReceipt
	if err := c.CallID(ctx, "submission.get", wire.ID(), key, &observed, runtime); err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, err
	}
	if observed.SubmissionKey != key {
		return result, &api.Error{Code: "INVALID_RESPONSE", Detail: "submission receipt does not match the original query key"}
	}
	switch observed.Admission {
	case api.SubmissionUnknown, api.SubmissionAccepted, api.SubmissionNotAccepted, api.SubmissionExpired:
		return observed, nil
	default:
		return result, &api.Error{Code: "INVALID_RESPONSE", Detail: "invalid submission admission state"}
	}
}
