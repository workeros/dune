// Package observe defines bounded structured events emitted by a Dune host.
// It contains no product implementation and does not provide reliable audit
// storage; applications choose and own a Sink appropriate for their deployment.
package observe

import (
	"context"
	"time"
)

const (
	AccessCheck            = "access.check"
	GatewayBackpressure    = "gateway.backpressure"
	GatewayConnection      = "gateway.connection"
	GatewayPeerDial        = "gateway.peer_dial"
	GatewayRoute           = "gateway.route"
	GatewayRouteRenewal    = "gateway.route_renewal"
	GatewayStream          = "gateway.stream"
	HostAdmissionRenewal   = "host.admission_renewal"
	ManagedProviderCall    = "managed.provider_call"
	ManagedRenewalDecision = "managed.renewal_decision"
	Dropped                = "observe.dropped"
)

// Event contains only bounded operational facts and controlled identifiers.
// It deliberately has no arbitrary attributes or error text, so emitters cannot
// accidentally attach commands, prompts, file contents, credentials or private
// provider configuration. Subject is also excluded; PrincipalID and Namespace
// are sufficient to correlate the authenticated Dune actor.
type Event struct {
	Time           time.Time `json:"time"`
	Name           string    `json:"name"`
	Outcome        string    `json:"outcome,omitempty"`
	DurationMicros int64     `json:"duration_micros,omitempty"`

	PrincipalID  string `json:"principal_id,omitempty"`
	Namespace    string `json:"namespace,omitempty"`
	RunnerID     string `json:"runner_id,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	Target       string `json:"target,omitempty"`
	Operation    string `json:"operation,omitempty"`
	Suboperation string `json:"suboperation,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	DecisionID   string `json:"decision_id,omitempty"`
	// Reason is used only for a validated, non-sensitive policy reason code.
	Reason        string `json:"reason,omitempty"`
	OperationID   string `json:"operation_id,omitempty"`
	ActionID      string `json:"action_id,omitempty"`
	FabricID      string `json:"fabric_id,omitempty"`
	ResourceRef   string `json:"resource_ref,omitempty"`
	PolicyVersion string `json:"policy_version,omitempty"`
	Route         string `json:"route,omitempty"`
	Role          string `json:"role,omitempty"`
	OwnerID       string `json:"owner_id,omitempty"`
	Incarnation   string `json:"incarnation,omitempty"`
	Revision      int64  `json:"revision,omitempty"`
	Generation    uint64 `json:"generation,omitempty"`
	Epoch         uint64 `json:"epoch,omitempty"`
	Count         uint64 `json:"count,omitempty"`
}

// Sink receives events serially from the host's bounded dispatcher. Record must
// honor ctx cancellation and return promptly. A sink is observational: it must
// not call back into the App or gate the operation represented by the event.
// The host never closes a caller-owned Sink.
type Sink interface {
	Record(context.Context, Event)
}

type SinkFunc func(context.Context, Event)

func (f SinkFunc) Record(ctx context.Context, event Event) { f(ctx, event) }
