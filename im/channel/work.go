package channel

import (
	"context"
	"errors"
	"time"
)

var ErrBindingChanged = errors.New("IM Binding changed before Agent submission")
var ErrClaimLost = errors.New("IM work claim expired or was replaced before Agent submission")

// WorkItem is a lease on one durably accepted event. The opaque token fences
// stale workers; a claimed event can be retried until BeginSubmission commits.
type WorkItem struct {
	Message InboundMessage
	Token   string
}

// WorkQueue separates transport ACK from Agent execution. A host must commit
// BeginSubmission before calling an Agent: a crash after that boundary leaves
// the event in an unknown state for reconciliation, never automatic replay.
// For revision-stamped events, BeginSubmission must atomically fence a changed
// or disabled Binding and return ErrBindingChanged without advancing state.
// If its claim no longer owns the event, BeginSubmission returns ErrClaimLost;
// this proves no submission barrier was committed by that call.
type WorkQueue interface {
	InboxStore
	Claim(context.Context, time.Duration) (WorkItem, bool, error)
	// PrepareRoute durably binds the canonical session key to a claim. It
	// reports whether an earlier unfinished event for that session must run
	// first; callers defer the current claim rather than submitting it.
	PrepareRoute(context.Context, WorkItem, SessionKey) (bool, error)
	ReleaseClaim(context.Context, WorkItem) error
	// DeferClaim returns a busy conversation's event to the queue without
	// charging a failed processing attempt. It must not be claimable before
	// the delay elapses and must fence stale claim tokens.
	DeferClaim(context.Context, WorkItem, time.Duration) error
	Ignore(context.Context, WorkItem) error
	BeginSubmission(context.Context, WorkItem) error
	Complete(context.Context, WorkItem) error
	MarkFailed(context.Context, WorkItem, string) error
	MarkUnknown(context.Context, WorkItem, string) error
}
