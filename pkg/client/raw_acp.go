package client

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aiomni/dune/pkg/api"
)

func (c *Client) RawACPState(ctx context.Context, runtime api.Runtime) (api.RawACPState, error) {
	var state api.RawACPState
	err := c.callACPRead(ctx, "acp.raw.state", struct{}{}, &state, runtime)
	return state, err
}

// ReadRawACP returns exact retained bytes. STREAM_GAP includes the current
// window; choosing to abandon missing bytes is an explicit caller decision.
func (c *Client) ReadRawACP(ctx context.Context, runtime api.Runtime, request api.RawACPRead) (api.RawACPOutput, error) {
	var output api.RawACPOutput
	err := c.callACPRead(ctx, "acp.raw.read", request, &output, runtime)
	var failure *api.Error
	if errors.As(err, &failure) && failure.Code == "STREAM_GAP" {
		var observed api.RawACPOutput
		if json.Unmarshal(failure.Payload, &observed) == nil && observed.StreamID == request.StreamID && observed.Channel == request.Channel && observed.Offset == request.Offset {
			output = observed
		}
	}
	return output, err
}

// TakeRawACP changes only future input admission. Already accepted messages
// still finish in their original order, even if this call loses its response.
func (c *Client) TakeRawACP(ctx context.Context, key api.SubmissionKey, request api.RawACPTake) (api.SubmissionReceipt, error) {
	return c.Submit(ctx, api.SubmissionRequest{SubmissionKey: key, Operation: "acp.raw.take", Payload: api.Payload(request)})
}

// WriteRawACP transfers one complete message without retrying. accepted and
// writing are not written; written only confirms delivery to the original pipe.
// QuerySubmission retains this distinction after connector interruption.
func (c *Client) WriteRawACP(ctx context.Context, key api.SubmissionKey, request api.RawACPWrite) (api.SubmissionReceipt, error) {
	return c.Submit(ctx, api.SubmissionRequest{SubmissionKey: key, Operation: "acp.raw.write", Payload: api.Payload(request)})
}
