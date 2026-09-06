package daemon

// ACP state lives beside the Agent on the development machine. Only pending
// RPCs, capability/session metadata and permissions are retained; message
// updates go straight to bounded live subscriptions, never to a transcript.
import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type acpAction struct {
	Action       string `json:"action"`
	Text         string `json:"text,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
	PermissionID string `json:"permission_id,omitempty"`
	OptionID     string `json:"option_id,omitempty"`
}
type acpPermission struct {
	ID     string          `json:"id"`
	Params json.RawMessage `json:"params"`
	rpcID  json.RawMessage
}
type acpState struct {
	Revision    uint64          `json:"revision"`
	Ready       bool            `json:"ready"`
	Busy        string          `json:"busy"`
	SessionID   string          `json:"session_id"`
	Cwd         string          `json:"cwd"`
	CanList     bool            `json:"can_list"`
	CanLoad     bool            `json:"can_load"`
	Agent       json.RawMessage `json:"agent,omitempty"`
	Permissions []acpPermission `json:"permissions"`
	List        json.RawMessage `json:"list,omitempty"`
	Error       string          `json:"error,omitempty"`
	StopReason  string          `json:"stop_reason,omitempty"`
}
type acpReply struct {
	Result json.RawMessage
	Err    error
}
type acpController struct {
	mu          sync.Mutex
	r           *runtime
	state       acpState
	pending     map[string]chan acpReply
	permissions map[string]acpPermission
	methods     map[string]string
	done        chan struct{}
	once        sync.Once
}

func newACPController(r *runtime) *acpController {
	return &acpController{r: r, state: acpState{Busy: "initialize", Cwd: r.cwd}, pending: map[string]chan acpReply{}, permissions: map[string]acpPermission{}, done: make(chan struct{}), methods: map[string]string{}}
}
func (a *acpController) snapshotLocked() acpState {
	s := a.state
	s.Permissions = make([]acpPermission, 0, len(a.permissions))
	for _, p := range a.permissions {
		s.Permissions = append(s.Permissions, p)
	}
	return s
}
func (a *acpController) snapshot() acpState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked()
}
func (a *acpController) publishLocked() {
	a.state.Revision++
	a.r.emit(&pb.Message{Kind: "acp_state", Payload: api.Payload(a.snapshotLocked())})
}
func (a *acpController) send(v any) error { return a.r.p.Write(append(api.Payload(v), '\n')) }
func (a *acpController) rpc(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := wire.ID()
	ch := make(chan acpReply, 1)
	a.mu.Lock()
	a.pending[id] = ch
	a.methods[id] = method
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.pending, id); delete(a.methods, id); a.mu.Unlock() }()
	if err := a.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	var expiry <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expiry = t.C
	}
	select {
	case reply := <-ch:
		return reply.Result, reply.Err
	case <-a.done:
		return nil, fmt.Errorf("ACP Agent exited; pending request is no longer valid")
	case <-expiry:
		// A timed-out lifecycle call has an unknown result. Do not admit another
		// operation to the same process and accidentally use the wrong session.
		a.r.stop()
		return nil, fmt.Errorf("ACP %s timed out; process stopped, request was not replayed", method)
	}
}
func (a *acpController) closed() {
	a.once.Do(func() { close(a.done) })
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Ready = false
	a.state.Busy = ""
	a.permissions = map[string]acpPermission{}
	if a.state.Error == "" {
		a.state.Error = "ACP Agent exited"
	}
	a.publishLocked()
}
func (a *acpController) receive(data []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	if m.Method == "" {
		var id string
		if json.Unmarshal(m.ID, &id) != nil {
			return
		}
		a.mu.Lock()
		ch := a.pending[id]
		if ch != nil && a.methods[id] != "session/list" {
			a.permissions = map[string]acpPermission{}
			a.publishLocked()
		}
		a.mu.Unlock()
		if ch != nil {
			reply := acpReply{Result: m.Result}
			if m.Error != nil {
				reply.Err = fmt.Errorf("ACP %d: %s", m.Error.Code, m.Error.Message)
			}
			select {
			case ch <- reply:
			default:
			}
		}
		return
	}
	if m.Method == "session/update" && len(m.ID) == 0 {
		a.r.emit(&pb.Message{Kind: "acp_update", Payload: m.Params})
		return
	}
	if len(m.ID) == 0 {
		return
	}
	if m.Method == "session/request_permission" {
		var params struct {
			SessionID string `json:"sessionId"`
			Options   []struct {
				ID string `json:"optionId"`
			} `json:"options"`
		}
		a.mu.Lock()
		permissionBytes := len(m.Params)
		for _, p := range a.permissions {
			permissionBytes += len(p.Params)
		}
		valid := permissionBytes <= 128*1024 && json.Unmarshal(m.Params, &params) == nil && params.SessionID == a.state.SessionID && a.state.Busy != "" && len(params.Options) > 0 && len(params.Options) <= 32 && len(a.permissions) < 16
		for _, p := range a.permissions {
			if string(p.rpcID) == string(m.ID) {
				valid = false
			}
		}
		if valid {
			key := wire.ID()
			a.permissions[key] = acpPermission{ID: key, Params: append(json.RawMessage(nil), m.Params...), rpcID: m.ID}
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
func (a *acpController) initialize() {
	result, err := a.rpc("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "dune", "version": "0.1.0"}}, 30*time.Second)
	var init struct {
		Version      int             `json:"protocolVersion"`
		Info         json.RawMessage `json:"agentInfo"`
		Capabilities struct {
			Load     bool `json:"loadSession"`
			Sessions struct {
				List json.RawMessage `json:"list"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err == nil && (json.Unmarshal(result, &init) != nil || init.Version != 1) {
		err = fmt.Errorf("unsupported ACP protocol version; expected 1")
	}
	a.mu.Lock()
	if err != nil {
		a.state.Error = err.Error()
		a.state.Busy = ""
		a.publishLocked()
		a.mu.Unlock()
		a.r.stop()
		return
	}
	a.state.Ready = true
	a.state.Busy = ""
	a.state.Agent = init.Info
	a.state.CanLoad = init.Capabilities.Load
	var object map[string]any
	a.state.CanList = json.Unmarshal(init.Capabilities.Sessions.List, &object) == nil && object != nil
	a.publishLocked()
	a.mu.Unlock()
	// New session is explicit in the UI: users may instead load Agent history.
}
func (a *acpController) action(req acpAction) (any, error) {
	a.mu.Lock()
	select {
	case <-a.done:
		a.mu.Unlock()
		return nil, fmt.Errorf("ACP Agent exited")
	default:
	}
	if req.Action == "permission" {
		p, ok := a.permissions[req.PermissionID]
		if !ok {
			a.mu.Unlock()
			return nil, fmt.Errorf("permission expired or already answered")
		}
		var params struct {
			Options []struct {
				ID string `json:"optionId"`
			} `json:"options"`
		}
		_ = json.Unmarshal(p.Params, &params)
		found := false
		for _, o := range params.Options {
			if o.ID == req.OptionID {
				found = true
			}
		}
		if !found {
			a.mu.Unlock()
			return nil, fmt.Errorf("invalid permission option")
		}
		delete(a.permissions, req.PermissionID)
		a.publishLocked()
		a.mu.Unlock()
		err := a.send(map[string]any{"jsonrpc": "2.0", "id": p.rpcID, "result": map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": req.OptionID}}})
		// The response is consumed before writing: failure is never auto-replayed.
		if err != nil {
			a.r.stop()
			return nil, fmt.Errorf("permission write outcome unknown: %w", err)
		}
		return map[string]bool{"accepted": true}, nil
	}
	if req.Action == "cancel" {
		if a.state.Busy != "prompt" {
			a.mu.Unlock()
			return nil, fmt.Errorf("no prompt to cancel")
		}
		id := a.state.SessionID
		permissions := a.permissions
		a.permissions = map[string]acpPermission{}
		a.state.Busy = "cancelling"
		a.publishLocked()
		a.mu.Unlock()
		for _, p := range permissions {
			if err := a.send(map[string]any{"jsonrpc": "2.0", "id": p.rpcID, "result": map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}}); err != nil {
				return nil, err
			}
		}
		return map[string]bool{"accepted": true}, a.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": id}})
	}
	if !a.state.Ready || a.state.Busy != "" {
		a.mu.Unlock()
		return nil, fmt.Errorf("ACP busy (%s); wait before starting or loading history", a.snapshot().Busy)
	}
	if (req.Action == "list" && !a.state.CanList) || (req.Action == "load" && !a.state.CanLoad) {
		a.mu.Unlock()
		return nil, &api.Error{Code: "UNSUPPORTED", Detail: "Agent did not advertise session/" + req.Action}
	}
	cwd := a.state.Cwd
	if req.Cwd != "" {
		cwd = req.Cwd
	}
	if !filepath.IsAbs(cwd) || len(cwd) > 4096 || len(req.SessionID) > 4096 || len(req.Cursor) > 8192 {
		a.mu.Unlock()
		return nil, fmt.Errorf("invalid ACP session parameters")
	}
	params := map[string]any{}
	switch req.Action {
	case "new":
		params = map[string]any{"cwd": cwd, "mcpServers": []any{}}
	case "load":
		if req.SessionID == "" {
			a.mu.Unlock()
			return nil, fmt.Errorf("session ID required")
		}
		params = map[string]any{"cwd": cwd, "sessionId": req.SessionID, "mcpServers": []any{}}
	case "list":
		params["cwd"] = cwd
		if req.Cursor != "" {
			params["cursor"] = req.Cursor
		}
	case "prompt":
		if a.state.SessionID == "" || req.Text == "" || len(req.Text) > 64*1024 {
			a.mu.Unlock()
			return nil, fmt.Errorf("create/load a session and supply 1..65536 bytes of text")
		}
		params = map[string]any{"sessionId": a.state.SessionID, "prompt": []any{map[string]string{"type": "text", "text": req.Text}}}
	default:
		a.mu.Unlock()
		return nil, fmt.Errorf("unsupported ACP action")
	}
	a.state.Busy = req.Action
	a.state.Error = ""
	a.state.StopReason = ""
	if req.Action == "list" {
		a.state.List = nil
	}
	if req.Action == "new" || req.Action == "load" {
		a.state.SessionID = req.SessionID
		a.state.Cwd = cwd
	}
	a.publishLocked()
	// A live replay boundary is distinct from a state snapshot. A browser
	// attaching midway through load must keep its incomplete-history notice.
	if req.Action == "new" || req.Action == "load" {
		a.r.emit(&pb.Message{Kind: "acp_reset"})
	}
	a.mu.Unlock()
	if req.Action == "prompt" {
		a.r.emit(&pb.Message{Kind: "acp_update", Payload: api.Payload(map[string]any{"sessionId": params["sessionId"], "update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": req.Text}}})})
	}
	go func() {
		timeout := 60 * time.Second
		if req.Action == "prompt" {
			timeout = 0
		}
		result, err := a.rpc("session/"+req.Action, params, timeout)
		a.mu.Lock()
		defer a.mu.Unlock()
		if err == nil {
			switch req.Action {
			case "new":
				var s struct {
					ID string `json:"sessionId"`
				}
				if json.Unmarshal(result, &s) != nil || s.ID == "" || len(s.ID) > 4096 {
					err = fmt.Errorf("Agent returned invalid sessionId")
				} else {
					a.state.SessionID = s.ID
				}
			case "list":
				var s struct {
					Sessions []json.RawMessage `json:"sessions"`
				}
				if json.Unmarshal(result, &s) != nil || s.Sessions == nil {
					err = fmt.Errorf("Agent returned invalid session list")
				} else {
					a.state.List = result
				}
			case "prompt":
				var s struct {
					Reason string `json:"stopReason"`
				}
				if json.Unmarshal(result, &s) != nil || s.Reason == "" {
					err = fmt.Errorf("Agent returned invalid prompt result")
				} else {
					a.state.StopReason = s.Reason
				}
			}
		}
		if err != nil {
			a.state.Error = err.Error()
			if req.Action == "load" || req.Action == "new" {
				a.state.SessionID = ""
			}
		}
		a.state.Busy = ""
		a.permissions = map[string]acpPermission{}
		a.publishLocked()
	}()
	return map[string]bool{"accepted": true}, nil
}
