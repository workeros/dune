package gateway

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

// This directory only arranges protocol fixtures. PostgreSQL's ownership
// semantics are covered separately by metadata and fabricd integration tests.
type peerDirectory struct {
	directoryStub
	mu sync.Mutex
	r  RouteLease
}

func (d *peerDirectory) Resolve(context.Context, string) (RouteLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.r.Epoch == 0 {
		return RouteLease{}, ErrRouteNotFound
	}
	r := d.r
	r.Route = cloneRoute(r.Route)
	r.ValidFor = max(0, time.Until(r.ExpiresAt))
	return r, nil
}

type peerHandler struct {
	ownedHandler
	open func(context.Context, *pb.Message, *Stream) (StreamHandler, error)
}

func (h peerHandler) Open(ctx context.Context, m *pb.Message, s *Stream) (StreamHandler, error) {
	return h.open(ctx, m, s)
}

type peerFixture struct {
	ctx            context.Context
	directory      *peerDirectory
	entry, owner   *Gateway
	daemon         *yamux.Session
	binding        api.Binding
	dials, redials atomic.Int32
}

func newPeerFixture(t *testing.T, ownerHandler ConnectionHandler) *peerFixture {
	t.Helper()
	f := &peerFixture{directory: &peerDirectory{}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	f.ctx = ctx
	t.Cleanup(cancel)
	d := f.directory
	d.acquire = func(_ context.Context, claim RouteClaim, epoch uint64) (RouteLease, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.r = RouteLease{Route: Route{RouteClaim: claim, Epoch: epoch + 1, ExpiresAt: time.Now().Add(ownerLeaseLimit)}, ValidFor: ownerLeaseLimit}
		return d.r, nil
	}
	d.publish = func(context.Context, Route) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.r.Published = true
		return nil
	}
	recovery := wire.ID()
	var err error
	f.owner, err = NewWithPeers(d, "https://owner.test/peer", recovery, func(context.Context, string, Route) (net.Conn, error) {
		f.redials.Add(1)
		return nil, errors.New("peer must not relay")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.owner.Close)
	f.entry, err = NewWithPeers(d, "https://entry.test/peer", recovery, func(_ context.Context, source string, destination Route) (net.Conn, error) {
		f.dials.Add(1)
		left, right := net.Pipe()
		go func() {
			_ = f.owner.ServeConn(ctx, right, BindingContext{Target: destination.Target, Role: RolePeer, PeerBootID: source}, ownerHandler)
		}()
		return left, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.entry.Close)
	f.daemon = f.session(t, f.owner, BindingContext{Target: "machine", Role: RoleDaemon}, ownedHandler{})
	control, welcome, err := wire.Handshake(f.daemon, &pb.Message{Kind: "hello", InputLeaseId: wire.ID(), Target: "machine", Incarnation: "boot", ConnectionGeneration: 1,
		Payload: api.Payload(api.Hello{Version: api.Version, Role: RoleDaemon}), Data: api.Payload(api.Binding{Capabilities: []string{"exec"}, Limits: map[string]int{"streams": 64}})})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.Decode(welcome, &f.binding); err != nil {
		t.Fatal(err)
	}
	if err := control.Send(&pb.Message{Kind: "lease_ready", InputLeaseId: welcome.InputLeaseId, RouteRecovery: f.binding.RouteRecovery, RouteEpoch: f.binding.RouteEpoch}); err != nil {
		t.Fatal(err)
	}
	for !f.owner.Online("machine") {
		select {
		case <-ctx.Done():
			t.Fatal("owner not published")
		case <-time.After(time.Millisecond):
		}
	}
	return f
}

func (f *peerFixture) session(t *testing.T, g *Gateway, binding BindingContext, handler ConnectionHandler) *yamux.Session {
	t.Helper()
	left, right := net.Pipe()
	go func() { _ = g.ServeConn(f.ctx, right, binding, handler) }()
	s, err := yamux.Client(left, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func (f *peerFixture) client(t *testing.T, handler ConnectionHandler) (*yamux.Session, error) {
	t.Helper()
	s := f.session(t, f.entry, BindingContext{Target: "machine", Role: RoleSDK}, handler)
	_, _, err := wire.Handshake(s, &pb.Message{Kind: "hello", Target: "machine", Payload: api.Payload(api.Hello{Version: api.Version, Role: RoleSDK})})
	return s, err
}

func (f *peerFixture) request(t *testing.T, s *yamux.Session, accessContext []byte) *wire.Stream {
	t.Helper()
	raw, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	st := wire.Wrap(raw)
	t.Cleanup(func() { st.Close() })
	_ = st.SetReadDeadline(time.Now().Add(2 * time.Second))
	err = st.Send(&pb.Message{Kind: "request", RequestId: "request-1", Operation: "runtime.attach", Target: f.binding.Target,
		Incarnation: f.binding.Incarnation, ConnectionGeneration: f.binding.Generation, RouteRecovery: f.binding.RouteRecovery, RouteEpoch: f.binding.RouteEpoch, AccessContext: accessContext})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func delegateHandler() ConnectionHandler {
	return peerHandler{open: func(_ context.Context, _ *pb.Message, s *Stream) (StreamHandler, error) {
		route, remote := s.PeerRoute()
		if !remote {
			return nil, errors.New("expected remote route")
		}
		// Application snapshots and buffers cannot mutate core's fixed route.
		route.Binding.Limits["streams"] = 999
		route.Binding.Capabilities[0] = "changed"
		fixed, _ := s.PeerRoute()
		if fixed.Binding.Limits["streams"] != 64 || fixed.Binding.Capabilities[0] != "exec" {
			return nil, errors.New("application mutated fixed route")
		}
		value := []byte("test-only-application-context")
		err := s.ForwardPeer(value)
		value[0] = 'X'
		return ownedHandler{}, err
	}}
}

func TestPeerSingleHopAndContextBoundary(t *testing.T) {
	var checked atomic.Int32
	ownerHandler := peerHandler{open: func(_ context.Context, m *pb.Message, s *Stream) (StreamHandler, error) {
		if string(m.AccessContext) != "test-only-application-context" || m.RequestId != "request-1" || m.Operation != "runtime.attach" {
			return nil, errors.New("invalid request context")
		}
		if _, remote := s.PeerRoute(); remote {
			return nil, errors.New("nested peer route")
		}
		if err := s.ForwardPeer([]byte("third-hop")); err == nil {
			return nil, errors.New("nested forwarding admitted")
		}
		checked.Add(1)
		return ownedHandler{}, s.Forward()
	}}
	f := newPeerFixture(t, ownerHandler)
	client, err := f.client(t, delegateHandler())
	if err != nil {
		t.Fatal(err)
	}
	request := f.request(t, client, nil)
	raw, err := f.daemon.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	fabric := wire.Wrap(raw)
	defer fabric.Close()
	_ = fabric.SetReadDeadline(time.Now().Add(2 * time.Second))
	m, err := fabric.Recv()
	if err != nil || len(m.AccessContext) != 0 || !wire.ValidID(m.InputLeaseId) || m.RouteEpoch != f.binding.RouteEpoch {
		t.Fatal("owner did not remove context and stamp its input grant", m, err)
	}
	if err := fabric.Send(&pb.Message{Kind: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if reply, err := request.Recv(); err != nil || reply.Kind != "accepted" {
		t.Fatal(reply, err)
	}
	if err := request.Send(&pb.Message{Kind: "input", Data: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	if input, err := fabric.Recv(); err != nil || string(input.Data) != "hello" || len(input.AccessContext) != 0 || !wire.ValidID(input.InputLeaseId) {
		t.Fatal(input, err)
	}
	// A failure after admission must close the original path, never select or
	// redial a replacement and resend the request or terminal input.
	f.owner.Close()
	select {
	case <-client.CloseChan():
	case <-time.After(2 * time.Second):
		t.Fatal("idle peer client outlived owner")
	}
	if f.dials.Load() != 1 || f.redials.Load() != 0 || checked.Load() != 1 {
		t.Fatal("request or peer dial repeated")
	}
}

func TestPeerRequiresIndependentRequestAuthorization(t *testing.T) {
	for _, mode := range []string{"owner-denies", "entry-denies", "missing-context", "sdk-injects", "oversize-context"} {
		t.Run(mode, func(t *testing.T) {
			var checked atomic.Int32
			f := newPeerFixture(t, peerHandler{open: func(context.Context, *pb.Message, *Stream) (StreamHandler, error) {
				checked.Add(1)
				return nil, errors.New("user revoked at owner")
			}})
			entry := delegateHandler()
			switch mode {
			case "missing-context":
				entry = ownedHandler{}
			case "entry-denies":
				entry = peerHandler{open: func(context.Context, *pb.Message, *Stream) (StreamHandler, error) {
					return nil, errors.New("denied at entry")
				}}
			case "oversize-context":
				entry = peerHandler{open: func(_ context.Context, _ *pb.Message, s *Stream) (StreamHandler, error) {
					return ownedHandler{}, s.ForwardPeer(make([]byte, MaxAccessContext+1))
				}}
			}
			client, err := f.client(t, entry)
			if err != nil {
				t.Fatal(err)
			}
			var injected []byte
			if mode == "sdk-injects" {
				injected = []byte("forged")
			}
			st := f.request(t, client, injected)
			if reply, err := st.Recv(); err != nil || reply.Code != "ACCESS_DENIED" {
				t.Fatal(reply, err)
			}
			if f.daemon.NumStreams() != 1 {
				t.Fatal("denied request reached fabricd")
			}
			expected := int32(0)
			if mode == "owner-denies" {
				expected = 1
			}
			if checked.Load() != expected {
				t.Fatal("wrong authorization entry reached", checked.Load())
			}
		})
	}
}

func TestPeerStaleDestinationNeverRedirects(t *testing.T) {
	for _, mode := range []string{"boot", "epoch", "recovery", "unpublished", "local-missing"} {
		t.Run(mode, func(t *testing.T) {
			f := newPeerFixture(t, ownedHandler{})
			f.directory.mu.Lock()
			switch mode {
			case "boot":
				f.directory.r.OwnerBootID = wire.ID()
			case "epoch":
				f.directory.r.Epoch++
			case "recovery":
				f.directory.r.RecoveryGeneration = wire.ID()
			case "unpublished":
				f.directory.r.Published = false
			case "local-missing":
				f.owner.mu.Lock()
				delete(f.owner.routes, "machine")
				f.owner.mu.Unlock()
			}
			f.directory.mu.Unlock()
			_, err := f.client(t, delegateHandler())
			var failure *api.Error
			if !errors.As(err, &failure) || failure.Code != "ROUTE_STALE" {
				t.Fatal("stale route admitted", err)
			}
			if f.redials.Load() != 0 || f.dials.Load() > 1 {
				t.Fatal("owner redirected or entry retried")
			}
			if f.daemon.NumStreams() != 1 {
				t.Fatal("stale route reached fabricd")
			}
		})
	}
}

func TestPeerIdentityMustMatchAuthenticatedTransport(t *testing.T) {
	f := newPeerFixture(t, ownedHandler{})
	for _, mode := range []string{"source", "either", "sdk"} {
		t.Run(mode, func(t *testing.T) {
			binding := BindingContext{Target: "machine", Role: RolePeer, PeerBootID: f.entry.BootID()}
			if mode != "source" {
				binding.Role, binding.PeerBootID = mode, ""
			}
			s := f.session(t, f.owner, binding, ownedHandler{})
			_, _, err := wire.Handshake(s, &pb.Message{Kind: "hello", Target: "machine", Incarnation: f.binding.Incarnation, ConnectionGeneration: f.binding.Generation,
				RouteRecovery: f.binding.RouteRecovery, RouteEpoch: f.binding.RouteEpoch, Payload: api.Payload(api.Hello{Version: api.Version, Role: RolePeer, PeerOwner: f.owner.BootID(), PeerSource: wire.ID()})})
			var failure *api.Error
			if !errors.As(err, &failure) || failure.Code != "HANDSHAKE" {
				t.Fatal("unauthenticated peer accepted", err)
			}
		})
	}
}

func TestPeerOwnerRejectsMissingOrOversizeContext(t *testing.T) {
	var checked atomic.Int32
	f := newPeerFixture(t, peerHandler{open: func(context.Context, *pb.Message, *Stream) (StreamHandler, error) {
		checked.Add(1)
		return nil, errors.New("must reject before application")
	}})
	s := f.session(t, f.owner, BindingContext{Target: "machine", Role: RolePeer, PeerBootID: f.entry.BootID()}, peerHandler{open: func(context.Context, *pb.Message, *Stream) (StreamHandler, error) {
		checked.Add(1)
		return nil, errors.New("must reject before application")
	}})
	_, _, err := wire.Handshake(s, &pb.Message{Kind: "hello", Target: "machine", Incarnation: f.binding.Incarnation, ConnectionGeneration: f.binding.Generation,
		RouteRecovery: f.binding.RouteRecovery, RouteEpoch: f.binding.RouteEpoch, Payload: api.Payload(api.Hello{Version: api.Version, Role: RolePeer, PeerOwner: f.owner.BootID(), PeerSource: f.entry.BootID()})})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{nil, make([]byte, MaxAccessContext+1)} {
		st := f.request(t, s, value)
		if reply, err := st.Recv(); err != nil || reply.Code != "ACCESS_DENIED" {
			t.Fatal(reply, err)
		}
	}
	if checked.Load() != 0 || f.daemon.NumStreams() != 1 {
		t.Fatal("invalid context reached application or fabricd")
	}
}

func TestPeerContextCannotChangeWithinStream(t *testing.T) {
	f := newPeerFixture(t, ownedHandler{})
	client, err := f.client(t, delegateHandler())
	if err != nil {
		t.Fatal(err)
	}
	st := f.request(t, client, nil)
	raw, err := f.daemon.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	fabric := wire.Wrap(raw)
	defer fabric.Close()
	_ = fabric.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := fabric.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Send(&pb.Message{Kind: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&pb.Message{Kind: "input", Data: []byte("forbidden"), AccessContext: []byte("replacement")}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.Recv(); err == nil {
		t.Fatal("replacement context reached execution")
	}
	if f.dials.Load() != 1 {
		t.Fatal("rejected input triggered redial")
	}
}

func TestPeerOwnerRechecksRouteBeforeApplication(t *testing.T) {
	var checked atomic.Int32
	f := newPeerFixture(t, peerHandler{open: func(context.Context, *pb.Message, *Stream) (StreamHandler, error) {
		checked.Add(1)
		return nil, errors.New("stale route reached application")
	}})
	client, err := f.client(t, delegateHandler())
	if err != nil {
		t.Fatal(err)
	}
	f.owner.mu.Lock()
	delete(f.owner.routes, "machine")
	f.owner.mu.Unlock()
	st := f.request(t, client, nil)
	if reply, err := st.Recv(); err != nil || reply.Code != "ROUTE_STALE" {
		t.Fatal(reply, err)
	}
	if checked.Load() != 0 || f.daemon.NumStreams() != 1 || f.dials.Load() != 1 {
		t.Fatal("stale route was checked, forwarded or retried")
	}
}
