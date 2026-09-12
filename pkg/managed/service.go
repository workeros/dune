// Package managed defines the Web-facing Managed Runner integration boundary.
//
// Dune deliberately does not implement cloud lifecycle orchestration. A host
// such as SandDance owns provider calls, durable operations, retries and
// reconciliation, then supplies a Service to the Dune Web API.
package managed

import (
	"context"
	"errors"
	"time"

	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
)

var (
	ErrBusy           = errors.New("managed operation is busy")
	ErrIntentConflict = errors.New("managed request key already has a different intent")
	ErrNotFound       = errors.New("managed resource not found")
	ErrForbidden      = errors.New("managed access denied")
	ErrInvalid        = errors.New("invalid managed request")
)

// Operation is the stable, display-safe state shared with the Dune frontend.
// Its persistence and execution semantics belong to the Service implementation.
type Operation struct {
	ID, OwnerID, RunnerID, FabricID, Action string
	BindingRevision                         int64
	CreatedAt                               time.Time
	Finished                                bool
	Outcome                                 string
}

type Creation struct {
	Runner    runner.Runner
	Operation Operation
}

// Enrollment is the one-shot Dune connector binding returned to a Managed
// implementation. The token must be delivered only to the selected provider
// bootstrap call and must never be persisted in plaintext.
type Enrollment struct {
	Token     string
	ExpiresAt time.Time
}

// RunnerState is the Dune-owned attachment fact visible to a Managed service.
// Lifecycle state and provider facts remain owned by that service.
type RunnerState struct {
	Runner runner.Runner
	Online bool
}

// RunnerAccess is the narrow callback from an external Managed implementation
// into Dune's three-table enterprise schema. IssueEnrollment creates the
// logical Managed Runner and one-shot connector token atomically.
type RunnerAccess interface {
	IssueEnrollment(context.Context, identity.User, string, runner.Runner, string) (Enrollment, error)
	State(context.Context, string, string) (RunnerState, error)
	SetSuspended(context.Context, string, string, bool) error
	Revoke(context.Context, string, string) error
}

// Mutation describes an accepted pause or resume request.
type Mutation struct {
	Operation
}

// Destruction describes an accepted destroy request. The external service owns
// all provider cleanup and recovery; these fields are presentation state only.
type Destruction struct {
	Operation
	ResourceRef                         string
	AccessClosedAt, AccessCloseDeadline time.Time
	AccessCloseOutcome                  string
}

type Status struct {
	Operation
	Stage, ProviderOutcome, ResourceRef, CredentialState, ErrorCode string
	ExpiresAt                                                       time.Time
	RenewalPolicyVersion, RenewalReason                             string
	RenewalObservedAt, RenewalNextCheckAt, RenewalUntil             time.Time
	AccessClosed, AccessSuspended                                   bool
	ResourceState                                                   string
	Capabilities                                                    *fabric.ResourceCapabilities
	AccessCloseOutcome                                              string
	AccessCloseDeadline                                             time.Time
}

// Service is intentionally a high-level product API. Implementations receive
// a verified Dune identity, never the browser cookie, and own their storage,
// workers, provider credentials, retries and reconciliation.
type Service interface {
	// BindRunnerAccess is called exactly once by host.Open before the service is
	// exposed to requests. The implementation must use this capability to create
	// and inspect Dune's logical Runner; it must not retain an earlier binding.
	BindRunnerAccess(RunnerAccess) error
	Templates(context.Context, identity.User, string) ([]fabric.TemplateStatus, error)
	Template(context.Context, identity.User, string, string, string, string) (fabric.TemplateStatus, error)
	Create(context.Context, identity.User, string, string, fabric.CreateRequest) (Creation, error)
	Destroy(context.Context, identity.User, string, string) (Destruction, error)
	Pause(context.Context, identity.User, string, string) (Mutation, error)
	Resume(context.Context, identity.User, string, string) (Mutation, error)
	Status(context.Context, identity.User, string) (Status, error)
	RunnerStatus(context.Context, identity.User, string) (Status, error)
}
