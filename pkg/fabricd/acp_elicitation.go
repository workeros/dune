package fabricd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/google/jsonschema-go/jsonschema"
)

type elicitationParams struct {
	SessionID     string          `json:"sessionId"`
	RequestID     json.RawMessage `json:"requestId"`
	ToolCallID    string          `json:"toolCallId"`
	Mode          string          `json:"mode"`
	Message       string          `json:"message"`
	Schema        json.RawMessage `json:"requestedSchema"`
	URL           string          `json:"url"`
	ElicitationID string          `json:"elicitationId"`
}

type acpElicitation struct {
	api.ACPElicitation
	acceptedURL bool
	rpcID       json.RawMessage
	params      elicitationParams
	requestID   string
	state       string
	schema      *jsonschema.Resolved
}

func (a *acpController) receiveElicitation(rpcID, raw json.RawMessage) {
	a.mu.Lock()
	if a.reconnecting {
		a.mu.Unlock()
		return
	}
	var params elicitationParams
	valid := len(raw) <= 128*1024 && json.Unmarshal(raw, &params) == nil && params.Message != ""
	requestID := ""
	if params.SessionID != "" {
		valid = valid && len(params.RequestID) == 0 && params.SessionID == a.state.SessionID
	} else {
		valid = valid && params.ToolCallID == "" && json.Unmarshal(params.RequestID, &requestID) == nil && a.pending[requestID] != nil && a.methods[requestID] != "session/prompt"
	}
	bytes := len(raw)
	for _, pending := range a.elicitations {
		if string(pending.rpcID) == string(rpcID) {
			a.mu.Unlock()
			return
		}
		bytes += len(pending.Params)
		if params.Mode == "url" && pending.params.ElicitationID == params.ElicitationID {
			valid = false
		}
	}
	for _, pending := range a.permissions {
		if string(pending.rpcID) == string(rpcID) {
			a.mu.Unlock()
			return
		}
	}
	valid = valid && len(a.elicitations) < 16 && bytes <= 128*1024
	switch params.Mode {
	case "form":
		var object map[string]any
		valid = valid && json.Unmarshal(params.Schema, &object) == nil && object != nil
	case "url":
		parsed, err := url.Parse(params.URL)
		valid = valid && err == nil && parsed.Hostname() != "" && parsed.User == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && params.ElicitationID != ""
	default:
		valid = false
	}
	if !valid {
		a.mu.Unlock()
		_ = a.send(map[string]any{"jsonrpc": "2.0", "id": rpcID, "error": map[string]any{"code": -32602, "message": "Invalid elicitation mode, scope, URL or capacity"}})
		return
	}
	e := &acpElicitation{ACPElicitation: api.ACPElicitation{ID: wire.ID(), Params: append(json.RawMessage(nil), raw...)}, rpcID: append(json.RawMessage(nil), rpcID...), params: params, requestID: requestID, state: "pending"}
	if conversation := a.conversation.describe(); conversation != nil {
		e.ConversationID = conversation.ID
		if a.active != nil && a.active.request.Action == "prompt" {
			e.TurnID = conversationTurnID(a.active.ref)
		}
	}
	if params.Mode == "form" {
		var err error
		e.schema, err = prepareElicitationSchema(params.Schema)
		if err != nil {
			e.FormError = err.Error()
		}
	}
	if a.reserveControl != nil {
		if err := a.reserveControl("elicitation", e.ID); err != nil {
			a.mu.Unlock()
			_ = a.send(map[string]any{"jsonrpc": "2.0", "id": rpcID, "error": map[string]any{"code": -32000, "message": "Elicitation response capacity unavailable"}})
			return
		}
	}
	if a.elicitations == nil {
		a.elicitations = map[string]*acpElicitation{}
	}
	a.elicitations[e.ID] = e
	a.elicitationRecordLocked(e, "pending", "")
	a.publishLocked()
	a.mu.Unlock()
}

func (a *acpController) pendingElicitationsLocked() []*acpElicitation {
	var result []*acpElicitation
	for _, e := range a.elicitations {
		if e.state == "pending" {
			result = append(result, e)
		}
	}
	return result
}

func (a *acpController) liveElicitationsLocked() []api.ACPElicitation {
	result := make([]api.ACPElicitation, 0)
	for _, e := range a.pendingElicitationsLocked() {
		result = append(result, e.ACPElicitation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (a *acpController) elicitationRecordLocked(e *acpElicitation, state, response string) {
	a.interactionRecordLocked(e.ConversationID, e.TurnID, api.ACPInteractionRecord{ID: e.ID, Kind: "elicitation", State: state, Title: e.params.Message, ToolCallID: e.params.ToolCallID, Response: response})
}

func (a *acpController) endElicitationLocked(e *acpElicitation, state string) {
	e.state = state
	a.elicitationRecordLocked(e, state, "")
	delete(a.elicitations, e.ID)
	if a.releaseControl != nil {
		a.releaseControl("elicitation", e.ID)
	}
}

func (a *acpController) clearElicitationsLocked(state string) {
	for _, e := range a.elicitations {
		a.endElicitationLocked(e, state)
	}
}

func (a *acpController) expireRequestElicitationsLocked(requestID string) {
	for _, e := range a.elicitations {
		if e.requestID == requestID && e.state == "pending" {
			a.endElicitationLocked(e, "expired")
		}
	}
}

// Called with mu and controlMu held; releases mu on every path. Admission
// consumes exactly this request before writing; an uncertain write is terminal.
func (a *acpController) respondElicitationLocked(action api.ACPAction, admit func() error) (any, error) {
	e := a.elicitations[action.ElicitationID]
	if e == nil || e.state != "pending" {
		a.mu.Unlock()
		return nil, fmt.Errorf("elicitation expired or already answered")
	}
	response := action.ElicitationResponse
	if err := validateElicitationResponse(e, response); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if admit != nil {
		if err := admit(); err != nil {
			a.mu.Unlock()
			return nil, err
		}
	}
	a.markQuestionActivityLocked(e.requestID)
	e.state = "submitting"
	e.acceptedURL = e.params.Mode == "url" && response.Action == "accept"
	a.elicitationRecordLocked(e, "submitting", "")
	a.controlling = true
	a.publishLocked()
	a.mu.Unlock()
	wireResponse := *response
	if e.params.Mode == "url" {
		wireResponse.Content = nil
	}
	err := a.sendElicitationResponse(e.rpcID, wireResponse)
	a.mu.Lock()
	if a.elicitations[e.ID] == e {
		state := "responded"
		if response.Action == "decline" {
			state = "declined"
		}
		if response.Action == "cancel" {
			state = "cancelled"
		}
		if e.params.Mode == "url" && response.Action == "accept" {
			state = "awaiting_completion"
		}
		if e.state == "completed" {
			state = "completed"
		}
		if err != nil {
			state = "unknown"
		}
		e.state = state
		a.elicitationRecordLocked(e, state, "")
		if state != "awaiting_completion" {
			delete(a.elicitations, e.ID)
		}
	}
	a.mu.Unlock()
	a.finishControl(err)
	if err != nil {
		a.r.stop()
		return nil, fmt.Errorf("elicitation write outcome unknown: %w", err)
	}
	return map[string]bool{"accepted": true}, nil
}

func validateElicitationResponse(e *acpElicitation, response *api.ACPElicitationResponse) error {
	if response == nil {
		return fmt.Errorf("elicitation_response is required")
	}
	if response.Action == "decline" || response.Action == "cancel" {
		return nil
	}
	if response.Action != "accept" {
		return fmt.Errorf("invalid elicitation action")
	}
	if e.params.Mode == "url" {
		return nil
	}
	if e.schema == nil {
		return fmt.Errorf("unsupported form schema: %s", e.FormError)
	}
	var values map[string]any
	if len(response.Content) > 64*1024 || json.Unmarshal(response.Content, &values) != nil || values == nil {
		return fmt.Errorf("form content must be an object within 64 KiB")
	}
	if err := e.schema.Validate(values); err != nil {
		return fmt.Errorf("form values do not match requested schema")
	}
	return validateElicitationFormats(e.params.Schema, values)
}

func (a *acpController) sendElicitationResponse(id json.RawMessage, response api.ACPElicitationResponse) error {
	if response.Action != "accept" {
		response.Content = nil
	}
	return a.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": response})
}

func (a *acpController) completeElicitation(raw json.RawMessage) {
	var params struct {
		ID string `json:"elicitationId"`
	}
	if json.Unmarshal(raw, &params) != nil || params.ID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reconnecting {
		return
	}
	for _, e := range a.elicitations {
		if !e.acceptedURL || e.params.ElicitationID != params.ID || (e.state != "awaiting_completion" && e.state != "submitting") {
			continue
		}
		// A fast completion may arrive before the response writer returns.
		a.markQuestionActivityLocked(e.requestID)
		previous := e.state
		e.state = "completed"
		a.elicitationRecordLocked(e, "completed", "")
		if previous != "submitting" {
			delete(a.elicitations, e.ID)
		}
		a.publishLocked()
		return
	}
}

// Form answers are sent to the Agent, but not copied into diagnostics/history.
func redactElicitationResponse(raw []byte) []byte {
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) != nil {
		return raw
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		return raw
	}
	if action, ok := result["action"].(string); ok && (action == "accept" || action == "decline" || action == "cancel") {
		if _, exists := result["content"]; exists {
			result["content"] = "[user response omitted from diagnostics]"
		}
		return api.Payload(envelope)
	}
	return raw
}

// Waiting for a person does not spend the Agent's lifecycle RPC timeout.
// Once they respond, the Agent gets a fresh bounded interval to finish.
func (a *acpController) markQuestionActivityLocked(requestID string) {
	if requestID == "" || a.pending[requestID] == nil {
		return
	}
	if a.questionActivity == nil {
		a.questionActivity = map[string]time.Time{}
	}
	a.questionActivity[requestID] = time.Now()
}

func (a *acpController) elicitationWaitRemaining(requestID string, timeout time.Duration) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.elicitations {
		if e.requestID == requestID {
			return timeout
		}
	}
	if activity, exists := a.questionActivity[requestID]; exists {
		return time.Until(activity.Add(timeout))
	}
	return 0
}
