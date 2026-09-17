package channel

import (
	"context"
	"errors"
)

var ErrInboxFull = errors.New("IM inbox has reached its pending-event capacity")

// InboxStore durably records a verified inbound event before its transport is
// acknowledged. Insert must be idempotent for the same binding/event pair and
// reject a conflicting payload with that identity. Agent execution is separate
// from transport acknowledgement.
type InboxStore interface {
	Insert(context.Context, InboundMessage) error
}

// BindingGuardedInbox atomically checks an active Binding revision and inserts
// its event. A transport must not ACK a stale revision between a read-only
// binding check and a separate inbox write. The event carries that revision;
// identical redelivery after rotation may be ACKed without changing the
// original stored revision or executing it under the new Agent target.
type BindingGuardedInbox interface {
	InsertForBinding(context.Context, BotBinding, InboundMessage) error
}

// Ingress is the one provider-independent event entry point. Providers retain
// ownership of verification and normalization; the host supplies the store.
type Ingress struct{ Inbox InboxStore }

func (i Ingress) Accept(ctx context.Context, message InboundMessage) error {
	if i.Inbox == nil {
		return errors.New("IM inbox store is required")
	}
	if message.BindingID == "" || message.EventID == "" || message.MessageID == "" {
		return errors.New("IM event identity is incomplete")
	}
	return i.Inbox.Insert(ctx, message)
}

var _ EventSink = Ingress{}
