package fabricd

import (
	"encoding/json"
	"strings"

	"github.com/aiomni/dune/pkg/api"
)

func rawString(fields map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(fields[key], &value)
	return value
}

func conversationTurnID(ref string) string { return "turn-" + ref }

func (s *conversationSlot) startTurn(ref, text string) {
	s.mutate(func(m *conversationModel) {
		turn := &api.ACPTurn{ID: conversationTurnID(ref), OperationRef: ref, State: "running"}
		m.description.CurrentTurn = turn
		m.put(api.ACPEntry{Type: "turn", TurnID: turn.ID, Turn: turn}, turn.ID)
		m.put(api.ACPEntry{Type: "message", TurnID: turn.ID, Message: &api.ACPMessage{Role: "user", Channel: "message", Source: "client", Status: "attempted",
			Content: []json.RawMessage{api.Payload(map[string]string{"type": "text", "text": text})}}}, "")
	})
}

func (s *conversationSlot) finishTurn(operation api.AgentOperation) {
	s.mutate(func(m *conversationModel) {
		id := conversationTurnID(operation.Ref)
		entry := m.find(id)
		if entry == nil {
			entry = &api.ACPEntry{Type: "turn", TurnID: id, ContextIncomplete: true}
		}
		turn := &api.ACPTurn{ID: id, OperationRef: operation.Ref, State: operation.State, StopReason: operation.StopReason}
		if operation.Error != "" {
			turn.Error = &api.ACPFailure{Code: "OPERATION_FAILED", Detail: operation.Error}
		}
		entry.Turn = turn
		m.description.CurrentTurn = turn
		m.put(*entry, id)
		for _, retained := range m.entries {
			value := retained.value
			if value.TurnID != id || value.Message == nil || value.Message.Role != "agent" {
				continue
			}
			message := *value.Message
			message.Status = operation.State
			value.Message = &message
			m.put(value, retained.key)
		}
	})
}

func (s *conversationSlot) update(update map[string]json.RawMessage, turnID string) {
	kind := rawString(update, "sessionUpdate")
	if kind == "" {
		kind = rawString(update, "session_update")
	}
	s.mutate(func(m *conversationModel) {
		switch kind {
		case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk":
			role, channel := "agent", "message"
			if kind == "user_message_chunk" {
				role = "user"
			}
			if kind == "agent_thought_chunk" {
				channel = "thought"
			}
			nativeID := rawString(update, "messageId")
			if nativeID == "" {
				nativeID = rawString(update, "message_id")
			}
			key := ""
			var entry *api.ACPEntry
			if nativeID != "" {
				key = strings.Join([]string{"message", role, channel, turnID, nativeID}, "\x00")
				entry = m.find(key)
			} else if len(m.entries) > 0 {
				last := m.entries[len(m.entries)-1].value
				if last.Message != nil && last.Message.MessageID == "" && last.Message.Role == role && last.Message.Channel == channel && last.Message.Source == "agent" && last.TurnID == turnID {
					var copy api.ACPEntry
					_ = json.Unmarshal(api.Payload(last), &copy)
					entry = &copy
				}
			}
			if entry == nil {
				entry = &api.ACPEntry{Type: "message", TurnID: turnID, Message: &api.ACPMessage{Role: role, Channel: channel, MessageID: nativeID, Source: "agent", Status: "unknown", Content: []json.RawMessage{}}}
			}
			if turnID != "" && role == "agent" {
				entry.Message.Status = "streaming"
			}
			if content := update["content"]; len(content) > 0 {
				appendConversationContent(entry.Message, content)
			}
			m.put(*entry, key)
		case "tool_call", "tool_call_update":
			id := rawString(update, "toolCallId")
			if id == "" {
				id = rawString(update, "tool_call_id")
			}
			if id == "" {
				m.put(api.ACPEntry{Type: "activity", ContextIncomplete: true, Activity: &api.ACPActivity{UpdateType: kind, Data: api.Payload(update)}}, "")
				return
			}
			key := "tool\x00" + turnID + "\x00" + id
			entry := m.find(key)
			if entry == nil {
				entry = &api.ACPEntry{Type: "tool", TurnID: turnID, ContextIncomplete: kind != "tool_call", Tool: &api.ACPTool{ID: id, Status: "unknown", Fields: map[string]json.RawMessage{}}}
			}
			for field, value := range update {
				entry.Tool.Fields[field] = value
			}
			if status, ok := update["status"]; ok {
				_ = json.Unmarshal(status, &entry.Tool.Status)
			}
			m.put(*entry, key)
		case "plan", "available_commands_update", "current_mode_update", "config_option_update", "usage_update":
			state := make(map[string]json.RawMessage, len(m.description.State)+1)
			for key, value := range m.description.State {
				state[key] = value
			}
			state[kind] = api.Payload(update)
			if len(api.Payload(state)) > api.MaxACPConversationStateBytes {
				state[kind] = json.RawMessage(`{"duneOmitted":true}`)
				m.description.ContentOmitted = true
			}
			m.description.State = state
		default:
			m.put(api.ACPEntry{Type: "activity", TurnID: turnID, Activity: &api.ACPActivity{UpdateType: kind, Data: api.Payload(update)}}, "")
		}
	})
}

func appendConversationContent(message *api.ACPMessage, content json.RawMessage) {
	var next map[string]json.RawMessage
	if json.Unmarshal(content, &next) == nil && rawString(next, "type") == "text" && len(message.Content) > 0 {
		last := len(message.Content) - 1
		var previous map[string]json.RawMessage
		if json.Unmarshal(message.Content[last], &previous) == nil && rawString(previous, "type") == "text" {
			previous["text"] = api.Payload(rawString(previous, "text") + rawString(next, "text"))
			message.Content[last] = api.Payload(previous)
			return
		}
	}
	message.Content = append(message.Content, append(json.RawMessage(nil), content...))
}

func (a *acpController) recordConversationUpdate(params json.RawMessage) {
	var envelope struct {
		SessionID string                     `json:"sessionId"`
		Update    map[string]json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &envelope) != nil || envelope.Update == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if envelope.SessionID == "" || envelope.SessionID != a.state.SessionID {
		a.conversation.mutate(func(m *conversationModel) { m.description.ContextIncomplete = true })
		return
	}
	turnID := ""
	if a.active != nil && a.active.request.Action == "prompt" && !a.active.responded && a.active.rpcID != "" {
		turnID = conversationTurnID(a.active.ref)
	}
	a.conversation.update(envelope.Update, turnID)
}
