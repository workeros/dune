package fabric

import (
	"context"
	"time"
)

// Action identifies one provider call already reserved in durable storage.
// ID is the action-specific idempotency key. Issuer and ExecutionRevision let
// an adapter use provider-side fencing when its platform supports it. A caller
// must never replace these values with a later worker's execution identity.
type Action struct {
	ID                string
	OperationID       string
	RunnerID          string
	FabricID          string
	RequestDigest     string
	ResourceRef       string
	Issuer            string
	BindingRevision   int64
	ExecutionRevision int64
}

// CreateCall contains the exact validated template snapshot accepted by Dune.
// Provider credentials and private options remain in the adapter instance.
type CreateCall struct {
	Action  Action
	Request CreateRequest
}

// ReconcileCall asks about the original create action. KnownResourceRef is the
// last resource association Dune durably accepted; it may be empty. The call
// does not grant permission to repeat the mutation, even when the provider
// reports no matching resource.
type ReconcileCall struct {
	Action           Action
	KnownResourceRef string
}

// BootstrapCall carries one resource-bound enrollment to the original action.
// Endpoint and GatewayURL are normalized public addresses. EnrollmentToken is
// the only secret and is available only to the first confirmed dispatcher.
type BootstrapCall struct {
	Action              Action
	EnrollmentToken     string
	EnrollmentExpiresAt time.Time
	Endpoint            string
	GatewayURL          string
	Version             string
}

// BootstrapReconcileCall inspects the original action without new enrollment
// material. It never grants permission to inject, install or start again.
type BootstrapReconcileCall struct {
	Action Action
}

type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeUnknown   Outcome = "unknown"
)

// Observation contains bounded facts verified by the adapter. ResourceRef may
// be present for a failed or unknown create when the provider exposed a partial
// allocation. ExpiresAt is a confirmed absolute time, not a requested TTL.
type Observation struct {
	Outcome     Outcome
	ResourceRef string
	ExpiresAt   time.Time
	Gone        bool
}

// CreateProvider is the smallest provider surface used by the first Managed
// stage. Create is called only after durable reservation and a final execution
// check. Reconcile must only inspect the original action correlation; it must
// not call Create or otherwise repeat a mutation.
//
// A non-nil error means the returned Observation is ignored. Deadline errors
// are recorded as timed out; every other error is unknown. An adapter that has
// verified partial facts returns OutcomeUnknown and nil. OutcomeFailed requires
// affirmative terminal evidence, not a missing or expired lookup record.
type CreateProvider interface {
	Create(context.Context, CreateCall) (Observation, error)
	ReconcileCreate(context.Context, ReconcileCall) (Observation, error)
}

// BootstrapProvider installs or starts fabricd for an already confirmed
// resource. Bootstrap must honor context and use Action.ID for provider-side
// deduplication or fencing. ReconcileBootstrap only observes that same action;
// a missing lookup is unknown unless the adapter has affirmative terminal proof.
type BootstrapProvider interface {
	Bootstrap(context.Context, BootstrapCall) (Observation, error)
	ReconcileBootstrap(context.Context, BootstrapReconcileCall) (Observation, error)
}
