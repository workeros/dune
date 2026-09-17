package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aiomni/dune/im/channel"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func (c *Channel) Send(ctx context.Context, address channel.ReplyAddress, message channel.OutboundMessage) (json.RawMessage, error) {
	if c.config.ReplyMode == ReplyStreaming {
		return nil, channel.RejectOutbound(errors.New("streaming_card requires OpenStream"))
	}
	if strings.TrimSpace(message.Text) == "" {
		return nil, channel.RejectOutbound(errors.New("empty Feishu reply"))
	}
	if message.DeliveryID == "" || len(message.DeliveryID) > 128 {
		return nil, channel.RejectOutbound(errors.New("Feishu reply requires a stable 1..128 byte DeliveryID"))
	}
	to, err := decodeAddress(address)
	if err != nil {
		return nil, channel.RejectOutbound(err)
	}
	if err := c.validateDeliverySession(to, message.Session); err != nil {
		return nil, channel.RejectOutbound(err)
	}
	msgType, content, err := c.finalContent(message.Text)
	if err != nil {
		return nil, channel.RejectOutbound(err)
	}
	uuid := outboundUUID(c.binding.ID, message.DeliveryID, "final-message")
	if err := validateFinalRequestSize(to, msgType, content, uuid); err != nil {
		return nil, channel.RejectOutbound(err)
	}
	messageID, err := c.sendContent(ctx, to, msgType, content, uuid)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"message_id": messageID})
}

func (c *Channel) validateDeliverySession(to replyAddress, session channel.SessionKey) error {
	if session.TenantID != c.binding.TenantID || session.BindingID != c.binding.ID || session.ChatID != to.ChatID || session.SubjectID == "" {
		return errors.New("Feishu delivery session does not match binding or chat")
	}
	if to.ChatKind == channel.ChatDirect && session.SubjectID != to.SenderOpenID {
		return errors.New("Feishu private delivery session does not match sender")
	}
	if to.ChatKind == channel.ChatGroup {
		// A root_id without thread_id is ambiguous until RootResolver looks
		// up the message; only the unambiguous top-level form can be checked
		// against the reply message ID here.
		if to.ThreadID == "" && to.RootMessageID == "" && session.SubjectID != to.ReplyMessageID {
			return errors.New("Feishu new-topic delivery session does not match root message")
		}
		if to.ThreadID != "" && to.RootMessageID != "" && session.SubjectID != to.RootMessageID {
			return errors.New("Feishu thread delivery session does not match root")
		}
	}
	return nil
}

func decodeAddress(address channel.ReplyAddress) (replyAddress, error) {
	var to replyAddress
	if address.Provider != Kind || address.Version != 1 || json.Unmarshal(address.Data, &to) != nil {
		return to, errors.New("invalid Feishu reply address")
	}
	switch to.ChatKind {
	case channel.ChatDirect:
		if !strings.HasPrefix(to.SenderOpenID, "ou_") || to.ChatID == "" {
			return to, errors.New("invalid Feishu private-chat address")
		}
	case channel.ChatGroup:
		if !strings.HasPrefix(to.ChatID, "oc_") || !strings.HasPrefix(to.ReplyMessageID, "om_") {
			return to, errors.New("invalid Feishu group-thread address")
		}
	default:
		return to, errors.New("invalid Feishu chat kind")
	}
	return to, nil
}

func (c *Channel) finalContent(text string) (string, string, error) {
	if c.config.ReplyMode == ReplyFinalCard {
		card := map[string]any{
			"schema": "2.0",
			"body": map[string]any{"elements": []any{
				map[string]any{"tag": "markdown", "content": text},
			}},
		}
		data, err := json.Marshal(card)
		if err != nil {
			return "", "", err
		}
		return "interactive", string(data), nil
	}
	data, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", "", err
	}
	return "text", string(data), nil
}

// Feishu limits the serialized message request body, not merely the inner
// content JSON. Content is a JSON string in that body, so quotes, slashes and
// control characters may be escaped a second time by the SDK.
func validateFinalRequestSize(to replyAddress, msgType, content, uuid string) error {
	var body any
	if to.ChatKind == channel.ChatGroup {
		body = larkim.NewReplyMessageReqBodyBuilder().MsgType(msgType).Content(content).ReplyInThread(true).Uuid(uuid).Build()
	} else {
		body = larkim.NewCreateMessageReqBodyBuilder().ReceiveId(to.SenderOpenID).MsgType(msgType).Content(content).Uuid(uuid).Build()
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	limit := 150 * 1024
	if msgType == "interactive" {
		limit = 30 * 1024
	}
	if len(data) > limit {
		return fmt.Errorf("Feishu %s message request exceeds %d KiB", msgType, limit/1024)
	}
	return nil
}

// sendContent uses the low-level API so a failed thread reply is never
// transparently retried as a new top-level group message.
func (c *Channel) sendContent(ctx context.Context, to replyAddress, msgType, content, uuid string) (string, error) {
	if uuid == "" {
		return "", errors.New("Feishu message UUID is required")
	}
	if to.ChatKind == channel.ChatGroup {
		request := larkim.NewReplyMessageReqBuilder().MessageId(to.ReplyMessageID).
			Body(larkim.NewReplyMessageReqBodyBuilder().MsgType(msgType).
				Content(content).ReplyInThread(true).Uuid(uuid).Build()).Build()
		response, err := c.client.Im.V1.Message.Reply(ctx, request)
		if err != nil {
			return "", err
		}
		if response == nil || !response.Success() || response.Data == nil || value(response.Data.MessageId) == "" {
			return "", fmt.Errorf("Feishu thread reply failed: %v", response)
		}
		return value(response.Data.MessageId), nil
	}
	request := larkim.NewCreateMessageReqBuilder().ReceiveIdType("open_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(to.SenderOpenID).
			MsgType(msgType).Content(content).Uuid(uuid).Build()).Build()
	response, err := c.client.Im.V1.Message.Create(ctx, request)
	if err != nil {
		return "", err
	}
	if response == nil || !response.Success() || response.Data == nil || value(response.Data.MessageId) == "" {
		return "", fmt.Errorf("Feishu direct send failed: %v", response)
	}
	return value(response.Data.MessageId), nil
}

// Feishu deduplicates message create/reply requests by UUID for one hour.
// This narrows duplicate risk, but an unknown result still requires explicit
// reconciliation; the UUID is not a license to replay after the window.
func outboundUUID(bindingID, deliveryID, operation string) string {
	digest := sha256.Sum256([]byte(bindingID + "\x00" + deliveryID + "\x00" + operation))
	return hex.EncodeToString(digest[:16])
}
