package fabricd

import (
	"encoding/json"
	"strings"

	"github.com/aiomni/dune/pkg/api"
)

func validNativeID(id string) bool { return id != "" && len(id) <= 4096 }

// A conversation belongs to one isolated ACP connection/session. v2 IDs are
// scoped by that conversation, never by our inferred foreground turn.
func v2MessageKey(id string) string { return "message\x00" + id }

func (s *conversationSlot) updateV2(update map[string]json.RawMessage, turnID string) {
	kind := rawString(update, "sessionUpdate")
	s.mutate(func(m *conversationModel) {
		switch kind {
		case "user_message", "agent_message", "agent_thought", "user_message_chunk", "agent_message_chunk", "agent_thought_chunk":
			id := rawString(update, "messageId")
			if !validNativeID(id) {
				m.retainUnknownUpdate(kind, update, turnID)
				return
			}
			key := v2MessageKey(id)
			entry := m.find(key)
			role, channel := "agent", "message"
			if strings.HasPrefix(kind, "user_") {
				role = "user"
			}
			if strings.HasPrefix(kind, "agent_thought") {
				channel = "thought"
			}
			if entry == nil {
				entry = &api.ACPEntry{Type: "message", TurnID: turnID, ContextIncomplete: m.description.PrefixEvicted,
					Message: &api.ACPMessage{MessageID: id, Role: role, Channel: channel, Source: "agent", Status: "unknown", Content: []json.RawMessage{}}}
			}
			if entry.Message.Role != role || entry.Message.Channel != channel {
				m.retainUnknownUpdate(kind, update, turnID)
				return
			}
			if turnID != "" && role == "agent" {
				entry.Message.Status = "streaming"
			}
			if !strings.HasSuffix(kind, "_chunk") {
				if meta, present := update["_meta"]; present {
					entry.Message.Meta = meta
				}
			} else if !capabilityObject(update["content"]) {
				m.retainUnknownUpdate(kind, update, turnID)
				return
			}
			if content, present := update["content"]; present {
				if strings.HasSuffix(kind, "_chunk") {
					appendConversationContent(entry.Message, content)
				} else {
					var blocks []json.RawMessage
					if json.Unmarshal(content, &blocks) != nil {
						m.retainUnknownUpdate(kind, update, turnID)
						return
					}
					if blocks == nil {
						blocks = []json.RawMessage{}
					}
					entry.Message.Content, entry.Message.Tail = blocks, nil
					entry.ContentOmitted, entry.Omissions = false, nil
				}
			}
			m.put(*entry, key)
		case "tool_call_update", "tool_call_content_chunk":
			id := rawString(update, "toolCallId")
			if !validNativeID(id) {
				m.retainUnknownUpdate(kind, update, turnID)
				return
			}
			key := "tool\x00" + id
			entry := m.find(key)
			if entry == nil {
				entry = &api.ACPEntry{Type: "tool", TurnID: turnID, ContextIncomplete: m.description.PrefixEvicted,
					Tool: &api.ACPTool{ID: id, Status: "unknown", Fields: map[string]json.RawMessage{}}}
			}
			if kind == "tool_call_content_chunk" {
				if !capabilityObject(update["content"]) {
					m.retainUnknownUpdate(kind, update, turnID)
					return
				}
				var blocks []json.RawMessage
				_ = json.Unmarshal(entry.Tool.Fields["content"], &blocks)
				entry.Tool.Fields["content"] = api.Payload(append(blocks, update["content"]))
			} else {
				for name, value := range update {
					entry.Tool.Fields[name] = value
				}
				if _, present := update["status"]; present {
					entry.Tool.Status, entry.Tool.StatusReason = rawString(update, "status"), ""
					if entry.Tool.Status == "" || len(entry.Tool.Status) > 256 {
						entry.Tool.Status = "unknown"
					}
				}
			}
			m.put(*entry, key)
		case "terminal_update", "terminal_output_chunk":
			m.updateTerminal(update, turnID)
		case "plan_update":
			var plan map[string]json.RawMessage
			if json.Unmarshal(update["plan"], &plan) != nil || !validNativeID(rawString(plan, "planId")) {
				m.retainUnknownUpdate(kind, update, turnID)
				return
			}
			m.setState("plan/"+rawString(plan, "planId"), update["plan"])
		case "session_info_update":
			s.mergeSessionInfo(update)
			m.setState(kind, api.Payload(update))
		case "state_update", "available_commands_update", "config_option_update", "usage_update":
			m.setState(kind, api.Payload(update))
		default:
			m.retainUnknownUpdate(kind, update, turnID)
		}
	})
}

func (m *conversationModel) retainUnknownUpdate(kind string, update map[string]json.RawMessage, turnID string) {
	if len(kind) > 256 {
		kind = "unknown_oversized_type"
	}
	m.put(api.ACPEntry{Type: "activity", TurnID: turnID, ContextIncomplete: true,
		Activity: &api.ACPActivity{UpdateType: kind, Data: api.Payload(update)}}, "")
}

// The RPC response supplies the association; text and arrival order cannot.
// An empty placeholder avoids duplicating content if native chunks arrive next.
func (s *conversationSlot) acknowledgeV2Message(ref, messageID string) bool {
	valid := true
	s.mutate(func(m *conversationModel) {
		key := v2MessageKey(messageID)
		entry := m.find(key)
		if entry == nil {
			entry = &api.ACPEntry{Type: "message", Message: &api.ACPMessage{MessageID: messageID, Role: "user", Channel: "message", Source: "agent", Content: []json.RawMessage{}}}
		}
		if entry.Message.Role != "user" || (entry.Message.OperationRef != "" && entry.Message.OperationRef != ref) {
			valid = false
			return
		}
		entry.TurnID = conversationTurnID(ref)
		entry.Message.OperationRef, entry.Message.Status = ref, "inserted"
		m.put(*entry, key)
	})
	return valid
}
