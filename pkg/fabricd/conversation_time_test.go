package fabricd

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func TestConversationMessageTimeSurvivesUpdatesAndReads(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run("v"+strconv.Itoa(version), func(t *testing.T) {
			slot := conversationFixture(t)
			slot.mutate(func(m *conversationModel) { m.description.ProtocolVersion = version })
			update := func(text string) {
				fields := map[string]json.RawMessage{
					"sessionUpdate": api.Payload("agent_message_chunk"), "messageId": api.Payload("reply"),
					"content": api.Payload(map[string]string{"type": "text", "text": text}),
				}
				if version == 2 {
					slot.updateV2(fields, "turn-request")
				} else {
					slot.update(fields, "turn-request")
				}
			}
			before := time.Now()
			slot.startTurn("request", promptContent(api.ACPAction{Text: "question"}))
			if version == 2 {
				slot.acknowledgeV2Message("request", "question")
			}
			question := conversationPage(t, slot).Entries[1].Message
			if question.RecordedAt == nil || question.RecordedAt.Before(before) || question.RecordedAt.After(time.Now()) {
				t.Fatalf("live user message has no host time: %+v", question)
			}
			if version == 2 {
				slot.updateV2(map[string]json.RawMessage{
					"sessionUpdate": api.Payload("user_message"), "messageId": api.Payload("question"),
					"content": api.Payload([]map[string]string{{"type": "text", "text": "question"}}),
				}, "turn-request")
				if got := conversationPage(t, slot).Entries[1].Message.RecordedAt; got == nil || !got.Equal(*question.RecordedAt) {
					t.Fatal("native user message replaced insertion time")
				}
			}
			update("first")
			first := conversationPage(t, slot).Entries[2]
			if first.Message.RecordedAt == nil || first.Message.RecordedAt.Before(before) || first.Message.RecordedAt.After(time.Now()) {
				t.Fatalf("live reply has no host time: %+v", first.Message)
			}
			update(strings.Repeat("more ", api.MaxACPEntryBytes))
			slot.finishTurn(api.AgentOperation{Ref: "request", State: "completed"})
			page := conversationPage(t, slot)
			final := page.Entries[2]
			if !final.ContentOmitted || final.Message.Status != "completed" {
				t.Fatal("fixture did not exercise truncation and completion")
			}
			if final.Message.RecordedAt == nil || !final.Message.RecordedAt.Equal(*first.Message.RecordedAt) {
				t.Fatal("streaming, truncation or completion changed message time")
			}
			get, err := slot.get(api.ACPConversationGet{ConversationID: page.Conversation.ID, EntryIDs: []string{first.ID}})
			if err != nil || len(get.Entries) != 1 || get.Entries[0].Message.RecordedAt == nil || !get.Entries[0].Message.RecordedAt.Equal(*first.Message.RecordedAt) {
				t.Fatalf("entry refresh changed message time: %+v, %v", get, err)
			}
		})
	}
}

func TestConversationDoesNotAssignCurrentTimeToHistoryOrLostContext(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run("v"+strconv.Itoa(version), func(t *testing.T) {
			slot := conversationFixture(t)
			slot.begin(api.ACPAction{Action: "load", SessionID: "history"})
			slot.mutate(func(m *conversationModel) { m.description.ProtocolVersion = version })
			update := func(id string) {
				fields := map[string]json.RawMessage{
					"sessionUpdate": api.Payload("agent_message_chunk"), "messageId": api.Payload(id),
					"content": api.Payload(map[string]string{"type": "text", "text": "retained content"}),
				}
				if version == 2 {
					slot.updateV2(fields, "")
				} else {
					slot.update(fields, "")
				}
			}
			update("history-message")
			slot.opened("history", "/work", "succeeded", nil)
			update("history-message")
			if conversationPage(t, slot).Entries[0].Message.RecordedAt != nil {
				t.Fatal("history acquired the load or update time")
			}
			slot.store.maxEntries = 1
			update("live-message")
			update("history-message")
			last := conversationPage(t, slot).Entries[0]
			if !last.ContextIncomplete || last.Message.RecordedAt != nil {
				t.Fatal("evicted message acquired a replacement time")
			}
		})
	}
}
