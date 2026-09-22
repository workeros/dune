package fabricd

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

// submitACP is the first admitted execution path. Existing protocol/controller
// code remains the sole owner of queue order and ACP semantics; admission only
// adds a durable, caller-addressable boundary before that queue can advance.
func (d *Engine) submitACP(s *executionStream, message *pb.Message, machine string) {
	var request api.SubmissionRequest
	if err := wire.Decode(message, &request); err != nil || request.SubmissionKey.Validate() != nil {
		s.Fail("INVALID_ARGUMENT", errors.New("complete caller-owned submission identity required"))
		return
	}
	key := request.SubmissionKey
	if !matchesSubmissionTarget(key.Target, message, machine) {
		s.Fail("STALE_BINDING", errors.New("submission differs from the routed target"))
		return
	}
	var action api.ACPAction
	if request.Operation != "acp.action" || json.Unmarshal(request.Payload, &action) != nil || (action.Action != "new" && action.Action != "load" && action.Action != "list" && action.Action != "prompt" && action.Action != "permission" && action.Action != "elicitation" && action.Action != "cancel") {
		s.Fail("UNSUPPORTED", errors.New("submission.acp requires a managed ACP action"))
		return
	}
	runtime, err := d.lookup(message)
	if err != nil {
		s.Fail("STALE_RUNTIME", err)
		return
	}
	if runtime.target != (api.SubmissionTarget{}) && runtime.target != key.Target {
		s.Fail("STALE_BINDING", errors.New("submission differs from the original Runtime binding"))
		return
	}
	if runtime.acp == nil || d.registry == nil {
		s.Fail("UNSUPPORTED", errors.New("managed ACP admission is unavailable"))
		return
	}
	if s.Send(&pb.Message{Kind: "accepted", RequestId: message.RequestId}) != nil {
		return
	}
	receipt, err := runtime.acp.submit(s.ctx, d.registry, key, action, "acp:"+runtime.inc)
	if err != nil {
		failSubmission(s, receipt, err)
		return
	}
	d.recordLifecycle("admission_receipt", runtime, receipt.OperationRef, "", 0)
	_ = s.Send(&pb.Message{Kind: "result", RequestId: message.RequestId, Payload: api.Payload(receipt)})
}

func (a *acpController) submit(ctx context.Context, registry *sessionregistry.Registry, key api.SubmissionKey, action api.ACPAction, receiver string) (api.SubmissionReceipt, error) {
	if action.Action == "permission" || action.Action == "elicitation" || action.Action == "cancel" {
		return a.submitControl(ctx, registry, key, action, receiver)
	}
	// Digest the interpreted business parameters, excluding transport identity.
	digest := sessionregistry.Digest("acp.action", api.Payload(action))
	claim, receipt, err := registry.ClaimKey(ctx, key, digest, receiver)
	if err != nil {
		return receipt, err
	}
	if !claim.Acquired() {
		switch receipt.Admission {
		case api.SubmissionAccepted:
			return receipt, nil
		case api.SubmissionNotAccepted:
			return receipt, &api.Error{Code: receipt.ErrorCode, Detail: "original submission was not accepted"}
		default:
			return receipt, &api.Error{Code: "RESULT_UNKNOWN", Detail: "original submission admission is not confirmed"}
		}
	}
	admissionAttempted := false
	a.mu.Lock()
	_, err = a.enqueueWithAdmissionLocked(action, func(operation api.AgentOperation) error {
		admissionAttempted = true
		var admissionErr error
		receipt, admissionErr = registry.Accept(ctx, claim, operation.Ref)
		return admissionErr
	})
	a.mu.Unlock()
	if err != nil && !admissionAttempted {
		var failure *api.Error
		if !errors.As(err, &failure) {
			err = &api.Error{Code: "INVALID_ARGUMENT", Detail: err.Error()}
			failure = err.(*api.Error)
		}
		var rejectionErr error
		receipt, rejectionErr = registry.Reject(ctx, claim, failure.Code)
		if rejectionErr != nil {
			err = &api.Error{Code: "RESULT_UNKNOWN", Detail: "request was not dispatched; admission decision could not be confirmed"}
		}
	}
	return receipt, err
}

// Controls consume only a reservation belonging to their effective target.
// Duplicate lookup precedes lifecycle validation because that target may have
// finished already. A new invalid answer must never consume another slot.
func (a *acpController) submitControl(ctx context.Context, registry *sessionregistry.Registry, key api.SubmissionKey, action api.ACPAction, receiver string) (api.SubmissionReceipt, error) {
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	digest := sessionregistry.Digest("acp.action", api.Payload(action))
	receipt, found, err := registry.Lookup(ctx, key, digest, receiver)
	if err != nil {
		return receipt, err
	}
	if found {
		return receipt, submissionDecisionError(receipt)
	}
	id := action.PermissionID
	if action.Action == "elicitation" {
		id = action.ElicitationID
	}
	if action.Action == "cancel" {
		id = action.OperationRef
	}
	if api.ValidateSubmissionID(id) != nil {
		return receipt, &api.Error{Code: "INVALID_ARGUMENT", Detail: "control requires an exact permission_id or operation_ref"}
	}
	admitted := false
	_, err = a.control(action, func() error {
		claim, existing, claimErr := registry.ClaimControl(ctx, key, digest, receiver, action.Action, id)
		receipt = existing
		if claimErr != nil {
			return claimErr
		}
		if !claim.Acquired() {
			return &api.Error{Code: "RESULT_UNKNOWN", Detail: "control key was concurrently claimed; query its original receipt"}
		}
		receipt, claimErr = registry.Accept(ctx, claim, wire.ID())
		admitted = claimErr == nil
		return claimErr
	})
	if !admitted {
		var failure *api.Error
		if err != nil && !errors.As(err, &failure) {
			err = &api.Error{Code: "INVALID_ARGUMENT", Detail: err.Error()}
		}
		return receipt, err
	}
	// The host finishes admitted writes even after the submitting stream closes.
	// A pipe failure is not a rejection and never grants permission to replay.
	stage, code := "written", ""
	if err != nil {
		stage, code = "input_unrecoverable", "RESULT_UNKNOWN"
	}
	progressCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	confirmed, progressErr := registry.Progress(progressCtx, key, receipt.OperationRef, stage, code, nil)
	if progressErr == nil {
		receipt = confirmed
	}
	if err != nil || progressErr != nil {
		return receipt, &api.Error{Code: "RESULT_UNKNOWN", Detail: "control was admitted; its write completion could not be confirmed"}
	}
	return receipt, nil
}

func submissionDecisionError(receipt api.SubmissionReceipt) error {
	switch receipt.Admission {
	case api.SubmissionAccepted:
		return nil
	case api.SubmissionNotAccepted:
		return &api.Error{Code: receipt.ErrorCode, Detail: "original submission was not accepted"}
	case api.SubmissionExpired:
		return &api.Error{Code: "SUBMISSION_EXPIRED", Detail: "original submission receipt expired; do not replay"}
	default:
		return &api.Error{Code: "RESULT_UNKNOWN", Detail: "original submission admission is not confirmed"}
	}
}

func failSubmission(s *executionStream, receipt api.SubmissionReceipt, err error) {
	code, detail := "RESULT_UNKNOWN", "submission admission could not be confirmed"
	var failure *api.Error
	if errors.As(err, &failure) {
		code, detail = failure.Code, failure.Detail
	}
	_ = s.Send(&pb.Message{Kind: "error", Code: code, Detail: detail, Payload: api.Payload(receipt)})
}

func matchesSubmissionTarget(target api.SubmissionTarget, message *pb.Message, machine string) bool {
	return target.MachineID == machine && target.RuntimeID == message.RuntimeId && target.RuntimeIncarnation == message.RuntimeIncarnation && target.RuntimeGeneration == message.RuntimeGeneration
}

// Submission reads bypass the transport result cache. Admission evidence is
// independently bounded and may remain after a Runtime leaves the active map.
func (d *Engine) querySubmission(s *executionStream, message *pb.Message, machine string) {
	var key api.SubmissionKey
	if err := wire.Decode(message, &key); err != nil || key.Validate() != nil {
		s.Fail("INVALID_ARGUMENT", errors.New("complete submission query key required"))
		return
	}
	if !matchesSubmissionTarget(key.Target, message, machine) {
		s.Fail("STALE_BINDING", errors.New("submission query target differs from the routed target"))
		return
	}
	if d.registry == nil {
		s.Fail("SESSION_UNAVAILABLE", errors.New("submission registry is unavailable"))
		return
	}
	select {
	case d.submissionReads <- struct{}{}:
		defer func() { <-d.submissionReads }()
	default:
		s.Fail("RESOURCE_EXHAUSTED", errors.New("submission read concurrency limit reached"))
		return
	}
	if err := s.Send(&pb.Message{Kind: "accepted", RequestId: message.RequestId}); err != nil {
		return
	}
	receipt, err := d.registry.Get(s.ctx, key)
	if err != nil {
		s.Fail("SESSION_UNAVAILABLE", errors.New("submission evidence could not be read"))
		return
	}
	_ = s.Send(&pb.Message{Kind: "result", RequestId: message.RequestId, Payload: api.Payload(receipt)})
}
