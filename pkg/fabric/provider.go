package fabric

import (
	"context"
	"errors"
	"time"
)

var ErrProviderUnavailable = errors.New("managed provider unavailable")

// ProviderBindingRef is the immutable provider configuration selected when a
// Managed create request is accepted.
type ProviderBindingRef struct {
	ID       string
	Revision int64
}

func (r ProviderBindingRef) Valid() bool {
	return boundedIdentifier(r.ID, 128) && r.Revision > 0
}

// ProviderBinding exposes one exact provider configuration without its raw
// credentials. Resolver implementations may refresh credentials for the same
// authentication subject while retaining this ID and revision.
type ProviderBinding struct {
	ProviderBindingRef
	FabricID     string
	Availability AvailabilityProvider
	Create       CreateProvider
	Bootstrap    BootstrapProvider
	Inspect      InspectProvider
	Renew        RenewProvider
	Destroy      DestroyProvider
	Candidate    CandidateProvider
	PauseResume  PauseResumeProvider
}

func (b ProviderBinding) Valid() bool {
	return b.ProviderBindingRef.Valid() && b.FabricID != "" &&
		b.Availability != nil && b.Create != nil && b.Bootstrap != nil &&
		b.Inspect != nil && b.Renew != nil && b.Destroy != nil && b.Candidate != nil
}

// ProviderResolver selects the current binding in the authenticated tenant
// scope and restores an exact binding for durable lifecycle work.
type ProviderResolver interface {
	Current(context.Context, string, string) (ProviderBinding, error)
	Exact(context.Context, string, int64) (ProviderBinding, error)
}

type AvailabilityReason string

const (
	AvailabilityMaintenance   AvailabilityReason = "maintenance"
	AvailabilityCapacity      AvailabilityReason = "capacity"
	AvailabilityConfiguration AvailabilityReason = "configuration"
	AvailabilityUnreachable   AvailabilityReason = "unreachable"
	AvailabilityUnknown       AvailabilityReason = "unknown"
)

// Availability is a public, bounded answer about accepting new resources.
// Existing resources must remain inspectable, renewable and destroyable while
// Available is false. Reason is a fixed code rather than an SDK error string.
type Availability struct {
	Available bool               `json:"available"`
	Reason    AvailabilityReason `json:"unavailable_reason,omitempty"`
}

func (a Availability) Valid() bool {
	if a.Available {
		return a.Reason == ""
	}
	switch a.Reason {
	case AvailabilityMaintenance, AvailabilityCapacity, AvailabilityConfiguration, AvailabilityUnreachable, AvailabilityUnknown:
		return true
	default:
		return false
	}
}

// AvailabilityProvider performs a bounded, read-only readiness check for new
// allocations in one Fabric namespace. It must not enumerate resources or
// mutate provider state. Errors are shown only as the fixed unreachable reason;
// returned fields and provider error text are ignored.
type AvailabilityProvider interface {
	Availability(context.Context) (Availability, error)
}

// Action identifies one provider call already reserved in durable storage.
// ID is the action-specific idempotency key. Issuer and ExecutionRevision let
// an adapter use provider-side fencing when its platform supports it. A caller
// must never replace these values with a later worker's execution identity.
type Action struct {
	ID                      string
	OperationID             string
	RunnerID                string
	FabricID                string
	RequestDigest           string
	ResourceRef             string
	Issuer                  string
	BindingRevision         int64
	ProviderBindingID       string
	ProviderBindingRevision int64
	ExecutionRevision       int64
	RenewUntil              time.Time
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

// RenewCall grants one mutation toward the absolute target stored in Action.
// The provider must treat Action.ID as the stable idempotency key.
type RenewCall struct {
	Action Action
}

// RenewReconcileCall only observes the original renewal. KnownExpiresAt is the
// latest provider fact Dune accepted and does not authorize another mutation.
type RenewReconcileCall struct {
	Action         Action
	KnownExpiresAt time.Time
}

// DestroyCall grants deletion of the exact resource named by Action. The
// provider must treat Action.ID as the stable idempotency key.
type DestroyCall struct {
	Action Action
}

// DestroyReconcileCall only observes the original deletion. It never grants
// permission to issue another delete request when the action cannot be found.
type DestroyReconcileCall struct {
	Action Action
}

// CandidateCall asks an adapter to verify that an operator-supplied resource
// reference belongs to the original provider action. It is read-only and does
// not grant permission to create, bootstrap, renew or destroy anything.
type CandidateCall struct {
	Action               Action
	CandidateResourceRef string
}

// InspectCall identifies one already associated resource. Inspect is read-only;
// it carries no action key and grants no permission to create, renew or destroy.
type InspectCall struct {
	RunnerID, FabricID, ResourceRef string
	BindingRevision                 int64
	ProviderBindingID               string
	ProviderBindingRevision         int64
}

// ResourceState is the small provider-independent lifecycle state exposed by
// Managed. In-flight work belongs to the durable Operation rather than here.
type ResourceState string

const (
	ResourceReady   ResourceState = "ready"
	ResourcePaused  ResourceState = "paused"
	ResourceUnknown ResourceState = "unknown"
)

// ResourceCapabilities records facts for one allocated resource. A nil
// pointer on Inspection or Observation means that the provider has not
// confirmed the capabilities yet.
type ResourceCapabilities struct {
	PauseResume  bool `json:"pause_resume"`
	DiskSnapshot bool `json:"disk_snapshot"`
}

type InspectionStatus string

const (
	InspectionConfirmed InspectionStatus = "confirmed"
	InspectionUnknown   InspectionStatus = "unknown"
)

// Inspection reports current facts for the exact resource. A confirmed live
// resource includes its absolute expiry. Gone is affirmative deletion evidence.
// Unknown carries no fields and never authorizes mutation.
type Inspection struct {
	Status       InspectionStatus
	ResourceRef  string
	ExpiresAt    time.Time
	Gone         bool
	State        ResourceState
	Capabilities *ResourceCapabilities
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
	Outcome      Outcome
	ResourceRef  string
	ExpiresAt    time.Time
	Gone         bool
	State        ResourceState
	Capabilities *ResourceCapabilities
}

type PauseResumeCall struct {
	RunnerID, FabricID, ResourceRef string
	BindingRevision                 int64
	ProviderBindingID               string
	ProviderBindingRevision         int64
}

// PauseResumeProvider applies one resource lifecycle mutation. Dune records
// the call before dispatch and uses Inspect for all recovery; callers must not
// replay a call whose result is unknown.
type PauseResumeProvider interface {
	Pause(context.Context, PauseResumeCall) error
	Resume(context.Context, PauseResumeCall) error
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

// InspectProvider performs bounded, read-only resource queries. Errors are
// recorded as unknown (or timed out) and all returned fields are ignored.
type InspectProvider interface {
	Inspect(context.Context, InspectCall) (Inspection, error)
}

// RenewProvider extends one already associated resource. Renew must honor the
// context and use Action.ID for provider-side deduplication or fencing.
// ReconcileRenew must only query the original action correlation and must never
// repeat Renew. Error handling matches CreateProvider: returned fields are
// ignored, and absence is unknown without affirmative terminal evidence.
type RenewProvider interface {
	Renew(context.Context, RenewCall) (Observation, error)
	ReconcileRenew(context.Context, RenewReconcileCall) (Observation, error)
}

// DestroyProvider removes one already access-closed resource. Destroy must use
// Action.ID for provider-side deduplication or fencing. ReconcileDestroy only
// queries the original action correlation. A successful observation requires
// affirmative Gone evidence for the exact resource.
type DestroyProvider interface {
	Destroy(context.Context, DestroyCall) (Observation, error)
	ReconcileDestroy(context.Context, DestroyReconcileCall) (Observation, error)
}

// CandidateProvider verifies a possible resource discovered outside Dune.
// A successful observation must name the exact candidate and satisfy the
// original action's normal success rules. Missing or stale evidence is unknown,
// never permission to repeat the mutation.
type CandidateProvider interface {
	VerifyCandidate(context.Context, CandidateCall) (Observation, error)
}
