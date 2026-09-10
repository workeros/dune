// Package gateway terminates two independent Yamux sessions and relays bounded
// application messages. It never executes commands or retries requests.
package gateway

import (
	"context"
	"fmt"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/observe"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
	"net"
	"sync"
	"time"
)

type route struct {
	ctx       context.Context
	s         *yamux.Session
	b         api.Binding
	input     *wire.InputWindow
	admission *AdmissionLease
	owner     *ownership
	peer      *Route
}

type Gateway struct {
	directory            Directory
	ownerAddress, bootID string
	dialPeer             PeerDialer
	mu                   sync.Mutex
	observeMu            sync.RWMutex
	observer             func(observe.Event)
	closed, draining     bool
	drained              chan struct{}
	routes               map[string]*route
	sessions             map[*yamux.Session]BindingContext
	blocked              map[string]chan struct{}
	targetStreams        map[string]int
	slots                chan struct{}
	streams              chan struct{}
}

func New() *Gateway {
	return &Gateway{
		routes: map[string]*route{}, sessions: map[*yamux.Session]BindingContext{},
		blocked: map[string]chan struct{}{}, targetStreams: map[string]int{},
		slots: make(chan struct{}, 256), streams: make(chan struct{}, 512), drained: make(chan struct{}),
	}
}

func (g *Gateway) Online(target string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.routes[target]
	return r != nil && !r.s.IsClosed() && r.inputAlive()
}

func (g *Gateway) routeError(target string, expected *route) *api.Error {
	if expected.peer != nil {
		if expected.ctx.Err() == nil && !expected.s.IsClosed() {
			return nil
		}
		return &api.Error{Code: "ROUTE_STALE", Detail: "peer connection closed"}
	}
	g.mu.Lock()
	current := g.routes[target]
	g.mu.Unlock()
	if current == expected && !expected.s.IsClosed() && expected.inputAlive() {
		return nil
	}
	code := "STALE_BINDING"
	if expected.owner != nil {
		code = "ROUTE_STALE"
	}
	return &api.Error{Code: code, Detail: "reconnect SDK for current binding"}
}

// Disconnect irreversibly rejects new connections and streams for target in
// this Gateway incarnation, closes every existing target session, and returns a
// channel that closes after their accepted streams and callbacks have exited.
// Repeated calls return the same completion channel.
func (g *Gateway) Disconnect(target string) <-chan struct{} {
	g.mu.Lock()
	done, exists := g.blocked[target]
	if !exists {
		done = make(chan struct{})
		g.blocked[target] = done
	}
	var closeSessions []*yamux.Session
	for sess, binding := range g.sessions {
		if binding.Target == target {
			closeSessions = append(closeSessions, sess)
		}
	}
	if !exists {
		g.finishDisconnectLocked(target)
	}
	g.mu.Unlock()
	for _, sess := range closeSessions {
		sess.Close()
	}
	return done
}

// Drop closes current sessions for target without permanently blocking a
// future connector. It is used after a Managed provider has accepted pause;
// durable suspension in metadata prevents reconnect until resume.
func (g *Gateway) Drop(target string) {
	g.mu.Lock()
	var sessions []*yamux.Session
	for session, binding := range g.sessions {
		if binding.Target == target {
			sessions = append(sessions, session)
		}
	}
	g.mu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}

func (g *Gateway) finishDisconnectLocked(target string) {
	done, blocked := g.blocked[target]
	if !blocked || g.targetStreams[target] != 0 {
		return
	}
	select {
	case <-done:
		return
	default:
	}
	for _, binding := range g.sessions {
		if binding.Target == target {
			return
		}
	}
	close(done)
}

func (g *Gateway) Close() {
	g.mu.Lock()
	g.closed = true
	g.beginDrainLocked()
	var closeSessions []*yamux.Session
	for sess := range g.sessions {
		closeSessions = append(closeSessions, sess)
	}
	g.mu.Unlock()
	for _, sess := range closeSessions {
		sess.Close()
	}
}

// ServeConn takes ownership of conn, including on error. ctx controls the
// connection lifetime. The handler is mandatory and belongs to this connection.
func (g *Gateway) ServeConn(ctx context.Context, conn net.Conn, binding BindingContext, handler ConnectionHandler) error {
	if conn == nil {
		return fmt.Errorf("nil connection")
	}
	defer conn.Close()
	if handler == nil || !binding.valid() || (binding.Role == RolePeer && g.directory == nil) {
		return fmt.Errorf("binding and connection handler required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case g.slots <- struct{}{}:
	default:
		g.emit(observe.Event{Name: observe.GatewayBackpressure, Outcome: "session_limit", Target: binding.Target, Role: binding.Role})
		return fmt.Errorf("session limit")
	}
	defer func() { <-g.slots }()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	s, e := yamux.Server(conn, wire.Config())
	if e != nil {
		return e
	}
	defer s.Close()
	g.mu.Lock()
	_, targetBlocked := g.blocked[binding.Target]
	if g.closed || g.draining || targetBlocked {
		g.mu.Unlock()
		return fmt.Errorf("gateway is not accepting connections")
	}
	g.sessions[s] = binding
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.sessions, s)
		g.finishDisconnectLocked(binding.Target)
		g.mu.Unlock()
	}()
	go func() {
		select {
		case <-s.CloseChan():
			cancel(context.Canceled)
		case <-ctx.Done():
		case <-binding.Admission.Done():
			cancel(fmt.Errorf("application admission expired"))
		}
	}()
	hookTimer := time.AfterFunc(HandlerTimeout, func() { cancel(context.DeadlineExceeded) })
	e = handler.Connected(ctx, &Connection{cancel: cancel})
	hookTimer.Stop()
	if e != nil {
		return e
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	timer := time.AfterFunc(5*time.Second, func() { s.Close() })
	defer timer.Stop()
	raw, e := s.AcceptStream()
	if e != nil {
		return nil
	}
	st := wire.Wrap(raw)
	m, e := st.Recv()
	if e != nil {
		return nil
	}
	var h api.Hello
	if wire.Decode(m, &h) != nil || m.Kind != "hello" || h.Version != api.Version || m.Target != binding.Target || (binding.Role != RoleEither && h.Role != binding.Role) || len(m.AccessContext) != 0 {
		st.Fail("HANDSHAKE", fmt.Errorf("invalid version or target"))
		return nil
	}
	// A shared standalone credential never admits the peer role.
	if h.Role == RolePeer {
		if binding.Role != RolePeer || h.PeerSource != binding.PeerBootID || h.PeerOwner != g.bootID || h.PeerSource == g.bootID {
			st.Fail("HANDSHAKE", fmt.Errorf("invalid authenticated peer identity"))
			return nil
		}
	} else if h.PeerSource != "" || h.PeerOwner != "" {
		st.Fail("HANDSHAKE", fmt.Errorf("peer identity on non-peer connection"))
		return nil
	}
	timer.Stop()
	defer st.Close()
	if h.Role == "daemon" {
		closed := g.observeConnection(binding.Target, h.Role, m.Incarnation, m.ConnectionGeneration)
		defer closed()
		return g.serveDaemon(ctx, s, st, m, binding)
	}
	if h.Role != RoleSDK && h.Role != RolePeer {
		st.Fail("HANDSHAKE", fmt.Errorf("invalid role"))
		return nil
	}
	g.mu.Lock()
	r := g.routes[binding.Target]
	g.mu.Unlock()
	if r == nil || r.s.IsClosed() || !r.inputAlive() {
		if h.Role == RolePeer {
			st.Fail("ROUTE_STALE", ErrRouteStale)
			return nil
		}
		if g.dialPeer == nil {
			st.Fail("OFFLINE", fmt.Errorf("fabricd offline"))
			return nil
		}
		r, e = g.connectPeer(ctx, binding.Target)
		if e != nil {
			st.Fail("ROUTE_STALE", e)
			return nil
		}
		r.admission = binding.Admission
		defer r.s.Close()
	}
	if h.Role == RolePeer && (r.owner == nil || m.Incarnation != r.b.Incarnation || m.ConnectionGeneration != r.b.Generation || m.RouteEpoch != r.b.RouteEpoch) {
		st.Fail("ROUTE_STALE", ErrRouteStale)
		return nil
	}
	// The fixed owner closing also closes idle peer connections, not just streams.
	stopRoute := context.AfterFunc(r.ctx, func() { s.Close() })
	defer stopRoute()
	if st.Send(&pb.Message{Kind: "welcome", Payload: api.Payload(r.b)}) != nil {
		return nil
	}
	closed := g.observeConnection(binding.Target, h.Role, r.b.Incarnation, r.b.Generation)
	defer closed()
	go func() { _, _ = st.Recv(); s.Close() }()
	sem := make(chan struct{}, wire.MaxStreams)
	for {
		raw, e := s.AcceptStream()
		if e != nil {
			return nil
		}
		select {
		case sem <- struct{}{}:
		default:
			g.emit(observe.Event{Name: observe.GatewayBackpressure, Outcome: "connection_stream_limit", Target: binding.Target, Role: h.Role})
			raw.Close()
			continue
		}
		if !g.acquireStream(binding.Target) {
			<-sem
			raw.Close()
			continue
		}
		streamBinding := binding
		streamBinding.Role = h.Role
		go func() {
			defer func() { <-sem; g.releaseStream(binding.Target) }()
			g.forward(ctx, wire.Wrap(raw), r, streamBinding, handler)
		}()
	}
}
