package lifecycle

import "time"

// ManagedStatus is an authorized, display-only lifecycle snapshot. Stage and
// ProviderOutcome describe persisted facts; they do not grant execution or
// imply that an unknown external mutation was rolled back.
type ManagedStatus struct {
	Operation
	Stage, ProviderOutcome, ResourceRef string
	ExpiresAt                           time.Time
	AccessClosed                        bool
	AccessCloseOutcome                  string
	AccessCloseDeadline                 time.Time
}
