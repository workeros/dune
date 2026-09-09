package lifecycle

import (
	"time"

	"github.com/aiomni/dune/pkg/fabric"
)

// ActionRequest describes one immutable provider mutation within an Operation.
// Digest covers its complete controlled arguments, including bootstrap material
// references or renewal policy decisions, never raw credentials. RenewUntil is
// absolute so a repeated request cannot slide the target forward.
type ActionRequest struct {
	Kind, Digest string
	RenewUntil   time.Time
}

// ProviderAction is written before a call can leave Dune. Its ID is the stable
// provider action key; Worker/ExecutionRevision identify the original issuer.
// A new Operation claimant may reconcile it but does not gain dispatch rights.
// A blank outcome means a call may have happened, not proof of non-submission.
type ProviderAction struct {
	ActionRequest
	ID, OperationID, ResourceRef string
	Worker                       string
	ExecutionRevision            int64
	StartedAt, CompletedAt       time.Time
	Outcome                      string
}

// BootstrapGrant is returned only when the action reservation and its
// resource-bound enrollment committed successfully. Token is a short-lived
// secret and is never returned by recovery reads.
type BootstrapGrant struct {
	Action    ProviderAction
	Token     string
	ExpiresAt time.Time
}

// Resource is the confirmed association in a configured Fabric namespace.
// ConfirmedAt is the first accepted observation and never a polling timestamp.
// AccessClosed is durable; provider deletion failure must not reopen access.
type Resource struct {
	RunnerID, FabricID, Ref             string
	ProviderBindingID                   string
	ProviderBindingRevision             int64
	ConfirmedAt, ExpiresAt              time.Time
	Gone, AccessClosed, AccessSuspended bool
	State                               string
	Capabilities                        *fabric.ResourceCapabilities
}

// ActionObservation is a trusted adapter's bounded, verified result for the
// original action. The service must verify operation/resource correlation and
// distinguish a final outcome from cancellation, a timeout or an empty lookup.
// ResourceRef is optional on uncertainty/failure. Once known it cannot change.
// Gone requires affirmative provider evidence; an elapsed expiry is insufficient.
type ActionObservation struct {
	Outcome      string
	ResourceRef  string
	ExpiresAt    time.Time
	Gone         bool
	State        string
	Capabilities *fabric.ResourceCapabilities
}
