// Package gateway terminates two independent Yamux sessions and relays bounded
// application messages. It never executes commands or retries requests.
package gateway

import (
	"context"
	"fmt"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
	"log"
	"net"
	"sync"
	"time"
)

type route struct {
	s *yamux.Session
	b api.Binding
}

type Gateway struct {
	mu       sync.Mutex
	closed   bool
	routes   map[string]*route
	sessions map[*yamux.Session]BindingContext
	slots    chan struct{}
	streams  chan struct{}
}

func New() *Gateway {
	return &Gateway{routes: map[string]*route{}, sessions: map[*yamux.Session]BindingContext{}, slots: make(chan struct{}, 256), streams: make(chan struct{}, 512)}
}

func (g *Gateway) Online(target string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.routes[target]
	return r != nil && !r.s.IsClosed()
}

func (g *Gateway) Disconnect(target string) {
	g.mu.Lock()
	var closeSessions []*yamux.Session
	for sess, binding := range g.sessions {
		if binding.Target == target {
			closeSessions = append(closeSessions, sess)
		}
	}
	g.mu.Unlock()
	for _, sess := range closeSessions {
		sess.Close()
	}
}

func (g *Gateway) Close() {
	g.mu.Lock()
	g.closed = true
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
	if handler == nil || binding.Target == "" || (binding.Role != RoleSDK && binding.Role != RoleDaemon && binding.Role != RoleEither) {
		return fmt.Errorf("binding and connection handler required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case g.slots <- struct{}{}:
	default:
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
	if g.closed {
		g.mu.Unlock()
		return fmt.Errorf("gateway closed")
	}
	g.sessions[s] = binding
	g.mu.Unlock()
	defer func() { g.mu.Lock(); delete(g.sessions, s); g.mu.Unlock() }()
	go func() {
		select {
		case <-s.CloseChan():
			cancel(context.Canceled)
		case <-ctx.Done():
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
	if wire.Decode(m, &h) != nil || m.Kind != "hello" || h.Version != api.Version || m.Target != binding.Target || (binding.Role != RoleEither && h.Role != binding.Role) {
		st.Fail("HANDSHAKE", fmt.Errorf("invalid version or target"))
		return nil
	}
	timer.Stop()
	defer st.Close()
	if h.Role == "daemon" {
		if m.Incarnation == "" || m.ConnectionGeneration == 0 {
			st.Fail("HANDSHAKE", fmt.Errorf("missing incarnation/generation"))
			return nil
		}
		var b api.Binding
		if len(m.Data) == 0 {
			return nil
		}
		if e := jsonBinding(m.Data, &b); e != nil {
			return nil
		}
		b.Target = binding.Target
		b.Incarnation = m.Incarnation
		b.Generation = m.ConnectionGeneration
		b.Version = api.Version
		r := &route{s: s, b: b}
		g.mu.Lock()
		old := g.routes[binding.Target]
		g.routes[binding.Target] = r
		g.mu.Unlock()
		if old != nil {
			old.s.Close()
		}
		defer func() {
			g.mu.Lock()
			if g.routes[binding.Target] == r {
				delete(g.routes, binding.Target)
			}
			g.mu.Unlock()
		}()
		if st.Send(&pb.Message{Kind: "welcome", Payload: api.Payload(b)}) != nil {
			return nil
		}
		log.Printf("fabricd registered incarnation=%s generation=%d", b.Incarnation, b.Generation)
		go func() {
			for {
				extra, e := s.AcceptStream()
				if e != nil {
					return
				}
				extra.Close()
			}
		}()
		_, _ = st.Recv()
		return nil
	}
	if h.Role != "sdk" {
		st.Fail("HANDSHAKE", fmt.Errorf("invalid role"))
		return nil
	}
	g.mu.Lock()
	r := g.routes[binding.Target]
	g.mu.Unlock()
	if r == nil || r.s.IsClosed() {
		st.Fail("OFFLINE", fmt.Errorf("fabricd offline"))
		return nil
	}
	if st.Send(&pb.Message{Kind: "welcome", Payload: api.Payload(r.b)}) != nil {
		return nil
	}
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
			raw.Close()
			continue
		}
		select {
		case g.streams <- struct{}{}:
		default:
			<-sem
			raw.Close()
			continue
		}
		go func() { defer func() { <-sem; <-g.streams }(); g.forward(ctx, wire.Wrap(raw), r, binding, handler) }()
	}
}
