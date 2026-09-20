package client

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// Submit transfers one caller-identified operation to its admission owner. It
// never retries a write. The returned receipt is not the business result; use
// QuerySubmission after an interruption and the original operation reference
// to observe execution. Supported operations are advertised separately.
func (c *Client) Submit(parent context.Context, request api.SubmissionRequest) (result api.SubmissionReceipt, err error) {
	key := request.SubmissionKey
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
	if request.Operation != "acp.action" || key.Target.RuntimeID == "" || !slices.Contains(c.Binding.Capabilities, "submission.acp") {
		return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not advertise this submission operation"}
	}
	runtime := api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration}
	var observed api.SubmissionReceipt
	if err := c.CallID(parent, "submission.acp", wire.ID(), request, &observed, &runtime); err != nil {
		var failure *api.Error
		if errors.As(err, &failure) && json.Unmarshal(failure.Payload, &observed) == nil && observed.SubmissionKey == key && validSubmissionReceipt(observed) {
			result = observed
		}
		return result, err
	}
	if observed.SubmissionKey != key || observed.Admission != api.SubmissionAccepted || observed.OperationRef == "" {
		return result, &api.Error{Code: "RESULT_UNKNOWN", Detail: "Agent submission receipt is not a confirmed admission for the original key"}
	}
	return observed, nil
}

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

func validSubmissionReceipt(receipt api.SubmissionReceipt) bool {
	switch receipt.Admission {
	case api.SubmissionAccepted:
		return receipt.OperationRef != ""
	case api.SubmissionNotAccepted:
		return receipt.ErrorCode != ""
	case api.SubmissionUnknown, api.SubmissionExpired:
		return true
	default:
		return false
	}
}
