package lifecycle

import "time"

const (
	AccessCloseWaiting   = "waiting"
	AccessCloseConfirmed = "confirmed"
	AccessCloseTimedOut  = "timed_out"
)

// ManagedDestruction is durable acceptance of one resource cleanup. Access is
// closed before this value is returned. CloseDeadline is fixed at acceptance
// and does not slide when another worker takes over the external deletion.
type ManagedDestruction struct {
	Operation
	ResourceRef                   string
	MachineID                     string
	AccessClosedAt, CloseDeadline time.Time
	AccessCloseOutcome            string
}

// AccessClosure is one durable request for an application incarnation to close
// every connection and accepted stream belonging to the old machine binding.
type AccessClosure struct {
	OperationID, InstanceID, MachineID string
	BindingRevision                    int64
	AcknowledgedAt                     time.Time
}
