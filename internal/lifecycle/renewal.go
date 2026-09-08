// Package lifecycle contains product lifecycle decisions above the execution
// protocol. Decisions alone do not submit provider calls or authorize access.
package lifecycle

import (
	"time"

	"github.com/aiomni/dune/pkg/renewal"
)

type RenewalConfig = renewal.Config
type RenewalState = renewal.State
type RenewalDecision = renewal.Decision

func DefaultRenewalConfig() RenewalConfig { return renewal.DefaultConfig() }

const (
	InspectionConfirmed = "confirmed"
	InspectionUnknown   = "unknown"
	InspectionTimedOut  = "timed_out"
)

// ResourceInspection is a bounded provider fact read. Confirmed live facts
// include the current absolute expiry; confirmed deletion is expressed only by
// Gone. Unknown and timed-out reads carry no resource assertions.
type ResourceInspection struct {
	Status      string
	ResourceRef string
	ExpiresAt   time.Time
	Gone        bool
}

// RenewalSchedule is one durable maintenance decision and its temporary
// inspection lease. RenewUntil is set only when policy has requested a future
// mutation; this record does not itself grant that mutation.
type RenewalSchedule struct {
	RunnerID, FabricID, ResourceRef string
	BindingRevision                 int64
	PolicyVersion, Reason, Facts    string
	ObservedAt, NextCheckAt         time.Time
	RenewUntil                      time.Time
	Lease
}

// ManagedRenewal is the immutable input copied from a policy schedule into an
// independent mutation Operation. Until never slides during retry or takeover.
type ManagedRenewal struct {
	OperationID, RunnerID, PolicyVersion string
	Until                                time.Time
}

// DecideRenewal is pure: it neither consults browser sessions nor performs
// provider I/O. The scheduler still owns durable claims, stable action keys,
// jitter/backoff, provider limits and confirmation of the resulting expiry.
func DecideRenewal(now time.Time, config RenewalConfig, state RenewalState) (RenewalDecision, error) {
	return renewal.Decide(now, config, state)
}
