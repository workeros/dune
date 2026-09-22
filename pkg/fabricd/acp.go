package fabricd

// ACP scheduling and protocol state live beside the Agent. Operation output is
// bounded in memory; the Agent remains responsible for its native transcript.
import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type acpPermission struct {
	api.ACPPermission
	rpcID json.RawMessage
}
type acpReply struct {
	Result json.RawMessage
	Err    error
}

const (
	acpBrowserUpdateBytes = 512 * 1024
	acpTextChunkBytes     = 256 * 1024
	acpSummaryFieldBytes  = 64 * 1024
)

type acpController struct {
	v2Draft                bool
	elicitationEnabled     bool
	mcpStdio               bool
	inputMu                sync.Mutex
	connection             *process.Process
	openedOnce             bool
	reconnecting           bool
	renewConnection        func() error
	mu                     sync.Mutex
	controlMu              sync.Mutex
	reserveControl         func(string, string) error
	releaseControl         func(string, string)
	controlling            bool
	r                      *runtime
	state                  api.ACPState
	pending                map[string]chan acpReply
	questionActivity       map[string]time.Time
	permissions            map[string]acpPermission
	elicitations           map[string]*acpElicitation
	elicitationCompletions []acpElicitationCompletion
	methods                map[string]string
	done                   chan struct{}
	once                   sync.Once
	queue                  []*acpQueuedAction
	active                 *acpQueuedAction
	operations             *operationLog
	conversation           *conversationSlot
	nativeSequence         int64
	requireMCP             bool
	mcpHTTP                bool
	mcpServers             []any
	mcpSecret              atomic.Pointer[string]
}

func newACPController(r *runtime) *acpController {
	r.updateActivity("working", "acp", "", "")
	if r.conversations == nil {
		r.conversations = newConversationStore()
	}
	a := &acpController{connection: r.p, r: r, operations: r.operationLog(), conversation: r.conversations.register(r.id, r.inc, func(change api.ACPConversationChanged) {
		r.emit(&pb.Message{Kind: "acp_conversation_changed", Payload: api.Payload(change)})
	}), state: api.ACPState{ProtocolVersion: 1, Busy: "initialize", Cwd: r.cwd}, pending: map[string]chan acpReply{}, permissions: map[string]acpPermission{}, elicitations: map[string]*acpElicitation{}, done: make(chan struct{}), methods: map[string]string{}}
	r.conversations.mu.Lock()
	a.conversation.events = r.events
	r.conversations.mu.Unlock()
	a.renewConnection = a.reconnect
	return a
}
func (a *acpController) snapshotLocked() api.ACPState {
	s := a.state
	s.Resources = &api.ACPResourceUsage{Queue: api.CapacityUsage{Used: len(a.queue), Limit: maxACPPending}, Permissions: api.CapacityUsage{Used: len(a.permissions), Limit: 16}, Elicitations: api.CapacityUsage{Used: len(a.elicitations), Limit: 16}, Operations: a.operations.usage()}
	s.Conversation = a.conversation.describe()
	s.Permissions = make([]api.ACPPermission, 0, len(a.permissions))
	for _, p := range a.permissions {
		s.Permissions = append(s.Permissions, p.ACPPermission)
	}
	sort.Slice(s.Permissions, func(i, j int) bool { return s.Permissions[i].ID < s.Permissions[j].ID })
	s.Elicitations = a.liveElicitationsLocked()
	return s
}
func (a *acpController) snapshot() api.ACPState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked()
}
func (a *acpController) publishLocked() {
	a.state.Revision++
	a.publishActivityLocked()
	a.r.emit(&pb.Message{Kind: "acp_state", Payload: api.Payload(a.snapshotLocked())})
}
func (a *acpController) send(v any) error {
	b := api.Payload(v)
	// Publish before writing so a fast Agent response cannot appear before the
	// request in the inspector. A subsequent write error is surfaced separately.
	a.r.emit(&pb.Message{Kind: "acp_stream", Payload: api.Payload(map[string]any{"direction": "input", "message": json.RawMessage(a.redactCredential(redactMCPConfiguration(redactElicitationResponse(b))))})})
	a.r.mu.Lock()
	p := a.r.p
	a.r.mu.Unlock()
	if err := p.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}
func (a *acpController) rpc(method string, params any, timeout time.Duration, operation *acpQueuedAction) (json.RawMessage, error) {
	id := wire.ID()
	ch := make(chan acpReply, 1)
	a.mu.Lock()
	select {
	case <-a.done:
		a.mu.Unlock()
		return nil, &api.Error{Code: "RESULT_UNKNOWN", Detail: "ACP Agent exited before RPC dispatch"}
	default:
	}
	a.pending[id] = ch
	a.methods[id] = method
	if operation != nil {
		operation.rpcID = id
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		delete(a.methods, id)
		delete(a.questionActivity, id)
		a.mu.Unlock()
	}()
	if err := a.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, &api.Error{Code: "RESULT_UNKNOWN", Detail: "ACP write outcome unknown: " + err.Error()}
	}
	var expiry <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		expiry = timer.C
	}
	for {
		select {
		case reply := <-ch:
			return reply.Result, reply.Err
		case <-a.done:
			select {
			case reply := <-ch:
				return reply.Result, reply.Err
			default:
			}
			return nil, &api.Error{Code: "RESULT_UNKNOWN", Detail: "ACP Agent exited before a matching RPC result"}
		case <-expiry:
			if remaining := a.elicitationWaitRemaining(id, timeout); remaining > 0 {
				timer.Reset(remaining)
				continue
			}
			// A timed-out lifecycle call has an unknown result. Do not admit another
			// operation to the same process and accidentally use the wrong session.
			a.r.stop()
			return nil, &api.Error{Code: "RESULT_UNKNOWN", Detail: "ACP " + method + " timed out; process stopped, request was not replayed"}
		}
	}
}
func (a *acpController) closed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closedLocked()
}

func (a *acpController) closedLocked() { a.closedWithExitLocked(nil) }

func (a *acpController) closedWithExitLocked(code *int) {
	a.once.Do(func() { close(a.done) })
	a.closeQueueLocked()
	a.conversation.exitedWithCode(code)
	a.state.Ready = false
	a.state.Busy = ""
	a.clearPermissionsLocked("expired")
	a.clearElicitationsLocked("expired")
	if a.state.Error == "" {
		a.state.Error = "ACP Agent exited"
	}
	a.publishLocked()
}
func (a *acpController) receive(data []byte) {
	// Mask the generated literal before it can enter operation output, permission
	// state or diagnostics. Native writes still use the original configuration.
	data = a.redactCredential(data)
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	// session/update already has its own browser event. The inspector can rebuild
	// the envelope from params; carrying the original RPC here would duplicate
	// large history updates on the browser stream.
	if m.Method == "session/update" && len(m.ID) == 0 {
		a.emitUpdate(m.Params)
		return
	}
	a.r.emit(&pb.Message{Kind: "acp_stream", Payload: api.Payload(map[string]any{"direction": "output", "message": redactMCPConfiguration(data)})})
	if m.Method == "" {
		var id string
		if json.Unmarshal(m.ID, &id) != nil {
			return
		}
		a.mu.Lock()
		if a.reconnecting {
			a.mu.Unlock()
			return
		}
		ch := a.pending[id]
		reply := acpReply{Result: m.Result}
		if m.Error != nil {
			reply.Err = formatACPError(m.Error.Code, m.Error.Message, m.Error.Data)
		}
		if a.active != nil && a.active.rpcID == id {
			a.receiveOperationReplyLocked(a.active, reply.Result, reply.Err)
		}
		if ch != nil {
			a.expireRequestElicitationsLocked(id)
			if a.state.ProtocolVersion != 2 && a.methods[id] != "session/list" {
				a.clearPermissionsLocked("expired")
			}
			a.publishLocked()
		}
		a.mu.Unlock()
		if ch != nil {
			select {
			case ch <- reply:
			default:
			}
		}
		return
	}
	if m.Method == "elicitation/complete" && len(m.ID) == 0 {
		a.completeElicitation(m.Params)
		return
	}
	if len(m.ID) == 0 {
		return
	}
	if m.Method == "elicitation/create" {
		a.receiveElicitation(m.ID, m.Params)
		return
	}
	if m.Method == "session/request_permission" {
		var params struct {
			SessionID string  `json:"sessionId"`
			Title     *string `json:"title"`
			Options   []struct {
				ID string `json:"optionId"`
			} `json:"options"`
		}
		a.mu.Lock()
		// Admission at the reader can precede an explicit open. Recheck under
		// the state lock so an in-flight old permission cannot enter that model.
		if a.reconnecting {
			a.mu.Unlock()
			return
		}
		permissionBytes := len(m.Params)
		for _, p := range a.permissions {
			permissionBytes += len(p.Params)
		}
		conversation := a.conversation.describe()
		valid := conversation != nil && permissionBytes <= 128*1024 && json.Unmarshal(m.Params, &params) == nil && params.SessionID == a.state.SessionID && (a.state.Busy != "" || a.state.ProtocolVersion == 2) && (a.state.ProtocolVersion != 2 || params.Title != nil) && len(params.Options) > 0 && len(params.Options) <= 32 && len(a.permissions) < 16
		for _, p := range a.permissions {
			if string(p.rpcID) == string(m.ID) {
				valid = false
			}
		}
		if valid {
			key := wire.ID()
			if a.reserveControl != nil {
				if err := a.reserveControl("permission", key); err != nil {
					a.mu.Unlock()
					_ = a.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32000, "message": "Dune permission response capacity unavailable"}})
					return
				}
			}
			turnID := ""
			if a.active.isForeground() {
				turnID = conversationTurnID(a.active.ref)
			}
			permission := acpPermission{ACPPermission: api.ACPPermission{ID: key, ConversationID: conversation.ID, TurnID: turnID, Params: append(json.RawMessage(nil), m.Params...)}, rpcID: m.ID}
			a.permissions[key] = permission
			a.permissionRecordLocked(permission, "pending", "")
			a.publishLocked()
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
		// Invalid or stale requests cannot become approvals.
		_ = a.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}})
		return
	}
	// No file-system/terminal capabilities are advertised. Agents that have
	// their own tools can use them; unsupported client methods fail explicitly.
	_ = a.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "Dune does not advertise this client capability"}})
}

func formatACPError(code int, message string, data json.RawMessage) error {
	detail := ""
	if len(data) > 0 && string(data) != "null" {
		data = redactMCPConfiguration(data)
		var structured struct {
			Details string `json:"details"`
		}
		if json.Unmarshal(data, &structured) == nil {
			detail = strings.TrimSpace(structured.Details)
		}
		if detail == "" {
			var text string
			if json.Unmarshal(data, &text) == nil {
				detail = strings.TrimSpace(text)
			} else {
				detail = strings.TrimSpace(string(data))
			}
		}
	}
	if detail == "" || detail == message {
		return fmt.Errorf("ACP %d: %s", code, message)
	}
	return fmt.Errorf("ACP %d: %s: %s", code, message, detail)
}

func (a *acpController) emitUpdate(params json.RawMessage) {
	params = redactMCPConfiguration(params)
	a.recordUpdate(params)
	a.recordConversationUpdate(params)
	if len(params) <= acpBrowserUpdateBytes {
		a.r.emit(&pb.Message{Kind: "acp_update", Payload: params})
		return
	}
	var envelope struct {
		SessionID string         `json:"sessionId"`
		Update    map[string]any `json:"update"`
	}
	if json.Unmarshal(params, &envelope) != nil || envelope.Update == nil {
		a.r.emit(&pb.Message{Kind: "acp_notice", Payload: api.Payload(map[string]any{
			"code":          "MESSAGE_OMITTED",
			"detail":        "A large ACP update could not be compacted; content was omitted and the Runtime continues",
			"message_bytes": len(params),
		})})
		return
	}
	kind, _ := envelope.Update["sessionUpdate"].(string)
	if kind == "" {
		kind, _ = envelope.Update["session_update"].(string)
	}
	if strings.Contains(kind, "message") {
		if content, ok := envelope.Update["content"].(map[string]any); ok {
			if text, ok := content["text"].(string); ok && text != "" {
				for _, part := range splitACPText(text, acpTextChunkBytes) {
					// Rebuild large message chunks from bounded metadata. Copying the
					// original update could attach an unrelated multi-MiB field to every
					// chunk and exceed the fixed transport frame again.
					update := map[string]any{}
					for _, key := range []string{"sessionUpdate", "session_update", "messageId", "message_id", "role"} {
						if value, ok := envelope.Update[key]; ok {
							update[key] = boundedACPField(value, 4096)
						}
					}
					nextContent := map[string]any{}
					if value, ok := content["type"]; ok {
						nextContent["type"] = boundedACPField(value, 4096)
					}
					nextContent["text"] = part
					update["content"] = nextContent
					a.r.emit(&pb.Message{Kind: "acp_update", Payload: api.Payload(map[string]any{"sessionId": envelope.SessionID, "update": update})})
				}
				return
			}
		}
	}

	compact := map[string]any{}
	for _, key := range []string{"sessionUpdate", "session_update", "messageId", "message_id", "toolCallId", "tool_call_id", "title", "kind", "status"} {
		if value, ok := envelope.Update[key]; ok {
			compact[key] = boundedACPField(value, 4096)
		}
	}
	for _, key := range []string{"content", "rawInput", "raw_input", "rawOutput", "raw_output", "_meta", "meta"} {
		if value, ok := envelope.Update[key]; ok {
			compact[key] = boundedACPField(value, acpSummaryFieldBytes)
		}
	}
	compact["duneOmittedBytes"] = len(params)
	a.r.emit(&pb.Message{Kind: "acp_update", Payload: api.Payload(map[string]any{"sessionId": envelope.SessionID, "update": compact})})
}

func boundedACPField(value any, limit int) any {
	b, err := json.Marshal(value)
	if err == nil && len(b) <= limit {
		return value
	}
	size := len(b)
	if err != nil {
		size = 0
	}
	return fmt.Sprintf("[Dune omitted oversized ACP field; original JSON bytes: %d]", size)
}

func splitACPText(text string, limit int) []string {
	parts := make([]string, 0, len(text)/limit+1)
	for len(text) > limit {
		end := limit
		for end > 0 && !utf8.ValidString(text[:end]) {
			end--
		}
		if end == 0 {
			end = limit
		}
		parts = append(parts, text[:end])
		text = text[end:]
	}
	if text != "" {
		parts = append(parts, text)
	}
	return parts
}
func (a *acpController) initialize() {
	if err := a.initializeConnection(); err != nil {
		a.mu.Lock()
		a.state.Error, a.state.Busy = err.Error(), ""
		a.publishLocked()
		a.mu.Unlock()
		a.r.stop()
	}
}
func (a *acpController) initializeConnection() error {
	result, err := a.rpc("initialize", a.initializationParams(), 30*time.Second, nil)
	var negotiated acpNegotiation
	if err == nil {
		negotiated, err = negotiateACP(result, a.v2Draft)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-a.done:
		return &api.Error{Code: "RESULT_UNKNOWN", Detail: "ACP exited during initialization"}
	default:
	}
	if a.state.MCPTransport == "http" && !negotiated.http {
		return &api.Error{Code: "UNSUPPORTED", Detail: "isolated connection no longer supports the configured HTTP MCP transport"}
	}
	if a.openedOnce && a.state.ProtocolVersion != negotiated.version {
		return &api.Error{Code: "UNSUPPORTED", Detail: "isolated connection changed the negotiated ACP version"}
	}
	if a.state.MCPTransport == "stdio" && !negotiated.stdio {
		return &api.Error{Code: "UNSUPPORTED", Detail: "isolated connection no longer supports stdio MCP"}
	}
	a.state.Ready = true
	if a.active == nil {
		a.state.Busy = ""
	}
	a.state.ProtocolVersion = negotiated.version
	a.state.Agent, a.state.CanLoad, a.state.CanResume = negotiated.info, negotiated.load, negotiated.resume
	a.state.PromptCapabilities, a.state.CanList = negotiated.prompt, negotiated.list
	a.mcpHTTP, a.mcpStdio = negotiated.http, negotiated.stdio
	a.publishLocked()
	return nil
}
func (a *acpController) action(req api.ACPAction) (any, error) {
	if req.Action == "permission" || req.Action == "elicitation" || req.Action == "cancel" {
		a.controlMu.Lock()
		defer a.controlMu.Unlock()
		return a.control(req, nil)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enqueueLocked(req)
}

// control runs with controlMu held. Admission is committed under the state
// lock after validating the effective target and before consuming it or writing.
func (a *acpController) control(req api.ACPAction, admit func() error) (any, error) {
	a.mu.Lock()
	select {
	case <-a.done:
		a.mu.Unlock()
		return nil, fmt.Errorf("ACP Agent exited")
	default:
	}
	if req.Action == "elicitation" {
		return a.respondElicitationLocked(req, admit)
	}
	if req.Action == "permission" {
		p, ok := a.permissions[req.PermissionID]
		if !ok {
			a.mu.Unlock()
			return nil, fmt.Errorf("permission expired or already answered")
		}
		var params struct {
			Options []struct {
				ID   string `json:"optionId"`
				Name string `json:"name"`
			} `json:"options"`
		}
		_ = json.Unmarshal(p.Params, &params)
		found, response := false, ""
		for _, o := range params.Options {
			if o.ID == req.OptionID {
				found, response = true, o.Name
			}
		}
		if !found {
			a.mu.Unlock()
			return nil, fmt.Errorf("invalid permission option")
		}
		if admit != nil {
			if err := admit(); err != nil {
				a.mu.Unlock()
				return nil, err
			}
		}
		delete(a.permissions, req.PermissionID)
		a.permissionRecordLocked(p, "submitting", "")
		a.controlling = true
		a.publishLocked()
		a.mu.Unlock()
		err := a.send(map[string]any{"jsonrpc": "2.0", "id": p.rpcID, "result": map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": req.OptionID}}})
		a.mu.Lock()
		outcome := "responded"
		if err != nil {
			outcome = "unknown"
		}
		a.permissionRecordLocked(p, outcome, response)
		a.mu.Unlock()
		a.finishControl(err)
		// The response is consumed before writing: failure is never auto-replayed.
		if err != nil {
			a.r.stop()
			return nil, fmt.Errorf("permission write outcome unknown: %w", err)
		}
		return map[string]bool{"accepted": true}, nil
	}
	if req.Action == "cancel" {
		if a.state.Busy != "prompt" || (req.OperationRef != "" && (a.active == nil || a.active.ref != req.OperationRef)) {
			a.mu.Unlock()
			return nil, fmt.Errorf("no prompt to cancel")
		}
		if admit != nil {
			if err := admit(); err != nil {
				a.mu.Unlock()
				return nil, err
			}
		}
		id := a.state.SessionID
		elicitations := a.pendingElicitationsLocked()
		a.clearElicitationsLocked("cancelled")
		permissions := a.permissions
		a.clearPermissionsLocked("cancelled")
		a.state.Busy = "cancelling"
		a.controlling = true
		a.publishLocked()
		a.mu.Unlock()
		for _, e := range elicitations {
			if err := a.sendElicitationResponse(e.rpcID, api.ACPElicitationResponse{Action: "cancel"}); err != nil {
				a.finishControl(err)
				return nil, err
			}
		}
		for _, p := range permissions {
			if err := a.send(map[string]any{"jsonrpc": "2.0", "id": p.rpcID, "result": map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}}); err != nil {
				a.finishControl(err)
				return nil, err
			}
		}
		err := a.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": id}})
		a.finishControl(err)
		return map[string]bool{"accepted": true}, err
	}
	defer a.mu.Unlock()
	return a.enqueueLocked(req)
}
