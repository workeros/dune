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
	_, _, err = wire.Handshake(s, &pb.Message{Kind: "hello", Target: "machine", Incarnation: "boot", ConnectionGeneration: 1,
		Payload: api.Payload(api.Hello{Version: api.Version, Role: role}), Data: api.Payload(api.Binding{})})
	if err != nil {
		t.Fatal(err)
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
