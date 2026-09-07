package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/wire"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

const (
	RoleSDK    = "sdk"
	RoleDaemon = "daemon"
	RolePeer   = "peer"
	// RoleEither is an explicit opt-in for a standalone shared credential.
	// Hosted machine credentials must use RoleDaemon.
	RoleEither     = "either"
	HandlerTimeout = time.Second
)

// BindingContext contains protocol constraints confirmed by the caller. It
// deliberately carries no user, session, Runner, or provider identity.
type BindingContext struct {
	Target, Role string
	// PeerBootID is the entry Gateway identity verified by the peer transport.
	// It is required exclusively for RolePeer; peer identity grants no user access.
	PeerBootID string
	// Admission optionally bounds this connection by a shared application lease.
	Admission *AdmissionLease
}

func (b BindingContext) valid() bool {
	if b.Target == "" || b.Admission.Remaining() <= 0 {
		return false
	}
	switch b.Role {
	case RoleSDK, RoleDaemon, RoleEither:
		return b.PeerBootID == ""
	case RolePeer:
		return wire.ValidID(b.PeerBootID)
	default:
		return false
	}
}

// ConnectionHandler is supplied for every authenticated connection. Open is
// called only after protocol validation and may run concurrently across streams.
// Handlers must respect cancellation and must not retain or mutate messages.
// Core owns all reading, forwarding, bounded writing, and connection shutdown.
type ConnectionHandler interface {
	Connected(context.Context, *Connection) error
	Open(context.Context, *pb.Message, *Stream) (StreamHandler, error)
}

type Direction uint8

const (
	ToFabric Direction = iota
	ToClient
)

// StreamHandler checks every subsequent message before delivery or forwarding.
// Each direction is ordered; opposite directions can call Message concurrently.
// Closed runs exactly once, after Message has returned in both directions.
type StreamHandler interface {
	Message(context.Context, Direction, *pb.Message) error
	Closed(error)
}

// Connection is a cancellation handle, not a readable or writable connection.
type Connection struct{ cancel context.CancelCauseFunc }

func (c *Connection) Cancel(err error) { c.cancel(err) }

// Stream exposes controlled responses and cancellation, never the raw stream.
// Send is serialized and has a bounded write deadline. Cancel is idempotent and
// unblocks pending reads/writes. Operations after cancellation fail.
type Stream struct {
	ctx           context.Context
	admission     *AdmissionLease
	cancel        context.CancelCauseFunc
	client        *wire.Stream
	mu            sync.Mutex
	forward       bool
	opened        bool
	peer          *Route
	accessContext []byte
}

// Context ends with this stream, including local handling and route closure.
func (s *Stream) Context() context.Context { return s.ctx }

func (s *Stream) Send(m *pb.Message) error {
	if s.admission.Remaining() <= 0 {
		return fmt.Errorf("application admission expired")
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return s.client.Send(m)
}
func (s *Stream) Cancel(err error) { s.cancel(err) }

// Forward chooses forwarding to the validated binding. It is only valid during
// Open. Without this call, the module handles the stream using Send and Message.
func (s *Stream) Forward() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.opened {
		return fmt.Errorf("forwarding must be chosen during Open")
	}
	if s.peer != nil {
		return fmt.Errorf("remote forwarding requires an application access context")
	}
	s.forward = true
	return nil
}

// PeerRoute returns the fixed remote owner selected for this connection. Local
// and inbound peer streams return false: an inbound peer can never relay again.
func (s *Stream) PeerRoute() (Route, bool) {
	if s.peer == nil {
		return Route{}, false
	}
	return cloneRoute(*s.peer), true
}

// ForwardPeer is chosen by the application after authorizing the request and
// constructing a bounded, independently verifiable context for this exact peer
// route and request. Core does not parse it. Generic Forward fails on this path.
func (s *Stream) ForwardPeer(accessContext []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.opened || s.peer == nil || len(accessContext) == 0 || len(accessContext) > MaxAccessContext {
		return fmt.Errorf("remote route and bounded access context required during Open")
	}
	s.accessContext = append([]byte(nil), accessContext...)
	s.forward = true
	return nil
}

func (s *Stream) finishOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened = true
	return s.forward
}

// A blocked hook cannot keep the client stream alive beyond its deadline.
// Host callbacks are trusted Go code and must return when ctx is cancelled;
// their capacity remains reserved until they return.
func (s *Stream) hook(fn func(context.Context) error) error {
	if s.admission.Remaining() <= 0 {
		return fmt.Errorf("application admission expired")
	}
	ctx, cancel := context.WithTimeout(s.ctx, HandlerTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { s.Cancel(context.Cause(ctx)) })
	defer stop()
	err := fn(ctx)
	if s.admission.Remaining() <= 0 {
		return fmt.Errorf("application admission expired")
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return err
}
