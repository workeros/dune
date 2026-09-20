package wire

import (
	"encoding/json"
	"sync"

	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

// StreamClass is computed from the decoded operation, never a caller-supplied
// priority. These are concurrency budgets, not business admission or authority.
type StreamClass string

const (
	StreamOpening      StreamClass = "opening"
	StreamOrdinary     StreamClass = "ordinary"
	StreamRead         StreamClass = "read"
	StreamWait         StreamClass = "wait"
	StreamPermission   StreamClass = "permission"
	StreamCancel       StreamClass = "cancel"
	StreamStop         StreamClass = "stop"
	StreamForget       StreamClass = "forget"
	MaxRequestReaders              = 8
	MaxReservedStreams             = 16
	// Includes the connection control stream. Ordinary streams cannot borrow
	// reserved execution slots even when the transport backlog has room.
	MaxPendingStreams = 1 + MaxRequestReaders + MaxStreams + 6*MaxReservedStreams
)

func RequestClass(m *pb.Message) StreamClass {
	if m.Kind != "request" || m.RequestId == "" || len(m.RequestId) > 128 {
		return StreamOrdinary
	}
	switch m.Operation {
	case "machine.info", "runtime.list", "runtime.get", "acp.state", "acp.raw.state", "acp.raw.read", "acp.conversation.read", "acp.conversation.get", "agent.operation.read", "runtime.capture", "runtime.scrollback", "profile.status":
		return StreamRead
	case "agent.operation.wait":
		// Long polls cannot occupy the slots used to read current state.
		return StreamWait
	case "submission.get":
		var key api.SubmissionKey
		if Decode(m, &key) == nil && key.Validate() == nil && matchesSubmission(key.Target, m) {
			return StreamRead
		}
	case "submission.acp", "runtime.stop", "runtime.forget":
		var request api.SubmissionRequest
		if Decode(m, &request) != nil || request.SubmissionKey.Validate() != nil || request.Target.RuntimeID == "" || !matchesSubmission(request.Target, m) {
			return StreamOrdinary
		}
		if m.Operation != "submission.acp" {
			if request.Operation != m.Operation || (len(request.Payload) != 0 && string(request.Payload) != "null") {
				return StreamOrdinary
			}
			if m.Operation == "runtime.stop" {
				return StreamStop
			}
			return StreamForget
		}
		var action api.ACPAction
		if request.Operation != "acp.action" || json.Unmarshal(request.Payload, &action) != nil {
			return StreamOrdinary
		}
		if action.Action == "permission" && api.ValidateSubmissionID(action.PermissionID) == nil && action.OptionID != "" {
			return StreamPermission
		}
		if action.Action == "cancel" && api.ValidateSubmissionID(action.OperationRef) == nil {
			return StreamCancel
		}
	}
	return StreamOrdinary
}

func matchesSubmission(t api.SubmissionTarget, m *pb.Message) bool {
	return t.MachineID == m.Target && t.RuntimeID == m.RuntimeId && t.RuntimeIncarnation == m.RuntimeIncarnation && t.RuntimeGeneration == m.RuntimeGeneration
}

// StreamCapacity bounds both undecoded first messages and classified handlers.
// Reading the first message has a separate deadline at each protocol boundary.
// scale=1 is a connection/Engine budget; Gateway uses scale=8 globally.
type StreamCapacity struct{ slots map[StreamClass]chan struct{} }

func NewStreamCapacity(scale int) *StreamCapacity {
	if scale < 1 {
		panic("stream capacity scale must be positive")
	}
	c := &StreamCapacity{slots: make(map[StreamClass]chan struct{})}
	for _, class := range []StreamClass{StreamOpening, StreamOrdinary, StreamRead, StreamWait, StreamPermission, StreamCancel, StreamStop, StreamForget} {
		limit := MaxReservedStreams
		if class == StreamOpening {
			limit = MaxRequestReaders
		} else if class == StreamOrdinary {
			limit = MaxStreams
		}
		c.slots[class] = make(chan struct{}, scale*limit)
	}
	return c
}

type StreamLease struct {
	mu       sync.Mutex
	capacity *StreamCapacity
	class    StreamClass
	released bool
}

func (c *StreamCapacity) Acquire(class StreamClass) *StreamLease {
	select {
	case c.slots[class] <- struct{}{}:
		return &StreamLease{capacity: c, class: class}
	default:
		return nil
	}
}

// Move releases the first-message reader only after securing the actual class.
// On failure the reader remains bounded until the rejection has been sent.
func (l *StreamLease) Move(class StreamClass) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return false
	}
	if l.class == class {
		return true
	}
	select {
	case l.capacity.slots[class] <- struct{}{}:
		<-l.capacity.slots[l.class]
		l.class = class
		return true
	default:
		return false
	}
}

func (l *StreamLease) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.released {
		l.released = true
		<-l.capacity.slots[l.class]
	}
}

func (c *StreamCapacity) Snapshot() map[string]api.CapacityUsage {
	out := make(map[string]api.CapacityUsage, len(c.slots))
	for class, slots := range c.slots {
		out[string(class)] = api.CapacityUsage{Used: len(slots), Limit: cap(slots)}
	}
	return out
}
