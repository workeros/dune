package fabricd

import (
	"encoding/base64"
	"encoding/json"

	"github.com/aiomni/dune/pkg/api"
)

const maxTerminalOutputBytes = 128 * 1024

func terminalKey(id string) string { return "terminal\x00" + id }

// Decode every chunk independently. Retain bytes, since a UTF-8 code point or
// ANSI sequence may span notifications. Generations prevent the UI from
// appending across a replacement snapshot, retention gap or malformed chunk.
func (m *conversationModel) updateTerminal(update map[string]json.RawMessage, turnID string) {
	kind, id := rawString(update, "sessionUpdate"), rawString(update, "terminalId")
	if !validNativeID(id) {
		m.retainUnknownUpdate(kind, update, turnID)
		return
	}
	key := terminalKey(id)
	entry := m.find(key)
	if entry == nil {
		entry = &api.ACPEntry{Type: "terminal", TurnID: turnID, ContextIncomplete: m.description.PrefixEvicted,
			Terminal: &api.ACPTerminal{ID: id, Output: []byte{}, Generation: 1, Fields: map[string]json.RawMessage{}}}
	}
	terminal := entry.Terminal
	var encoded json.RawMessage
	if kind == "terminal_output_chunk" {
		encoded = update["data"]
	} else {
		for field, value := range update {
			if field != "output" {
				terminal.Fields[field] = value
			}
		}
		if output, exists := update["output"]; exists {
			terminal.Generation++
			terminal.Output, terminal.OutputKnown, terminal.OmittedBytes, terminal.OutputMeta = []byte{}, false, 0, nil
			entry.ContentOmitted, entry.Omissions = false, nil
			if string(output) != "null" {
				var snapshot map[string]json.RawMessage
				if json.Unmarshal(output, &snapshot) == nil {
					encoded, terminal.OutputMeta = snapshot["data"], snapshot["_meta"]
				}
				if encoded == nil {
					encoded = json.RawMessage(`null`)
				}
			}
		}
	}
	if encoded != nil || kind == "terminal_output_chunk" {
		var data string
		decoded, err := []byte(nil), json.Unmarshal(encoded, &data)
		if err == nil && string(encoded) != "null" {
			decoded, err = base64.StdEncoding.Strict().DecodeString(data)
		}
		if err != nil || string(encoded) == "null" {
			terminal.Generation++
			terminal.Output, terminal.OutputKnown, terminal.OmittedBytes = []byte{}, false, 0
			entry.ContentOmitted = true
			entry.Omissions = []api.ACPOmission{{Path: "terminal.output", Reason: "invalid_terminal_bytes; previous decoder state discarded"}}
		} else {
			terminal.OutputKnown = true
			terminal.Output = append(terminal.Output, decoded...)
			if dropped := len(terminal.Output) - maxTerminalOutputBytes; dropped > 0 {
				terminal.Output = append([]byte(nil), terminal.Output[dropped:]...)
				terminal.OmittedBytes += uint64(dropped)
				terminal.Generation++
				entry.ContentOmitted = true
				entry.Omissions = []api.ACPOmission{{Path: "terminal.output", Reason: "terminal_budget; retained tail"}}
			}
		}
	}
	if len(api.Payload(terminal.Fields)) > 16*1024 || len(terminal.Fields) > 128 {
		terminal.Fields = boundConversationFields(terminal.Fields, 16*1024)
		entry.ContentOmitted = true
	}
	if len(terminal.OutputMeta) > 16*1024 {
		terminal.OutputMeta = omittedConversationValue(terminal.OutputMeta)
		entry.ContentOmitted = true
	}
	m.put(*entry, key)
}
