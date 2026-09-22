package fabricd

import (
	"encoding/json"

	"github.com/aiomni/dune/pkg/api"
)

func (a *acpController) receiveOperationReplyLocked(operation *acpQueuedAction, result json.RawMessage, err error) {
	if a.active != operation || operation.responded {
		return
	}
	operation.responded = true
	if err != nil || a.state.ProtocolVersion != 2 || operation.request.Action != "prompt" {
		a.settleOperationLocked(operation, result, err)
		return
	}
	var accepted struct {
		MessageID string `json:"messageId"`
	}
	if json.Unmarshal(result, &accepted) != nil || !validNativeID(accepted.MessageID) || !a.conversation.acknowledgeV2Message(operation.ref, accepted.MessageID) {
		a.settleOperationLocked(operation, nil, &api.Error{Code: "RESULT_UNKNOWN", Detail: "Agent did not confirm a distinct inserted user message"})
		return
	}
	// A fast Agent may report completion before the insertion response. Both
	// facts must be confirmed before another queued prompt can be dispatched.
	if operation.idleResult != nil {
		a.settleOperationLocked(operation, operation.idleResult, nil)
	}
}

func (a *acpController) applyV2StateLocked(update map[string]json.RawMessage) {
	if rawString(update, "sessionUpdate") != "state_update" {
		return
	}
	state := rawString(update, "state")
	if state == "" || len(state) > 256 {
		a.markOutputIncompleteLocked()
		return
	}
	a.state.ForegroundState = state
	if state != "idle" {
		if a.active.isForeground() {
			a.active.idleResult = nil
		}
		a.adoptV2ForegroundLocked()
		a.publishLocked()
		return
	}
	if a.active.isForeground() {
		reason := rawString(update, "stopReason")
		if reason == "" || len(reason) > 256 {
			a.state.Error = "ACP idle update did not confirm why foreground work stopped"
			a.markOutputIncompleteLocked()
			a.publishLocked()
			return
		}
		a.active.idleResult = api.Payload(map[string]string{"stopReason": reason})
		if a.active.responded {
			a.settleOperationLocked(a.active, a.active.idleResult, nil)
		}
	}
	a.publishLocked()
}

// Work may already be running when a session is resumed, or start without a
// client prompt. Give that foreground interval its own cancellable identity;
// do not attach it to the last completed prompt or manufacture a new prompt.
func (a *acpController) adoptV2ForegroundLocked() {
	if a.state.ProtocolVersion != 2 || a.active != nil || a.state.ForegroundState == "" || a.state.ForegroundState == "idle" {
		return
	}
	status, err := a.operations.create()
	if err == nil && a.reserveControl != nil {
		err = a.reserveControl("cancel", status.Ref)
	}
	if err != nil {
		if status.Ref != "" {
			a.operations.set(status.Ref, "failed", "", "foreground control reservation unavailable")
		}
		a.state.Ready, a.state.Error = false, "cannot retain ACP foreground control: "+err.Error()
		a.cancelPendingLocked("foreground control unavailable; queued request was not sent")
		a.r.stop()
		return
	}
	a.active = &acpQueuedAction{ref: status.Ref, responded: true, request: api.ACPAction{Action: "foreground", SessionID: a.state.SessionID, Cwd: a.state.Cwd}}
	a.state.Busy, a.state.OperationRef = "prompt", status.Ref
	a.conversation.startTurn(status.Ref, nil)
	a.publishOperation(a.operations.set(status.Ref, "running", "", ""))
}
