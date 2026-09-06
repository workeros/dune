package identity

import (
	"errors"
	"time"
)

var (
	ErrLinkConflict = errors.New("identity link conflicts with an existing identity or request")
	ErrLinkNotFound = errors.New("identity link request not found")
)

// LinkRequest is an explicit, trusted administrator decision. The host must
// authenticate Actor, authorize the operation and verify ownership of both the
// stable external identity and the target Dune principal before calling Dune.
// Reason records the non-secret verification basis or approval reference.
// RequestID must be stable across retries; reusing it with different fields fails.
// Linking never moves an identity from another principal or merges accounts.
type LinkRequest struct {
	RequestID, Actor, PrincipalID, Namespace, Subject, Reason string
}

// LinkRecord is persisted atomically with the association and session revocation.
// It records a successful link decision, not unsuccessful attempts or all admin
// activity. Hosts remain responsible for auditing their authorization decisions.
type LinkRecord struct {
	LinkRequest
	CreatedAt time.Time
}
