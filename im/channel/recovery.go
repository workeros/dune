package channel

import "context"

// RecoveryStore clears unfinished bookkeeping only when the turn's own
// durable delivery has a known terminal outcome. A still-running turn also
// requires its worker lease to expire. It never submits or replays an Agent
// prompt or provider operation. Unknown outcomes require investigation.
type RecoveryStore interface {
	ReconcileConfirmedDelivery(context.Context, SessionKey, string) error
	ReconcileRejectedDelivery(context.Context, SessionKey, string) error
}
