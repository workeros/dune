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

func (s *conversationSlot) startTurn(ref string, content []json.RawMessage) {
	s.mutate(func(m *conversationModel) {
		turn := &api.ACPTurn{ID: conversationTurnID(ref), OperationRef: ref, State: "running"}
		m.invalidatesAll = true
		m.description.CurrentTurn = turn
		m.put(api.ACPEntry{Type: "turn", TurnID: turn.ID, Turn: turn}, turn.ID)
		if m.description.ProtocolVersion == 2 {
			return
		}
		m.put(api.ACPEntry{Type: "message", TurnID: turn.ID, Message: &api.ACPMessage{Role: "user", Channel: "message", Source: "client", Status: "attempted",
			Content: content}}, "")
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
			turn.Error = boundedConversationFailure(&api.ACPFailure{Code: "OPERATION_FAILED", Detail: operation.Error})
		}
		entry.Turn = turn
		m.invalidatesAll = true
		m.description.CurrentTurn = turn
		m.put(*entry, id)
		for _, retained := range m.entries {
			value := retained.value
			if value.TurnID != id {
				continue
			}
			if value.Message != nil && value.Message.Role == "agent" {
				message := *value.Message
				message.Status = operation.State
				value.Message = &message
				m.put(value, retained.key)
			}
			if m.description.ProtocolVersion != 2 && value.Tool != nil && value.Tool.Status != "completed" && value.Tool.Status != "failed" {
				tool := *value.Tool
				tool.Status, tool.StatusReason = "unknown", "turn_ended_without_tool_result"
				if operation.State == "cancelled" {
					tool.Status = "interrupted"
				}
				value.Tool = &tool
				m.put(value, retained.key)
			}
		}
	})
}

func (s *conversationSlot) update(update map[string]json.RawMessage, turnID string) {
	kind := rawString(update, "sessionUpdate")
	if kind == "" {
		kind = rawString(update, "session_update")
	}
	if len(kind) > 256 {
		kind = "unknown_oversized_type"
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
			invalidID := len(nativeID) > 4096
			if invalidID {
				m.invalidatesAll = true
				nativeID = ""
				m.description.ContentOmitted = true
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
				entry.ContextIncomplete = m.description.PrefixEvicted
			}
			if invalidID {
				entry.ContextIncomplete = true
				entry.ContentOmitted = true
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
			if id == "" || len(id) > 4096 {
				id = rawString(update, "tool_call_id")
			}
			if id == "" || len(id) > 4096 {
				m.put(api.ACPEntry{Type: "activity", ContextIncomplete: true, Activity: &api.ACPActivity{UpdateType: kind, Data: api.Payload(update)}}, "")
				return
			}
			key := "tool\x00" + turnID + "\x00" + id
			entry := m.find(key)
			// A late update may still name a retained tool after its turn ended.
			// With multiple uses of that native ID the wire attribution is
			// ambiguous; retain a partial object instead of choosing a turn.
			if entry == nil && kind == "tool_call_update" && turnID == "" {
				match := ""
				for _, candidate := range m.entries {
					if candidate.value.Tool != nil && candidate.value.Tool.ID == id {
						if match != "" {
							match = ""
							break
						}
						match = candidate.key
					}
				}
				if match != "" {
					key, entry = match, m.find(match)
				}
			}
			if entry == nil {
				entry = &api.ACPEntry{Type: "tool", TurnID: turnID, ContextIncomplete: kind != "tool_call" || turnID == "", Tool: &api.ACPTool{ID: id, Status: "unknown", Fields: map[string]json.RawMessage{}}}
			}
			for field, value := range update {
				entry.Tool.Fields[field] = value
			}
			if status, ok := update["status"]; ok {
				entry.Tool.Status, entry.Tool.StatusReason = "unknown", ""
				var nativeStatus string
				if json.Unmarshal(status, &nativeStatus) == nil && nativeStatus != "" {
					entry.Tool.Status = nativeStatus
				}
				if len(entry.Tool.Status) > 256 {
					entry.Tool.Status = "unknown"
					entry.ContentOmitted = true
				}
			}
			if kind == "tool_call" {
				entry.ContextIncomplete = entry.TurnID == ""
			}
			m.put(*entry, key)
		case "plan", "available_commands_update", "current_mode_update", "config_option_update", "usage_update":
			m.setState(kind, api.Payload(update))
		default:
			m.put(api.ACPEntry{Type: "activity", TurnID: turnID, Activity: &api.ACPActivity{UpdateType: kind, Data: api.Payload(update)}}, "")
		}
	})
}

func appendConversationContent(message *api.ACPMessage, content json.RawMessage) {
	if len(message.Tail) > 0 {
		tail := &api.ACPMessage{Content: message.Tail}
		appendConversationContent(tail, content)
		message.Tail = tail.Content
		return
	}
	var next map[string]json.RawMessage
	if json.Unmarshal(content, &next) == nil && rawString(next, "type") == "text" && len(next) == 2 && len(message.Content) > 0 {
		last := len(message.Content) - 1
		var previous map[string]json.RawMessage
		if json.Unmarshal(message.Content[last], &previous) == nil && rawString(previous, "type") == "text" && len(previous) == 2 {
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
		a.conversation.store.mergeFailure("invalid_update")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.done:
		return
	default:
	}
	if a.reconnecting {
		return
	}
	if envelope.SessionID == "" || envelope.SessionID != a.state.SessionID {
		a.conversation.store.mergeFailure("session_mismatch")
		a.conversation.mutate(func(m *conversationModel) {
			m.description.ContextIncomplete = true
			m.invalidatesAll = true
		})
		return
	}
	turnID := ""
	withinV2Work := a.state.ProtocolVersion == 2 && (a.state.ForegroundState == "running" || a.state.ForegroundState == "requires_action")
	if a.active.isForeground() && (withinV2Work || (a.state.ProtocolVersion != 2 && !a.active.responded && a.active.rpcID != "")) {
		turnID = conversationTurnID(a.active.ref)
	}
	if a.state.ProtocolVersion == 2 {
		a.conversation.updateV2(envelope.Update, turnID)
		a.applyV2StateLocked(envelope.Update)
	} else {
		a.conversation.update(envelope.Update, turnID)
	}
}

func (m *conversationModel) setState(kind string, data json.RawMessage) {
	m.invalidatesAll = true
	state := make(map[string]json.RawMessage, len(m.description.State)+1)
	for key, value := range m.description.State {
		state[key] = value
	}
	state[kind] = data
	if len(api.Payload(state)) > api.MaxACPConversationStateBytes {
		state = boundConversationFields(state, api.MaxACPConversationStateBytes)
		m.description.ContentOmitted = true
	}
	m.description.State = state
}
