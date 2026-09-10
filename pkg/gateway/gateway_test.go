package gateway_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

type handler struct {
	open    func(context.Context, *pb.Message, *gateway.Stream) (gateway.StreamHandler, error)
	message func(context.Context, gateway.Direction, *pb.Message) error
	closed  atomic.Int32
	done    chan error
	flow    chan *gateway.Stream
}

func (h *handler) Connected(context.Context, *gateway.Connection) error { return nil }
func (h *handler) Open(ctx context.Context, m *pb.Message, stream *gateway.Stream) (gateway.StreamHandler, error) {
	if h.flow != nil {
		h.flow <- stream
	}
	if h.open != nil {
		return h.open(ctx, m, stream)
	}
	return h, stream.Forward()
}
func (h *handler) Message(ctx context.Context, dir gateway.Direction, m *pb.Message) error {
	if h.message != nil {
		return h.message(ctx, dir, m)
	}
	return nil
}
func (h *handler) Closed(err error) {
	h.closed.Add(1)
	if h.done != nil {
		h.done <- err
	}
}

func session(t *testing.T, ctx context.Context, g *gateway.Gateway, role string, h gateway.ConnectionHandler) *yamux.Session {
	t.Helper()
	local, remote := net.Pipe()
	go func() { _ = g.ServeConn(ctx, remote, gateway.BindingContext{Target: "machine", Role: role}, h) }()
	s, err := yamux.Client(local, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctrl, welcome, err := wire.Handshake(s, &pb.Message{Kind: "hello", InputLeaseId: wire.ID(), Target: "machine", Incarnation: "boot", ConnectionGeneration: 1,
		Payload: api.Payload(api.Hello{Version: api.Version, Role: role}), Data: api.Payload(api.Binding{})})
	if err != nil {
		t.Fatal(err)
	}
	if role == gateway.RoleDaemon {
		if err := ctrl.Send(&pb.Message{Kind: "lease_ready", InputLeaseId: welcome.InputLeaseId}); err != nil {
			t.Fatal(err)
		}
		for !g.Online("machine") {
			select {
			case <-ctx.Done():
				t.Fatal("confirmed route was not published")
			case <-time.After(time.Millisecond):
			}
		}
	}
	return s
}

func fixture(t *testing.T, h gateway.ConnectionHandler) (*yamux.Session, *yamux.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	g := gateway.New()
	t.Cleanup(g.Close)
	_, daemonHandler, err := (access.Grant{Target: "machine", Role: gateway.RoleDaemon}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	daemon := session(t, ctx, g, gateway.RoleDaemon, daemonHandler)
	return session(t, ctx, g, gateway.RoleSDK, h), daemon
}

func request(t *testing.T, s *yamux.Session) *wire.Stream {
	t.Helper()
	raw, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	st := wire.Wrap(raw)
	t.Cleanup(func() { st.Close() })
	_ = st.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := st.Send(&pb.Message{Kind: "request", RequestId: "request-1", Operation: "runtime.attach", Target: "machine", Incarnation: "boot", ConnectionGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestMandatoryHandlerAndConnectionOwnership(t *testing.T) {
	g := gateway.New()
	defer g.Close()
	local, remote := net.Pipe()
	defer local.Close()
	if err := g.ServeConn(context.Background(), remote, gateway.BindingContext{Target: "machine", Role: gateway.RoleSDK}, nil); err == nil {
		t.Fatal("missing handler accepted")
	}
	if _, err := local.Write([]byte("x")); err == nil {
		t.Fatal("failed connection left open")
	}
}

func TestDropClosesCurrentTargetButAllowsReentry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g := gateway.New()
	defer g.Close()
	_, daemonHandler, err := (access.Grant{Target: "machine", Role: gateway.RoleDaemon}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	first := session(t, ctx, g, gateway.RoleDaemon, daemonHandler)
	g.Drop("machine")
	for (g.Online("machine") || !first.IsClosed()) && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	if ctx.Err() != nil || !first.IsClosed() {
		t.Fatal("drop did not close the current target session")
	}
	second := session(t, ctx, g, gateway.RoleDaemon, daemonHandler)
	if second.IsClosed() || !g.Online("machine") {
		t.Fatal("drop permanently blocked a future target session")
	}
}

func TestRequestDeniedBeforeForwarding(t *testing.T) {
	h := &handler{open: func(context.Context, *pb.Message, *gateway.Stream) (gateway.StreamHandler, error) {
		return nil, errors.New("denied")
	}}
	client, daemon := fixture(t, h)
	st := request(t, client)
	m, err := st.Recv()
	if err != nil || m.Code != "ACCESS_DENIED" {
		t.Fatalf("reply = %v, %v", m, err)
	}
	if daemon.NumStreams() != 1 {
		t.Fatal("denied request reached fabric")
	}
}

func TestMessageDeniedAndClosedExactlyOnce(t *testing.T) {
	h := &handler{done: make(chan error, 1), message: func(ctx context.Context, direction gateway.Direction, m *pb.Message) error {
		if direction == gateway.ToFabric && m.Kind == "input" {
			return errors.New("read only")
		}
		return nil
	}}
	client, daemon := fixture(t, h)
	st := request(t, client)
	raw, err := daemon.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	remote := wire.Wrap(raw)
	defer remote.Close()
	_ = remote.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := remote.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := remote.Send(&pb.Message{Kind: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&pb.Message{Kind: "input", Data: []byte("forbidden")}); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Recv(); err == nil {
		t.Fatal("forbidden input reached fabric")
	}
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close")
	}
	st.Close()
	if h.closed.Load() != 1 {
		t.Fatal("duplicate termination notification")
	}
}

func TestIdleStreamCancellation(t *testing.T) {
	h := &handler{done: make(chan error, 1), flow: make(chan *gateway.Stream, 1)}
	client, daemon := fixture(t, h)
	st := request(t, client)
	raw, err := daemon.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	remote := wire.Wrap(raw)
	defer remote.Close()
	_ = remote.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := remote.Recv(); err != nil {
		t.Fatal(err)
	}
	flow := <-h.flow
	flow.Cancel(errors.New("expired"))
	flow.Cancel(errors.New("expired again"))
	if _, err := st.Recv(); err == nil {
		t.Fatal("idle client remains readable")
	}
	if _, err := remote.Recv(); err == nil {
		t.Fatal("idle fabric remains readable")
	}
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("missing termination notification")
	}
	if h.closed.Load() != 1 {
		t.Fatal("duplicate termination notification")
	}
	if err := flow.Send(&pb.Message{Kind: "result"}); err == nil {
		t.Fatal("write after cancellation succeeded")
	}
}

func TestHookTimeoutClosesBlockedStream(t *testing.T) {
	h := &handler{open: func(ctx context.Context, _ *pb.Message, _ *gateway.Stream) (gateway.StreamHandler, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	client, daemon := fixture(t, h)
	st := request(t, client)
	if _, err := st.Recv(); err == nil {
		t.Fatal("timed-out hook left stream open")
	}
	if daemon.NumStreams() != 1 {
		t.Fatal("timed-out request reached fabric")
	}
}

func TestLocallyHandledRequestDoesNotForward(t *testing.T) {
	h := &handler{done: make(chan error, 1)}
	h.open = func(_ context.Context, _ *pb.Message, flow *gateway.Stream) (gateway.StreamHandler, error) {
		return h, flow.Send(&pb.Message{Kind: "result", Data: []byte("local")})
	}
	client, daemon := fixture(t, h)
	st := request(t, client)
	m, err := st.Recv()
	if err != nil || string(m.Data) != "local" {
		t.Fatalf("reply = %v, %v", m, err)
	}
	if daemon.NumStreams() != 1 {
		t.Fatal("local request reached fabric")
	}
	st.Close()
	select {
	case err := <-h.done:
		if err != io.EOF {
			t.Fatalf("close reason = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing local termination notification")
	}
}

func TestDaemonRouteRequiresConfirmedLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g := gateway.New()
	defer g.Close()
	_, daemonHandler, err := (access.Grant{Target: "machine", Role: gateway.RoleDaemon}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	original := session(t, ctx, g, gateway.RoleDaemon, daemonHandler)
	local, remote := net.Pipe()
	go func() {
		_ = g.ServeConn(ctx, remote, gateway.BindingContext{Target: "machine", Role: gateway.RoleDaemon}, daemonHandler)
	}()
	replacement, err := yamux.Client(local, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	control, welcome, err := wire.Handshake(replacement, &pb.Message{Kind: "hello", InputLeaseId: wire.ID(), Target: "machine", Incarnation: "new-boot", ConnectionGeneration: 2,
		Payload: api.Payload(api.Hello{Version: api.Version, Role: gateway.RoleDaemon}), Data: api.Payload(api.Binding{})})
	if err != nil {
		t.Fatal(err)
	}
	if welcome.InputLeaseId == "" || welcome.InputLeaseMs != uint32(wire.InputLeaseDuration/time.Millisecond) {
		t.Fatal("missing bounded input grant", welcome)
	}
	// A welcome is only an offer. Before confirmation, discovery and SDK routing
	// must retain the original connection and the original tunnel stays alive.
	probe, err := func() (*pb.Message, error) {
		left, right := net.Pipe()
		go func() {
			_ = g.ServeConn(ctx, right, gateway.BindingContext{Target: "machine", Role: gateway.RoleSDK}, &handler{})
		}()
		peer, err := yamux.Client(left, wire.Config())
		if err != nil {
			return nil, err
		}
		defer peer.Close()
		_, reply, err := wire.Handshake(peer, &pb.Message{Kind: "hello", Target: "machine", Payload: api.Payload(api.Hello{Version: api.Version, Role: gateway.RoleSDK})})
		return reply, err
	}()
	var selected api.Binding
	if err != nil || wire.Decode(probe, &selected) != nil || selected.Incarnation != "boot" || original.IsClosed() {
		t.Fatal("unconfirmed handshake replaced original route", selected, err)
	}
	if err := control.Send(&pb.Message{Kind: "lease_ready", InputLeaseId: "wrong-challenge"}); err != nil {
		t.Fatal(err)
	}
	_ = control.SetReadDeadline(time.Now().Add(time.Second))
	failure, err := control.Recv()
	if err != nil || failure.Code != "HANDSHAKE" || original.IsClosed() || !g.Online("machine") {
		t.Fatal("invalid confirmation changed original route", failure, err)
	}
}

func TestForwardOverwritesCallerLease(t *testing.T) {
	c, daemon := fixture(t, &handler{})
	stream := request(t, c)
	raw, err := daemon.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	remote := wire.Wrap(raw)
	defer remote.Close()
	_ = remote.SetReadDeadline(time.Now().Add(time.Second))
	first, err := remote.Recv()
	if err != nil || first.InputLeaseId == "" || first.InputLeaseMs != 0 {
		t.Fatal("first request lacks gateway input grant", first, err)
	}
	if err := stream.Send(&pb.Message{Kind: "input", Data: []byte("input"), InputLeaseId: "forged", InputLeaseMs: 999999}); err != nil {
		t.Fatal(err)
	}
	next, err := remote.Recv()
	if err != nil || next.InputLeaseId != first.InputLeaseId || next.InputLeaseMs != 0 {
		t.Fatal("caller selected execution input lease", next, err)
	}
}

func TestHandshakeRejectsOldOrUnleasedProtocol(t *testing.T) {
	for _, attempt := range []struct{ version, role string }{
		{"dune-mvp/1", gateway.RoleDaemon},
		{"dune-mvp/1", gateway.RoleSDK},
		{api.Version, gateway.RoleDaemon},
	} {
		t.Run(attempt.version+"/"+attempt.role, func(t *testing.T) {
			g := gateway.New()
			defer g.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			local, remote := net.Pipe()
			go func() {
				_ = g.ServeConn(ctx, remote, gateway.BindingContext{Target: "machine", Role: attempt.role}, &handler{})
			}()
			peer, err := yamux.Client(local, wire.Config())
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			_, _, err = wire.Handshake(peer, &pb.Message{Kind: "hello", Target: "machine", Incarnation: "boot", ConnectionGeneration: 1,
				Payload: api.Payload(api.Hello{Version: attempt.version, Role: attempt.role}), Data: api.Payload(api.Binding{})})
			var denied *api.Error
			if !errors.As(err, &denied) || denied.Code != "HANDSHAKE" || g.Online("machine") {
				t.Fatal("incompatible handshake reached business routing", err)
			}
		})
	}
}
