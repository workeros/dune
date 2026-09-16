package channel

import "context"

// RecoveryStore clears unfinished bookkeeping only when the turn's own
// durable delivery is already confirmed complete. A still-running turn also
// requires its worker lease to expire. It never submits or replays an Agent
// prompt or provider operation. Other outcomes require external investigation.
type RecoveryStore interface {
	ReconcileConfirmedDelivery(context.Context, SessionKey, string) error
}
