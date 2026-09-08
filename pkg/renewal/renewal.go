// Package renewal defines the policy boundary for Managed resource renewal.
// Policies decide from a bounded authoritative snapshot; they do not call the
// provider, write Dune metadata or grant execution access.
package renewal

import (
	"context"
	"errors"
	"time"
)

type Config struct {
	Enabled              bool
	ExtendBy             time.Duration
	RenewBefore          time.Duration
	FirstConnectionGrace time.Duration
}

func DefaultConfig() Config {
	return Config{Enabled: true, ExtendBy: time.Hour, RenewBefore: 10 * time.Minute, FirstConnectionGrace: 5 * time.Minute}
}

func (c Config) Validate() error {
	if c.ExtendBy <= 0 || c.RenewBefore <= 0 || c.RenewBefore >= c.ExtendBy || c.FirstConnectionGrace < 0 {
		return errors.New("renewal requires 0 < renew_before < extend_by and a nonnegative first_connection_grace")
	}
	return nil
}

// State contains lifecycle facts that a policy may use. Times come from the
// shared database and survive worker restarts. A policy cannot change them.
type State struct {
	Managed             bool
	ResourceRef         string
	ResourceConfirmedAt time.Time
	EverReady           bool
	BootstrapFailed     bool
	Destroying          bool
	MutationPending     bool
	FactsConfirmed      bool
	ResourceGone        bool
	ExpiresAt           time.Time
}

// Input identifies the resource and actor whose maintenance scope was accepted
// with the original Create operation. It contains no browser session, provider
// credential, template parameters or identity subject.
type Input struct {
	Now         time.Time
	PrincipalID string
	// PrincipalEnabled is the current authoritative Dune account state. It lets
	// enterprise policy decide whether an accepted resource remains maintained.
	PrincipalEnabled bool
	Namespace        string
	RunnerID         string
	FabricID         string
	CreatedAt        time.Time
	State            State
}

// Decision requests one frozen absolute renewal target or a future policy
// evaluation. It never claims that the provider mutation has succeeded.
type Decision struct {
	Renew     bool
	Until     time.Time
	RecheckAt time.Time
	// Reason is a public, non-sensitive code beginning with A-Z and containing
	// only uppercase ASCII letters, digits, underscore, dash, dot, colon or slash.
	Reason string
}

// Policy is called after a fresh provider inspection and outside SQL
// transactions. Implementations must honor context cancellation and must not
// mutate provider or Dune state. Versioning is configured separately by the
// host so every cluster replica uses the same declared policy meaning.
type Policy interface {
	Decide(context.Context, Input) (Decision, error)
}

type PolicyFunc func(context.Context, Input) (Decision, error)

func (f PolicyFunc) Decide(ctx context.Context, input Input) (Decision, error) {
	if f == nil {
		return Decision{}, errors.New("renewal policy function is nil")
	}
	return f(ctx, input)
}

// Personal is Dune's deterministic default policy.
type Personal struct{ Config Config }

func (p Personal) Decide(_ context.Context, input Input) (Decision, error) {
	return Decide(input.Now, p.Config, input.State)
}

// Decide implements the default policy without I/O.
func Decide(now time.Time, config Config, state State) (Decision, error) {
	if err := config.Validate(); err != nil {
		return Decision{}, err
	}
	if now.IsZero() {
		return Decision{}, errors.New("renewal requires the current time")
	}
	stop := func(reason string) (Decision, error) { return Decision{Reason: reason}, nil }
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
		return Decision{}, errors.New("renewal requires the original resource confirmation time")
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
	recheck := now.Add(30 * time.Second)
	if !graceEnd.IsZero() && graceEnd.Before(recheck) {
		recheck = graceEnd
	}
	wait := func(reason string) (Decision, error) {
		return Decision{Reason: reason, RecheckAt: recheck}, nil
	}
	if state.MutationPending {
		return wait("MUTATION_PENDING")
	}
	if !state.FactsConfirmed || state.ExpiresAt.IsZero() {
		return wait("FACTS_UNKNOWN")
	}
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
	return Decision{Renew: true, Until: now.Add(config.ExtendBy), Reason: "RENEW"}, nil
}

// ValidateDecision enforces lifecycle safety independently of policy choice.
// A custom policy can choose when and how far to renew, but cannot mutate a
// missing, stale, unconfirmed or concurrently changing resource.
func ValidateDecision(input Input, decision Decision) error {
	if input.Now.IsZero() || input.PrincipalID == "" || input.RunnerID == "" || input.FabricID == "" || input.CreatedAt.IsZero() || input.CreatedAt.After(input.Now) {
		return errors.New("renewal policy input is incomplete")
	}
	if !validReason(decision.Reason) {
		return errors.New("renewal policy requires a bounded reason")
	}
	if decision.Renew {
		state := input.State
		if decision.Until.UnixMilli() <= state.ExpiresAt.UnixMilli() || !decision.RecheckAt.IsZero() || !state.Managed || state.ResourceRef == "" || state.ResourceConfirmedAt.IsZero() || state.ResourceConfirmedAt.After(input.Now) || state.Destroying || state.ResourceGone || state.BootstrapFailed || state.MutationPending || !state.FactsConfirmed || state.ExpiresAt.IsZero() || !state.ExpiresAt.After(input.Now) {
			return errors.New("renewal policy cannot mutate the current lifecycle state")
		}
		return nil
	}
	if !decision.Until.IsZero() || (!decision.RecheckAt.IsZero() && decision.RecheckAt.UnixMilli() <= input.Now.UnixMilli()) {
		return errors.New("renewal policy returned an invalid schedule")
	}
	return nil
}

func validReason(reason string) bool {
	if len(reason) == 0 || len(reason) > 128 || reason[0] < 'A' || reason[0] > 'Z' {
		return false
	}
	for _, char := range []byte(reason[1:]) {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' && char != '-' && char != '.' && char != ':' && char != '/' {
			return false
		}
	}
	return true
}
