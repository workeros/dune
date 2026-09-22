package fabricd

import (
	"encoding/json"
	"sort"
	"unicode/utf8"

	"github.com/aiomni/dune/pkg/api"
)

const conversationTextPartBytes = 48 * 1024

func boundedConversationEntry(entry api.ACPEntry) api.ACPEntry {
	entry.ContentOmitted = true
	switch {
	case entry.Message != nil:
		message := *entry.Message
		if len(message.Meta) > 16*1024 {
			message.Meta = omittedConversationValue(message.Meta)
		}
		tail := message.Tail
		if len(tail) == 0 {
			tail = message.Content
		}
		message.Content = boundConversationBlocks(message.Content, conversationTextPartBytes, false)
		message.Tail = boundConversationBlocks(tail, conversationTextPartBytes, true)
		entry.Message = &message
		entry.Omissions = []api.ACPOmission{{Path: "message.content", Reason: "entry_budget; retained prefix and separate tail"}}
	case entry.Tool != nil:
		tool := *entry.Tool
		tool.Fields = boundConversationFields(tool.Fields, 192*1024)
		entry.Tool = &tool
		entry.Omissions = []api.ACPOmission{{Path: "tool.fields", Reason: "entry_budget"}}
	case entry.Terminal != nil:
		terminal := *entry.Terminal
		terminal.Fields = boundConversationFields(terminal.Fields, 16*1024)
		terminal.OutputMeta = omittedConversationValue(terminal.OutputMeta)
		entry.Terminal = &terminal
	case entry.Activity != nil:
		entry.Activity = &api.ACPActivity{UpdateType: entry.Activity.UpdateType, Data: omittedConversationValue(entry.Activity.Data)}
		entry.Omissions = []api.ACPOmission{{Path: "activity.data", Reason: "entry_budget"}}
	}
	return entry
}

// Budget the encoded string, including JSON/HTML escaping. UTF-8 boundaries
// are selected before encoding, including for a tail that starts mid-rune.
func boundedConversationText(text string, budget int, tail bool) string {
	if budget < 2 {
		return ""
	}
	if len(api.Payload(text)) <= budget {
		return text
	}
	part := func(n int) string {
		if tail {
			start := len(text) - n
			for start < len(text) && !utf8.RuneStart(text[start]) {
				start++
			}
			return text[start:]
		}
		for n > 0 && n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		return text[:n]
	}
	low, high := 0, min(len(text), budget)
	for low < high {
		middle := (low + high + 1) / 2
		if len(api.Payload(part(middle))) <= budget {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return part(low)
}

func boundConversationBlocks(blocks []json.RawMessage, budget int, tail bool) []json.RawMessage {
	result := []json.RawMessage{}
	remaining := budget - 2
	for n := 0; n < len(blocks) && remaining > 128 && len(result) < 64; n++ {
		index := n
		if tail {
			index = len(blocks) - n - 1
		}
		block := blocks[index]
		if len(block) > remaining {
			var fields map[string]json.RawMessage
			if json.Unmarshal(block, &fields) == nil && rawString(fields, "type") == "text" {
				block = api.Payload(map[string]string{"type": "text", "text": boundedConversationText(rawString(fields, "text"), remaining-64, tail)})
			} else {
				kind := boundedConversationText(rawString(fields, "type"), 1024, false)
				block = api.Payload(map[string]any{"type": kind, "dune_omitted": true})
			}
		}
		if len(block) > remaining {
			break
		}
		result = append(result, block)
		remaining -= len(block) + 1
	}
	if tail {
		for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
			result[left], result[right] = result[right], result[left]
		}
	}
	return result
}

func omittedConversationValue(value json.RawMessage) json.RawMessage {
	kind := "scalar"
	if len(value) > 0 {
		switch value[0] {
		case '{':
			kind = "object"
		case '[':
			kind = "array"
		case '"':
			kind = "string"
		}
	}
	return api.Payload(map[string]any{"dune_omitted": true, "json_type": kind, "encoded_bytes": len(value)})
}

func boundConversationFields(fields map[string]json.RawMessage, budget int) map[string]json.RawMessage {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := map[string]json.RawMessage{}
	remaining := budget - 2
	for _, key := range keys {
		if len(key) > 1024 || len(result) >= 128 {
			continue
		}
		value := fields[key]
		if len(value) > 32*1024 {
			value = omittedConversationValue(value)
		}
		cost := len(api.Payload(key)) + len(value) + 2
		if cost > remaining {
			continue
		}
		result[key] = value
		remaining -= cost
	}
	return result
}

func boundedConversationFailure(failure *api.ACPFailure) *api.ACPFailure {
	if failure == nil {
		return nil
	}
	return &api.ACPFailure{Code: boundedConversationText(failure.Code, 256, false), Detail: boundedConversationText(failure.Detail, 4096, false)}
}
