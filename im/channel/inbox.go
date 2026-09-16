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
