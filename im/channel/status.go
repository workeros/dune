package channel

import "context"

// BindingStats are durable, per-Binding counts. They do not imply that a
// remote platform or Agent is healthy; transport state is reported separately.
type BindingStats struct {
	Queued               int64 `json:"queued"`
	Claimed              int64 `json:"claimed"`
	Submitting           int64 `json:"submitting"`
	FailedEvents         int64 `json:"failed_events"`
	UnknownEvents        int64 `json:"unknown_events"`
	UnknownConversations int64 `json:"unknown_conversations"`
	UnknownDeliveries    int64 `json:"unknown_deliveries"`
}

type StatusStore interface {
	BindingStats(context.Context, string) (BindingStats, error)
}
