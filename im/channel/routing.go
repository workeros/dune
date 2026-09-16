package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// GroupRoute contains a provider-verified subject and an optional lookup ref.
// The ref is not part of the session key.
type GroupRoute struct {
	SubjectID         string
	ProviderThreadRef string
}

type GroupSubjectResolver interface {
	ResolveGroup(context.Context, InboundMessage) (GroupRoute, error)
}

// SessionKey represents one bot conversation without leaking sender identity
// into group thread keys. Its encoded form is safe to use as a storage key.
type SessionKey struct {
	TenantID  string `json:"tenant_id"`
	BindingID string `json:"binding_id"`
	ChatID    string `json:"chat_id"`
	SubjectID string `json:"subject_id"`
}

func (k SessionKey) String() string {
	data, _ := json.Marshal(k)
	sum := sha256.Sum256(data)
	return "im-v1:" + hex.EncodeToString(sum[:])
}

// Route validates that the event belongs to the selected binding and derives
// the conversation key. It never interprets group root IDs on its own.
func Route(ctx context.Context, binding BotBinding, message InboundMessage, subjects GroupSubjectResolver) (SessionKey, string, error) {
	var key SessionKey
	if binding.ID == "" || binding.TenantID == "" || message.BindingID != binding.ID {
		return key, "", errors.New("inbound message does not match a tenant binding")
	}
	key = SessionKey{TenantID: binding.TenantID, BindingID: binding.ID, ChatID: message.ChatID}
	switch message.ChatKind {
	case ChatDirect:
		if message.SenderID == "" || message.ChatID == "" {
			return SessionKey{}, "", errors.New("direct message requires sender and chat IDs")
		}
		key.SubjectID = message.SenderID
	case ChatGroup:
		if message.ChatID == "" || subjects == nil {
			return SessionKey{}, "", errors.New("group message requires chat ID and subject resolver")
		}
		group, err := subjects.ResolveGroup(ctx, message)
		if err != nil {
			return SessionKey{}, "", fmt.Errorf("resolve group subject: %w", err)
		}
		if group.SubjectID == "" {
			return SessionKey{}, "", errors.New("group subject is empty")
		}
		key.SubjectID = group.SubjectID
		return key, group.ProviderThreadRef, nil
	default:
		return SessionKey{}, "", fmt.Errorf("unsupported chat kind %q", message.ChatKind)
	}
	return key, "", nil
}
