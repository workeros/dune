package channel

import "context"

// RecoveryStore only clears a fenced unknown turn when its own durable
// delivery is already confirmed complete. It never submits or replays an
// Agent prompt or a provider operation. Other outcomes require separate
// external investigation and remain unknown.
type RecoveryStore interface {
	ReconcileConfirmedDelivery(context.Context, SessionKey, string) error
}
