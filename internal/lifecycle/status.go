package lifecycle

import (
	"time"

	"github.com/aiomni/dune/pkg/fabric"
)

// ManagedStatus is an authorized, display-only lifecycle snapshot. Stage and
// ProviderOutcome describe persisted facts; they do not grant execution or
// imply that an unknown external mutation was rolled back.
type ManagedStatus struct {
	Operation
	Stage, ProviderOutcome, ResourceRef string
	ExpiresAt                           time.Time
	RenewalPolicyVersion                string
	RenewalReason                       string
	RenewalObservedAt                   time.Time
	RenewalNextCheckAt                  time.Time
	RenewalUntil                        time.Time
	AccessClosed                        bool
	AccessSuspended                     bool
	ResourceState                       string
	Capabilities                        *fabric.ResourceCapabilities
	AccessCloseOutcome                  string
	AccessCloseDeadline                 time.Time
}
