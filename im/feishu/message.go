package feishu

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aiomni/dune/im/channel"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type replyAddress struct {
	ChatKind       channel.ChatKind `json:"chat_kind"`
	ChatID         string           `json:"chat_id"`
	SenderOpenID   string           `json:"sender_open_id"`
	ReplyMessageID string           `json:"reply_message_id"`
	RootMessageID  string           `json:"root_message_id,omitempty"`
	ThreadID       string           `json:"thread_id,omitempty"`
}

func (c *Channel) normalize(event *larkim.P2MessageReceiveV1) (channel.InboundMessage, bool, error) {
	var result channel.InboundMessage
	if event == nil || event.EventV2Base == nil || event.EventV2Base.Header == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil {
		return result, false, errors.New("incomplete Feishu message event")
	}
	if event.EventV2Base.Header.AppID != c.config.AppID {
		return result, false, errors.New("Feishu event App ID mismatch")
	}
	message, sender := event.Event.Message, event.Event.Sender
	if value(sender.SenderType) != "user" || value(message.MessageType) != "text" {
		return result, false, nil
	}
	if sender.SenderId == nil || value(sender.SenderId.OpenId) == "" || value(message.ChatId) == "" || value(message.MessageId) == "" || event.EventV2Base.Header.EventID == "" {
		return result, false, errors.New("Feishu message missing required IDs")
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(value(message.Content)), &body); err != nil {
		return result, false, fmt.Errorf("decode Feishu text content: %w", err)
	}
	if strings.TrimSpace(body.Text) == "" {
		return result, false, nil
	}
	chatKind := channel.ChatKind("")
	switch value(message.ChatType) {
	case "p2p":
		chatKind = channel.ChatDirect
	case "group":
		chatKind = channel.ChatGroup
	default:
		return result, false, nil
	}
	address := replyAddress{
		ChatKind: chatKind, ChatID: value(message.ChatId),
		SenderOpenID:   value(sender.SenderId.OpenId),
		ReplyMessageID: value(message.MessageId),
		RootMessageID:  value(message.RootId), ThreadID: value(message.ThreadId),
	}
	encoded, err := json.Marshal(address)
	if err != nil {
		return result, false, err
	}
	var mentionedIDs []string
	for _, mention := range message.Mentions {
		if mention != nil && mention.Id != nil && value(mention.Id.OpenId) != "" {
			// Admission policy compares these IDs with this application's bot
			// identity. mentioned_type is optional in the event; an exact
			// Open ID match is the authority, not its descriptive type.
			mentionedIDs = append(mentionedIDs, value(mention.Id.OpenId))
		}
	}
	result = channel.InboundMessage{
		BindingID: c.binding.ID, BindingRevision: c.binding.Revision, EventID: event.EventV2Base.Header.EventID,
		MessageID: value(message.MessageId), SenderID: value(sender.SenderId.OpenId),
		ChatID: value(message.ChatId), ChatKind: chatKind,
		MentionedIDs: mentionedIDs, Text: body.Text,
		Address: channel.ReplyAddress{Provider: Kind, Version: 1, Data: encoded},
	}
	return result, true, nil
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
