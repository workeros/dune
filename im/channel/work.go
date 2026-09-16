package channel

import (
	"context"
	"time"
)

// WorkItem is a lease on one durably accepted event. The opaque token fences
// stale workers; a claimed event can be retried until BeginSubmission commits.
type WorkItem struct {
	Message InboundMessage
	Token   string
}

// WorkQueue separates transport ACK from Agent execution. A host must commit
// BeginSubmission before calling an Agent: a crash after that boundary leaves
// the event in an unknown state for reconciliation, never automatic replay.
type WorkQueue interface {
	InboxStore
	Claim(context.Context, time.Duration) (WorkItem, bool, error)
	ReleaseClaim(context.Context, WorkItem) error
	Ignore(context.Context, WorkItem) error
	BeginSubmission(context.Context, WorkItem) error
	Complete(context.Context, WorkItem) error
	MarkUnknown(context.Context, WorkItem, string) error
}
