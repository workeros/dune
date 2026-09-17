package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// TurnDeliveryID names exactly one outbound response for a durably accepted
// event. Recovery uses the same mapping without re-running the Agent turn.
func TurnDeliveryID(bindingID, eventID string) string {
	digest := sha256.Sum256([]byte(bindingID + "\x00" + eventID))
	return "im-" + hex.EncodeToString(digest[:])
}

// Delivery is one Agent turn's outbound response. Pending and unknown phases
// require reconciliation before any external operation can be replayed.
// ProviderState is opaque and versioned by the provider.
type Delivery struct {
	ID                   string          `json:"id"`
	Session              SessionKey      `json:"session"`
	Address              ReplyAddress    `json:"address"`
	Mode                 string          `json:"mode"`
	Phase                string          `json:"phase"`
	Operation            string          `json:"operation,omitempty"`
	ProviderStateVersion int             `json:"provider_state_version"`
	ProviderState        json.RawMessage `json:"provider_state,omitempty"`
	AgentTurnCompleted   bool            `json:"agent_turn_completed,omitempty"`
	Revision             int64           `json:"revision"`
}

type DeliveryStore interface {
	Reserve(context.Context, Delivery) (Delivery, bool, error)
	Commit(context.Context, Delivery) (Delivery, error)
}

// DeliveryManager owns the provider-independent delivery state machine.
// Providers supply opaque state snapshots and operation labels; they cannot
// skip a durable intent or silently retry an uncertain external operation.
type DeliveryManager struct{ Store DeliveryStore }

// ErrOutboundRejected means a Provider rejected the reply before making any
// external request. A transport error must never be wrapped with this value.
var ErrOutboundRejected = errors.New("IM outbound rejected before external request")

func RejectOutbound(cause error) error {
	return errors.Join(ErrOutboundRejected, cause)
}

func (m DeliveryManager) Reserve(ctx context.Context, initial Delivery) (Delivery, bool, error) {
	if m.Store == nil {
		return Delivery{}, false, errors.New("delivery store is unavailable")
	}
	return m.Store.Reserve(ctx, initial)
}

func (m DeliveryManager) Intent(ctx context.Context, d Delivery, operation string, providerState json.RawMessage) (Delivery, error) {
	if operation == "" || (d.Phase != "reserved" && d.Phase != "active") {
		return Delivery{}, errors.New("delivery is not ready for a new operation")
	}
	d.Phase, d.Operation, d.ProviderState = "pending", operation, providerState
	return m.Store.Commit(ctx, d)
}

func (m DeliveryManager) Confirm(ctx context.Context, d Delivery, complete bool, providerState json.RawMessage) (Delivery, error) {
	if d.Phase != "pending" || d.Operation == "" {
		return Delivery{}, errors.New("delivery has no pending operation to confirm")
	}
	d.Phase = "active"
	if complete {
		d.Phase = "complete"
	}
	d.Operation, d.ProviderState = "", providerState
	return m.Store.Commit(ctx, d)
}

func (m DeliveryManager) Unknown(ctx context.Context, d Delivery) (Delivery, error) {
	if d.Phase != "pending" || d.Operation == "" {
		return Delivery{}, errors.New("delivery has no pending operation to reconcile")
	}
	d.Phase = "unknown"
	return m.Store.Commit(ctx, d)
}

// Reject records a known local failure. It cannot confirm delivery and is
// distinct from unknown, where a platform side effect may have occurred.
func (m DeliveryManager) Reject(ctx context.Context, d Delivery) (Delivery, error) {
	if d.Phase != "pending" || d.Operation == "" {
		return Delivery{}, errors.New("delivery has no pending operation to reject")
	}
	d.Phase, d.Operation, d.ProviderState = "failed", "", nil
	return m.Store.Commit(ctx, d)
}
