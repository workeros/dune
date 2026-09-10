package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

var (
	ErrRouteNotFound = errors.New("route not found")
	ErrRouteBusy     = errors.New("route has a live owner")
	ErrRouteStale    = errors.New("route ownership is stale")
)

// RouteClaim contains the immutable connection and owner identity proposed for
// one ownership term. OwnerAddress names an instance, not a load balancer.
// Binding contains daemon identity and capabilities; its route fields stay zero
// until core has acquired and confirmed a term.
type RouteClaim struct {
	Target, OwnerBootID, OwnerAddress string
	Binding                           api.Binding
}

// Route identifies one ownership term, independently of daemon generation.
// Published is false until fabricd has confirmed this term. An expired or
// released record retains its Epoch and must never be treated as a live route.
type Route struct {
	RouteClaim
	Epoch     uint64
	Published bool
	ExpiresAt time.Time
}

// RouteLease reports the database's remaining duration as well as diagnostic
// wall time. Callers subtract elapsed local monotonic time since starting the
// directory call; receiving a delayed result never extends execution validity.
type RouteLease struct {
	Route
	ValidFor time.Duration
}

// Directory provides atomic ownership operations; core does not know SQL,
// accounts or Runner identities. Implementations must honor context, preserve
// epochs on release and reject an expired Renew.
// Renew never shortens the existing term: prior input grants rely on that bound.
// Errors with an unknown commit outcome must never trigger a blind retry.
// Acquire reserves an unpublished term. Publish follows fabricd confirmation.
// Resolve also returns expired/unpublished records so Acquire can compare their
// epoch; absence alone uses expectedEpoch=0. A live owner cannot be preempted,
// including by another connection with the same OwnerBootID.
type Directory interface {
	Acquire(context.Context, RouteClaim, uint64) (RouteLease, error)
	Publish(context.Context, Route) error
	Resolve(context.Context, string) (RouteLease, error)
	Renew(context.Context, Route) (RouteLease, error)
	Release(context.Context, Route) error
}
