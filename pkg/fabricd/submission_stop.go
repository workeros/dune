package fabricd

import (
	"context"
	"errors"
	"time"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

// Stop bypasses ordinary queues/caches and ACP stdin. Its host admits before
// closing the ownership pipe, then completes even if the caller disconnects.
func (d *Engine) submitStop(s *executionStream, message *pb.Message, machine string) {
	var request api.SubmissionRequest
	if wire.Decode(message, &request) != nil || request.SubmissionKey.Validate() != nil || request.Target.RuntimeID == "" || request.Operation != "runtime.stop" || (len(request.Payload) != 0 && string(request.Payload) != "null") {
		s.Fail("INVALID_ARGUMENT", errors.New("stop requires the original Runtime submission key and no payload"))
		return
	}
	key := request.SubmissionKey
	receipt := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	if !matchesSubmissionTarget(key.Target, message, machine) {
		failSubmission(s, receipt, &api.Error{Code: "STALE_BINDING", Detail: "stop differs from the routed target"})
		return
	}
	if d.registry == nil {
		failSubmission(s, receipt, &api.Error{Code: "SESSION_UNAVAILABLE", Detail: "stop admission registry is unavailable"})
		return
	}
	if s.Send(&pb.Message{Kind: "accepted", RequestId: message.RequestId}) != nil {
		return
	}
	digest, receiver := sessionregistry.Digest("runtime.stop", nil), "runtime:"+key.Target.RuntimeIncarnation
	receipt, found, err := d.registry.Lookup(s.ctx, key, digest, receiver)
	if err == nil && !found {
		var r *runtime
		r, err = d.lookup(message)
		if err == nil && r.target != (api.SubmissionTarget{}) && r.target != key.Target {
			err = &api.Error{Code: "STALE_BINDING", Detail: "stop differs from the original Runtime binding"}
		}
		if err == nil {
			receipt, err = d.stopSubmission(s.ctx, r, key, digest, receiver)
		}
	} else if err == nil {
		err = submissionDecisionError(receipt)
	}
	if err != nil {
		failSubmission(s, receipt, err)
		return
	}
	_ = s.Send(&pb.Message{Kind: "result", RequestId: message.RequestId, Payload: api.Payload(receipt)})
}

func (d *Engine) stopSubmission(ctx context.Context, r *runtime, key api.SubmissionKey, digest [32]byte, receiver string) (api.SubmissionReceipt, error) {
	acquired, receipt, err := d.registry.AcceptStop(ctx, key, digest, receiver, wire.ID())
	if err != nil || !acquired {
		if err == nil {
			err = submissionDecisionError(receipt)
		}
		return receipt, err
	}
	// From here onward the accepted operation belongs to its host. Cancellation
	// affects only delivery of the response, never the original stop itself.
	d.recordLifecycle("stop_accepted", r, receipt.OperationRef, "", 0)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err = d.stop(r); err == nil {
		err = r.waitStop(finishCtx)
	}
	stage, code := "stopped", ""
	if err != nil {
		stage, code = "stopping", "RESULT_UNKNOWN"
	}
	current := r.info()
	updated, progressErr := d.registry.Progress(finishCtx, key, receipt.OperationRef, stage, code, &current)
	if progressErr == nil {
		receipt = updated
	}
	if err != nil || progressErr != nil {
		return receipt, &api.Error{Code: "RESULT_UNKNOWN", Detail: "stop was admitted; process-group exit or its durable result is unconfirmed"}
	}
	return receipt, nil
}
