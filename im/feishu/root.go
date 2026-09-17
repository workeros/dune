package feishu

import (
	"context"
	"errors"
	"fmt"

	"github.com/aiomni/dune/im/channel"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type MessageMetadata struct {
	MessageID string
	ChatID    string
	RootID    string
	ParentID  string
	ThreadID  string
}

type MessageLookup interface {
	LookupMessage(ctx context.Context, messageID string) (MessageMetadata, error)
}

// RootResolver verifies Feishu topic identity. Conversation persistence binds
// the returned thread ref atomically with the canonical session subject.
type RootResolver struct {
	Conversations channel.ConversationStore
	Messages      MessageLookup
}

func (r RootResolver) ResolveGroup(ctx context.Context, msg channel.InboundMessage) (channel.GroupRoute, error) {
	var result channel.GroupRoute
	if msg.ChatKind != channel.ChatGroup || msg.BindingID == "" || msg.ChatID == "" || msg.MessageID == "" || r.Conversations == nil {
		return result, errors.New("incomplete group message or conversation store")
	}
	address, err := decodeAddress(msg.Address)
	if err != nil || address.ChatID != msg.ChatID || address.ReplyMessageID != msg.MessageID {
		return result, errors.New("Feishu message reply address does not match event")
	}
	if address.ThreadID != "" {
		stored, found, err := r.Conversations.FindByThreadRef(ctx, msg.BindingID, msg.ChatID, address.ThreadID)
		if err != nil {
			return result, err
		}
		root := address.RootMessageID
		if root != "" && found && stored.SubjectID != root {
			return result, errors.New("Feishu thread root conflicts with locally bound topic")
		}
		if root == "" {
			if found {
				root = stored.SubjectID
			} else {
				metadata, err := r.lookupChecked(ctx, msg)
				if err != nil {
					return result, err
				}
				if metadata.ThreadID != address.ThreadID {
					return result, errors.New("message is not in the expected Feishu thread")
				}
				root = metadata.RootID
				if root == "" && metadata.ParentID == "" {
					root = metadata.MessageID // verified root message itself
				}
			}
		}
		if root == "" {
			return result, errors.New("Feishu thread root remains unknown")
		}
		return channel.GroupRoute{SubjectID: root, ProviderThreadRef: address.ThreadID}, nil
	}
	if address.RootMessageID != "" {
		// A plain reply can have root_id without belonging to a topic.
		// Inspect the message rather than joining an unrelated thread.
		metadata, err := r.lookupChecked(ctx, msg)
		if err != nil {
			return result, err
		}
		if metadata.ThreadID != "" {
			if metadata.RootID == "" {
				return result, errors.New("Feishu reply is in a thread but has no verified root")
			}
			return channel.GroupRoute{SubjectID: metadata.RootID, ProviderThreadRef: metadata.ThreadID}, nil
		}
	}
	// A top-level group message, including an ordinary non-thread reply,
	// becomes the anchor of a new bot topic when it is admitted.
	return channel.GroupRoute{SubjectID: msg.MessageID}, nil
}

func (r RootResolver) lookupChecked(ctx context.Context, msg channel.InboundMessage) (MessageMetadata, error) {
	if r.Messages == nil {
		return MessageMetadata{}, errors.New("Feishu message lookup is required for ambiguous thread")
	}
	metadata, err := r.Messages.LookupMessage(ctx, msg.MessageID)
	if err != nil {
		return metadata, fmt.Errorf("look up Feishu message: %w", err)
	}
	if metadata.MessageID != msg.MessageID || metadata.ChatID != msg.ChatID {
		return MessageMetadata{}, errors.New("Feishu message lookup does not match event")
	}
	return metadata, nil
}

// LookupMessage uses the bot's ordinary message API when an event omits the
// root. The returned chat/thread IDs are checked again by RootResolver.
func (c *Channel) LookupMessage(ctx context.Context, messageID string) (MessageMetadata, error) {
	var metadata MessageMetadata
	response, err := c.client.Im.V1.Message.Get(ctx, larkim.NewGetMessageReqBuilder().MessageId(messageID).Build())
	if err != nil {
		return metadata, err
	}
	if response == nil || !response.Success() || response.Data == nil || len(response.Data.Items) != 1 || response.Data.Items[0] == nil {
		return metadata, fmt.Errorf("Feishu message lookup failed: %v", response)
	}
	item := response.Data.Items[0]
	metadata = MessageMetadata{
		MessageID: value(item.MessageId), ChatID: value(item.ChatId),
		RootID: value(item.RootId), ParentID: value(item.ParentId), ThreadID: value(item.ThreadId),
	}
	return metadata, nil
}

var _ channel.GroupSubjectResolver = RootResolver{}
var _ MessageLookup = (*Channel)(nil)
