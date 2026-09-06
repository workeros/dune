// Package lifecycle contains product lifecycle decisions above the execution
// protocol. Decisions alone do not submit provider calls or authorize access.
package lifecycle

import (
	"errors"
	"time"
)

// RenewalConfig belongs to the application configuration. Construct defaults
// before decoding overrides so an explicit zero grace period remains zero.
type RenewalConfig struct {
	Enabled              bool
	ExtendBy             time.Duration
	RenewBefore          time.Duration
	FirstConnectionGrace time.Duration
}

func DefaultRenewalConfig() RenewalConfig {
	return RenewalConfig{Enabled: true, ExtendBy: time.Hour, RenewBefore: 10 * time.Minute, FirstConnectionGrace: 5 * time.Minute}
}

func (c RenewalConfig) Validate() error {
	if c.ExtendBy <= 0 || c.RenewBefore <= 0 || c.RenewBefore >= c.ExtendBy || c.FirstConnectionGrace < 0 {
		return errors.New("renewal requires 0 < renew_before < extend_by and a nonnegative first_connection_grace")
	}
	return nil
}

// RenewalState is the minimum authoritative lifecycle snapshot needed by the
// personal policy. Times survive worker restarts; they are not polling times.
// The caller must bind facts to the current resource and execution revision.
type RenewalState struct {
	Managed             bool
	ResourceRef         string
	ResourceConfirmedAt time.Time
	EverReady           bool
	BootstrapFailed     bool
	Destroying          bool
	// MutationPending includes calls whose completion is unknown. A worker lease
	// expiring or a context being cancelled does not clear this business lock.
	MutationPending bool
	// FactsConfirmed means the provider's current observation confirms the bound
	// resource and its expiry. A cached expiry or a failed lookup is insufficient.
	FactsConfirmed bool
	// ResourceGone requires the provider to confirm deletion or expiry. A failed
	// lookup or the passage of ExpiresAt must not set this fact.
	ResourceGone bool
	ExpiresAt    time.Time
}

// RenewalDecision is a policy result, never a claim that renewal succeeded.
// Until is an absolute target to retain for this action, not recompute on retry.
// RecheckAt asks the scheduler to inspect facts and evaluate again. A zero value
// means no periodic check under this policy; a new lifecycle fact may reevaluate.
type RenewalDecision struct {
	Renew     bool
	Until     time.Time
	RecheckAt time.Time
	Reason    string
}

// DecideRenewal is pure: it neither consults browser sessions nor performs
// provider I/O. The scheduler still owns durable claims, stable action keys,
// jitter/backoff, provider limits and confirmation of the resulting expiry.
func DecideRenewal(now time.Time, config RenewalConfig, state RenewalState) (RenewalDecision, error) {
	if err := config.Validate(); err != nil {
		return RenewalDecision{}, err
	}
	if now.IsZero() {
		return RenewalDecision{}, errors.New("renewal requires the current time")
	}
	stop := func(reason string) (RenewalDecision, error) { return RenewalDecision{Reason: reason}, nil }
	if !state.Managed {
		return stop("ATTACHED")
	}
	if state.Destroying {
		return stop("DESTROYING")
	}
	if state.ResourceGone {
		return stop("RESOURCE_GONE")
	}
	if !config.Enabled {
		return stop("DISABLED")
	}
	if state.ResourceRef == "" {
		return stop("RESOURCE_UNKNOWN")
	}
	if state.ResourceConfirmedAt.IsZero() || state.ResourceConfirmedAt.After(now) {
		return RenewalDecision{}, errors.New("renewal requires the original resource confirmation time")
	}
	if state.BootstrapFailed {
		return stop("BOOTSTRAP_FAILED")
	}
	var graceEnd time.Time
	if !state.EverReady {
		graceEnd = state.ResourceConfirmedAt.Add(config.FirstConnectionGrace)
		if !now.Before(graceEnd) {
			return stop("FIRST_CONNECTION_TIMEOUT")
		}
	}
	// A bounded inspection cadence covers changing provider facts and unfinished
	// calls. The worker adds jitter without delaying beyond this policy deadline.
	recheck := now.Add(30 * time.Second)
	if !graceEnd.IsZero() && graceEnd.Before(recheck) {
		recheck = graceEnd
	}
	wait := func(reason string) (RenewalDecision, error) {
		return RenewalDecision{Reason: reason, RecheckAt: recheck}, nil
	}
	if state.MutationPending {
		return wait("MUTATION_PENDING")
	}
	if !state.FactsConfirmed || state.ExpiresAt.IsZero() {
		return wait("FACTS_UNKNOWN")
	}
	// A past timestamp needs a fresh provider observation. It is not proof of
	// deletion and cannot authorize a blind renewal or recreation after downtime.
	if !state.ExpiresAt.After(now) {
		return wait("EXPIRY_REQUIRES_RECONCILIATION")
	}
	due := state.ExpiresAt.Add(-config.RenewBefore)
	if due.After(now) {
		recheck = due
		if !graceEnd.IsZero() && graceEnd.Before(recheck) {
			recheck = graceEnd
		}
		return wait("NOT_DUE")
	}
	return RenewalDecision{Renew: true, Until: now.Add(config.ExtendBy), Reason: "RENEW"}, nil
}
