package lifecycle

import (
	"errors"
	"time"
)

var ErrBusy = errors.New("lifecycle operation is busy")
var ErrLeaseLost = errors.New("lifecycle execution lease lost")
var ErrIntentConflict = errors.New("lifecycle request key already has a different intent")

// Intent is an already authorized, bounded lifecycle request. The repository
// never accepts provider credentials, commands or arbitrary request bodies.
// RequestKey is scoped to the actor; Digest covers the entire canonical request.
type Intent struct {
	ID, RequestKey, Digest          string
	PrincipalID, Namespace, Subject string
	RunnerID, FabricID              string
	BindingRevision                 int64
	ProviderBindingID               string
	ProviderBindingRevision         int64
	Action                          string
}

// Operation separates the durable business lock (Exclusive) from the temporary
// right to update its record (Lease). Lease expiry permits observation takeover,
// never implies that an earlier provider call or its side effects have stopped.
type Operation struct {
	Intent
	CreatedAt  time.Time
	FinishedAt time.Time
	Finished   bool
	Exclusive  bool
	Outcome    string
	Lease
}

// Lease is an execution snapshot, not a provider fencing token. Worker must be
// a fresh process incarnation, not a reusable display name. Revision increases
// on every claim; all updates compare it and the original resource binding.
type Lease struct {
	Worker   string
	Revision int64
	Until    time.Time
}
