package fabricd

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

const maxACPPending = 32

type acpQueuedAction struct {
	request   api.ACPAction
	ref       string
	rpcID     string
	responded bool
	reconnect bool
}

// Called with the controller lock: validation, admission and order are shared
// by every Web, IM and MCP caller, regardless of the host Pod they reached.
func (a *acpController) enqueueLocked(req api.ACPAction) (api.AgentOperation, error) {
	if !a.state.Ready {
		return api.AgentOperation{}, fmt.Errorf("ACP is not ready")
	}
	if a.requireMCP && a.state.MCPTransport == "" {
		return api.AgentOperation{}, &api.Error{Code: "AGENT_NOT_READY", Detail: "Agent MCP configuration has not been confirmed"}
	}
	if (req.Action == "list" && !a.state.CanList) || (req.Action == "load" && !a.state.CanLoad) {
		return api.AgentOperation{}, &api.Error{Code: "UNSUPPORTED", Detail: "Agent did not advertise session/" + req.Action}
	}
	if req.Cwd == "" {
		req.Cwd = a.state.Cwd
	}
	if !filepath.IsAbs(req.Cwd) || len(req.Cwd) > 4096 || strings.ContainsFunc(req.Cwd, unicode.IsControl) || len(req.SessionID) > 4096 || strings.ContainsFunc(req.SessionID, unicode.IsControl) || len(req.Cursor) > 8192 {
		return api.AgentOperation{}, fmt.Errorf("invalid ACP session parameters")
	}
	switch req.Action {
	case "new", "list":
	case "load":
		if req.SessionID == "" {
			return api.AgentOperation{}, fmt.Errorf("session ID required")
		}
	case "prompt":
		if err := a.checkPromptConversationLocked(req); err != nil {
			return api.AgentOperation{}, err
		}
		if req.SessionID == "" {
			req.SessionID = a.state.SessionID
		}
		if req.SessionID == "" || req.Text == "" || len(req.Text) > 64*1024 {
			return api.AgentOperation{}, fmt.Errorf("create/load a session and supply 1..65536 bytes of text")
		}
		if req.SessionID != a.state.SessionID || req.Cwd != a.state.Cwd {
			return api.AgentOperation{}, &api.Error{Code: "STALE_SESSION", Detail: "native ACP session changed"}
		}
	default:
		return api.AgentOperation{}, fmt.Errorf("unsupported ACP action")
	}
	// Only the immediately preceding operation may share an in-flight load.
	// MCP configuration cannot change while operations are queued/running.
	previous := a.active
	if len(a.queue) > 0 {
		previous = a.queue[len(a.queue)-1]
	}
	if req.Action == "load" && previous != nil && previous.request.Action == "load" && previous.request.SessionID == req.SessionID && previous.request.Cwd == req.Cwd {
		a.operations.mu.Lock()
		status := a.operations.records[previous.ref].status
		a.operations.mu.Unlock()
		return status, nil
	}
	if len(a.queue) >= maxACPPending {
		return api.AgentOperation{}, &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "ACP pending queue is full"}
	}
	status, err := a.operations.create()
	if err != nil {
		return status, err
	}
	a.queue = append(a.queue, &acpQueuedAction{request: req, ref: status.Ref})
	if a.active == nil && !a.controlling {
		a.startNextLocked()
		status.State = "running"
	} else {
		a.state.Pending = len(a.queue)
		a.publishOperation(status)
		a.publishLocked()
	}
	return status, nil
}

func (a *acpController) publishOperation(status api.AgentOperation) {
	if status.Ref != "" {
		a.r.emit(&pb.Message{Kind: "agent_operation", Payload: api.Payload(status)})
	}
}

func (a *acpController) startNextLocked() {
	for len(a.queue) > 0 {
		operation := a.queue[0]
		a.queue[0] = nil
		a.queue = a.queue[1:]
		req := operation.request
		if req.Action == "prompt" {
			if err := a.checkPromptConversationLocked(req); err != nil {
				a.operations.mu.Lock()
				a.operations.records[operation.ref].status.ErrorCode = "CONVERSATION_CHANGED"
				a.operations.mu.Unlock()
				a.publishOperation(a.operations.set(operation.ref, "failed", "", err.Error()))
				continue
			}
		}
		if req.Action == "prompt" && (req.SessionID != a.state.SessionID || req.Cwd != a.state.Cwd) {
			a.publishOperation(a.operations.set(operation.ref, "failed", "", "native ACP session changed before execution"))
			continue
		}
		a.active = operation
		a.state.OperationRef, a.state.Pending = operation.ref, len(a.queue)
		a.state.Busy, a.state.Error, a.state.StopReason = req.Action, "", ""
		params := map[string]any{}
		switch req.Action {
		case "new", "load":
			operation.reconnect = a.openedOnce
			a.openedOnce = true
			a.reconnecting = operation.reconnect
			a.conversation.begin(req)
			a.operations.mu.Lock()
			a.operations.records[operation.ref].status.ConversationID = a.conversation.describe().ID
			a.operations.mu.Unlock()
			servers := a.mcpServers
			if servers == nil {
				servers = []any{}
			}
			params = map[string]any{"cwd": req.Cwd, "mcpServers": servers}
			if req.Action == "load" {
				params["sessionId"] = req.SessionID
			}
			a.state.SessionID, a.state.Cwd = req.SessionID, req.Cwd
		case "list":
			params["cwd"] = req.Cwd
			if req.Cursor != "" {
				params["cursor"] = req.Cursor
			}
			a.state.List = nil
		case "prompt":
			params = map[string]any{"sessionId": req.SessionID, "prompt": []any{map[string]string{"type": "text", "text": req.Text}}}
		}
		a.publishOperation(a.operations.set(operation.ref, "running", "", ""))
		a.publishLocked()
		if req.Action == "new" || req.Action == "load" {
			a.r.emit(&pb.Message{Kind: "acp_reset"})
		}
		if req.Action == "prompt" {
			a.conversation.startTurn(operation.ref, string(a.redactCredential([]byte(req.Text))))
			a.r.emit(&pb.Message{Kind: "acp_update", Payload: api.Payload(map[string]any{"sessionId": req.SessionID, "update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": req.Text}}})})
		}
		go a.runOperation(operation, params)
		return
	}
	a.state.Pending = 0
}

func (a *acpController) runOperation(operation *acpQueuedAction, params map[string]any) {
	if operation.reconnect {
		if err := a.renewConnection(); err != nil {
			a.mu.Lock()
			a.state.Ready, a.reconnecting = false, false
			a.settleOperationLocked(operation, nil, err)
			a.closedLocked()
			a.mu.Unlock()
			a.r.stop()
			a.r.finish(-1)
			return
		}
	}
	timeout := 60 * time.Second
	if operation.request.Action == "prompt" {
		timeout = 0
	}
	result, err := a.rpc("session/"+operation.request.Action, params, timeout, operation)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settleOperationLocked(operation, result, err)
}

// receive calls this before reading the next protocol line or publishing exit.
// The RPC goroutine calls it only for transport failures/timeouts; the active
// identity makes a duplicate completion harmless.
func (a *acpController) settleOperationLocked(operation *acpQueuedAction, result json.RawMessage, err error) {
	if a.active != operation {
		return
	}
	state, reason, detail := "completed", "", ""
	if err == nil {
		reason, err = a.applyResultLocked(operation.request.Action, result)
	}
	if err != nil {
		state = "failed"
		var failure *api.Error
		if errors.As(err, &failure) && failure.Code == "RESULT_UNKNOWN" {
			state = "unknown"
		}
		detail = err.Error()
		a.state.Error = detail
		if operation.request.Action == "new" || operation.request.Action == "load" {
			a.state.SessionID = ""
		}
	} else if reason == "cancelled" || reason == "canceled" {
		state = "cancelled"
	}
	if state == "unknown" {
		a.operations.markIncomplete(operation.ref)
	}
	if err == nil && (operation.request.Action == "new" || operation.request.Action == "load") {
		native := a.confirmSessionLocked()
		a.operations.mu.Lock()
		if record := a.operations.records[operation.ref]; record != nil {
			record.status.NativeSession = native
		}
		a.operations.mu.Unlock()
	}
	status := api.AgentOperation{Ref: operation.ref, State: state, StopReason: reason, Error: detail}
	if operation.request.Action == "new" || operation.request.Action == "load" {
		outcome := "succeeded"
		var failure *api.ACPFailure
		if err != nil {
			outcome = state
			code := "ACP_OPEN_FAILED"
			var apiFailure *api.Error
			if errors.As(err, &apiFailure) {
				code = apiFailure.Code
			}
			failure = &api.ACPFailure{Code: code, Detail: detail}
		}
		a.conversation.opened(a.state.SessionID, a.state.Cwd, outcome, failure)
	}
	if operation.request.Action == "prompt" {
		a.conversation.finishTurn(status)
	}
	a.publishOperation(a.operations.set(operation.ref, state, reason, detail))
	a.active = nil
	a.state.Busy, a.state.OperationRef = "", ""
	a.permissions = map[string]acpPermission{}
	if state == "unknown" {
		// An unconfirmed boundary cannot safely release the next queued prompt.
		a.cancelPendingLocked("previous ACP outcome is unknown; request was not sent")
		a.state.Ready = false
		a.r.stop()
	}
	a.publishLocked()
	if a.state.Ready && !a.controlling {
		a.startNextLocked()
	}
}

func (a *acpController) applyResultLocked(action string, result json.RawMessage) (string, error) {
	switch action {
	case "new":
		var value struct {
			ID string `json:"sessionId"`
		}
		if json.Unmarshal(result, &value) != nil || strings.TrimSpace(value.ID) == "" || len(value.ID) > 4096 || strings.ContainsFunc(value.ID, unicode.IsControl) {
			return "", &api.Error{Code: "RESULT_UNKNOWN", Detail: "Agent returned invalid sessionId"}
		}
		a.state.SessionID = value.ID
	case "load":
		var value map[string]json.RawMessage
		if json.Unmarshal(result, &value) != nil || value == nil {
			return "", &api.Error{Code: "RESULT_UNKNOWN", Detail: "Agent returned invalid load result"}
		}
	case "list":
		var value struct {
			Sessions []json.RawMessage `json:"sessions"`
		}
		if json.Unmarshal(result, &value) != nil || value.Sessions == nil {
			return "", fmt.Errorf("Agent returned invalid session list")
		}
		a.state.List = result
	case "prompt":
		var value struct {
			Reason string `json:"stopReason"`
		}
		if json.Unmarshal(result, &value) != nil || value.Reason == "" || len(value.Reason) > 256 {
			return "", &api.Error{Code: "RESULT_UNKNOWN", Detail: "Agent returned invalid prompt result"}
		}
		a.state.StopReason = value.Reason
		return value.Reason, nil
	}
	return "", nil
}

// The controller owns the confirmation sequence. Published values are never
// mutated, so Runtime lists and old operation results can safely share them.
func (a *acpController) confirmSessionLocked() *api.NativeSession {
	var agent struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(a.state.Agent, &agent)
	if len(agent.Version) > 256 || strings.ContainsFunc(agent.Version, unicode.IsControl) {
		agent.Version = ""
	}
	a.nativeSequence++
	native := &api.NativeSession{ID: a.state.SessionID, Cwd: a.state.Cwd,
		Sequence: a.nativeSequence, Source: "acp-response", AgentVersion: agent.Version,
		ResumeSupported: a.state.CanLoad}
	a.r.mu.Lock()
	a.r.nativeSession = native
	a.r.mu.Unlock()
	return native
}

func (a *acpController) cancelPendingLocked(detail string) {
	for _, operation := range a.queue {
		a.publishOperation(a.operations.set(operation.ref, "cancelled", "", detail))
	}
	a.queue, a.state.Pending = nil, 0
}

func (a *acpController) closeQueueLocked() {
	if a.active != nil {
		a.operations.markIncomplete(a.active.ref)
		status := a.operations.set(a.active.ref, "unknown", "", "Agent exited before operation completion; request was not replayed")
		if a.active.request.Action == "prompt" {
			a.conversation.finishTurn(status)
		}
		a.publishOperation(status)
		a.active = nil
	}
	a.cancelPendingLocked("Agent exited; queued request was not sent")
	a.state.OperationRef = ""
}

func (a *acpController) recordUpdate(params json.RawMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil || a.active.request.Action != "prompt" {
		return
	}
	var envelope struct {
		SessionID string `json:"sessionId"`
	}
	if a.active.responded || a.active.rpcID == "" || json.Unmarshal(params, &envelope) != nil || envelope.SessionID != a.active.request.SessionID {
		a.operations.markIncomplete(a.active.ref)
		return
	}
	a.operations.append(a.active.ref, params)
}

func (a *acpController) markOutputIncompleteLocked() {
	a.conversation.mutate(func(m *conversationModel) {
		m.invalidatesAll = true
		m.description.ContentOmitted = true
		m.description.ContextIncomplete = true
	})
	if a.active != nil {
		a.operations.markIncomplete(a.active.ref)
	}
}

// A cancel/permission response targets the current operation. If its prompt RPC
// finishes during this write, defer the next prompt until the control is sent.
func (a *acpController) finishControl(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.controlling = false
	if err != nil {
		a.state.Ready = false
		a.state.Error = "ACP control write outcome unknown: " + err.Error()
		a.cancelPendingLocked("control outcome unknown; queued request was not sent")
		a.r.stop()
		a.publishLocked()
	} else if a.active == nil && a.state.Ready {
		a.startNextLocked()
	}
}

func (a *acpController) checkPromptConversationLocked(req api.ACPAction) error {
	if req.ExpectedConversationID == "" || len(req.ExpectedConversationID) > 128 {
		return conversationArgument("expected_conversation_id is required for managed ACP prompts")
	}
	current := a.conversation.describe()
	if current == nil || current.ID != req.ExpectedConversationID {
		return &api.Error{Code: "CONVERSATION_CHANGED", Detail: "observed conversation was replaced; request was not sent"}
	}
	return nil
}
