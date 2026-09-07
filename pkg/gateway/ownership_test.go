package gateway

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

type directoryStub struct {
	acquire func(context.Context, RouteClaim, uint64) (RouteLease, error)
	publish func(context.Context, Route) error
	renew   func(context.Context, Route) (RouteLease, error)
	release func(context.Context, Route) error
}

func (d directoryStub) Resolve(context.Context, string) (RouteLease, error) {
	return RouteLease{}, ErrRouteNotFound
}
func (d directoryStub) Acquire(ctx context.Context, claim RouteClaim, epoch uint64) (RouteLease, error) {
	return d.acquire(ctx, claim, epoch)
}
func (d directoryStub) Publish(ctx context.Context, route Route) error {
	if d.publish != nil {
		return d.publish(ctx, route)
	}
	return nil
}
func (d directoryStub) Renew(ctx context.Context, route Route) (RouteLease, error) {
	return d.renew(ctx, route)
}
func (d directoryStub) Release(ctx context.Context, route Route) error {
	if d.release != nil {
		return d.release(ctx, route)
	}
	return nil
}

type ownedHandler struct{}

func (ownedHandler) Connected(context.Context, *Connection) error { return nil }
func (ownedHandler) Open(_ context.Context, _ *pb.Message, stream *Stream) (StreamHandler, error) {
	return ownedHandler{}, stream.Forward()
}
func (ownedHandler) Message(context.Context, Direction, *pb.Message) error { return nil }
func (ownedHandler) Closed(error)                                          {}

func TestOwnershipDeadlineCountsDirectoryDelay(t *testing.T) {
	for _, delay := range []time.Duration{30 * time.Millisecond, 100 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			var calls atomic.Int32
			directory := directoryStub{acquire: func(_ context.Context, claim RouteClaim, epoch uint64) (RouteLease, error) {
				calls.Add(1)
				time.Sleep(delay)
				return RouteLease{Route: Route{RouteClaim: claim, Epoch: epoch + 1, ExpiresAt: time.Now().Add(100 * time.Hour)}, ValidFor: 80 * time.Millisecond}, nil
			}}
			g, err := NewWithDirectory(directory, "https://instance.test/peer", wire.ID())
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			owner, err := g.acquireOwner(context.Background(), api.Binding{Target: "machine", Version: api.Version, Incarnation: wire.ID(), Generation: 1})
			if delay > 80*time.Millisecond {
				if !errors.Is(err, ErrRouteStale) || owner != nil {
					t.Fatal("late response revived expired ownership", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if remaining := owner.remaining(); remaining > 55*time.Millisecond {
					t.Fatal("response receipt extended ownership", remaining)
				}
				closed := make(chan struct{})
				go owner.watch(context.Background(), func() { close(closed) })
				select {
				case <-closed:
				case <-time.After(100 * time.Millisecond):
					t.Fatal("idle ownership outlived local deadline")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("ownership acquisition replayed")
			}
		})
	}
}

func TestOwnershipRenewalCannotReviveExpiredTerm(t *testing.T) {
	var calls atomic.Int32
	directory := directoryStub{renew: func(_ context.Context, route Route) (RouteLease, error) {
		calls.Add(1)
		// Deliberately model a response delivered after cancellation. Even a
		// successful database renewal cannot revive local execution permission.
		time.Sleep(80 * time.Millisecond)
		return RouteLease{Route: route, ValidFor: ownerLeaseLimit}, nil
	}}
	owner := &ownership{directory: directory, route: Route{Epoch: 1}, until: time.Now().Add(30 * time.Millisecond), changed: make(chan struct{}, 1)}
	if err := owner.renew(context.Background()); !errors.Is(err, ErrRouteStale) || owner.remaining() > 0 {
		t.Fatal("late renewal revived local term", err)
	}
	if err := owner.renew(context.Background()); !errors.Is(err, ErrRouteStale) || calls.Load() != 1 {
		t.Fatal("expired ownership performed another write", err)
	}
}

func TestOwnedHandshakeRequiresEpochConfirmation(t *testing.T) {
	for _, correct := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong-epoch", true: "confirmed"}[correct], func(t *testing.T) {
			var published, released atomic.Int32
			directory := directoryStub{
				acquire: func(_ context.Context, claim RouteClaim, _ uint64) (RouteLease, error) {
					return RouteLease{Route: Route{RouteClaim: claim, Epoch: 7}, ValidFor: ownerLeaseLimit}, nil
				},
				publish: func(context.Context, Route) error { published.Add(1); return nil },
				release: func(context.Context, Route) error { released.Add(1); return nil },
			}
			g, err := NewWithDirectory(directory, "https://instance.test/peer", wire.ID())
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			left, right := net.Pipe()
			done := make(chan struct{})
			go func() {
				_ = g.ServeConn(ctx, right, BindingContext{Target: "machine", Role: RoleDaemon}, ownedHandler{})
				close(done)
			}()
			peer, err := yamux.Client(left, wire.Config())
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			control, welcome, err := wire.Handshake(peer, &pb.Message{Kind: "hello", InputLeaseId: wire.ID(), Target: "machine", Incarnation: wire.ID(), ConnectionGeneration: 1, Payload: api.Payload(api.Hello{Version: api.Version, Role: RoleDaemon}), Data: api.Payload(api.Binding{})})
			if err != nil {
				t.Fatal(err)
			}
			var binding api.Binding
			if err := wire.Decode(welcome, &binding); err != nil {
				t.Fatal(err)
			}
			if binding.RouteRecovery != g.recovery || binding.RouteEpoch != 7 || welcome.InputLeaseMs == 0 || welcome.InputLeaseMs >= 15000 {
				t.Fatal("ownership missing or input grant exceeds conservative owner time", welcome)
			}
			if published.Load() != 0 || g.Online("machine") {
				t.Fatal("owner published before fabricd confirmation")
			}
			epoch := uint64(8)
			if correct {
				epoch = 7
			}
			if err := control.Send(&pb.Message{Kind: "lease_ready", InputLeaseId: welcome.InputLeaseId, RouteRecovery: g.recovery, RouteEpoch: epoch}); err != nil {
				t.Fatal(err)
			}
			if correct {
				for !g.Online("machine") {
					select {
					case <-ctx.Done():
						t.Fatal("confirmed owner not published")
					case <-time.After(time.Millisecond):
					}
				}
				if published.Load() != 1 {
					t.Fatal("confirmation did not publish exactly once")
				}
			} else {
				failure, err := control.Recv()
				if err != nil || failure.Code != "HANDSHAKE" {
					t.Fatal("wrong epoch accepted", failure, err)
				}
				if published.Load() != 0 || g.Online("machine") {
					t.Fatal("wrong epoch published")
				}
			}
			peer.Close()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("ownership cleanup blocked")
			}
			if released.Load() != 1 {
				t.Fatal("owner not released exactly once")
			}
		})
	}
}

type ownedLocalHandler struct {
	ownedHandler
	opened chan *Stream
}

func (h ownedLocalHandler) Open(_ context.Context, _ *pb.Message, stream *Stream) (StreamHandler, error) {
	h.opened <- stream
	return h, stream.Send(&pb.Message{Kind: "accepted"})
}

func TestOwnerExpiryClosesLocallyHandledStream(t *testing.T) {
	directory := directoryStub{acquire: func(_ context.Context, claim RouteClaim, _ uint64) (RouteLease, error) {
		return RouteLease{Route: Route{RouteClaim: claim, Epoch: 1}, ValidFor: 500 * time.Millisecond}, nil
	}}
	g, err := NewWithDirectory(directory, "https://instance.test/peer", wire.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connect := func(role string, handler ConnectionHandler) (*yamux.Session, *wire.Stream, *pb.Message) {
		t.Helper()
		left, right := net.Pipe()
		go func() { _ = g.ServeConn(ctx, right, BindingContext{Target: "machine", Role: role}, handler) }()
		peer, err := yamux.Client(left, wire.Config())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { peer.Close() })
		control, welcome, err := wire.Handshake(peer, &pb.Message{Kind: "hello", InputLeaseId: wire.ID(), Target: "machine", Incarnation: "boot", ConnectionGeneration: 1, Payload: api.Payload(api.Hello{Version: api.Version, Role: role}), Data: api.Payload(api.Binding{})})
		if err != nil {
			t.Fatal(err)
		}
		return peer, control, welcome
	}
	_, control, welcome := connect(RoleDaemon, ownedHandler{})
	if err := control.Send(&pb.Message{Kind: "lease_ready", InputLeaseId: welcome.InputLeaseId, RouteRecovery: g.recovery, RouteEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	for !g.Online("machine") {
		select {
		case <-ctx.Done():
			t.Fatal("owner not published")
		case <-time.After(time.Millisecond):
		}
	}
	opened := make(chan *Stream, 1)
	peer, _, _ := connect(RoleSDK, ownedLocalHandler{opened: opened})
	raw, err := peer.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	request := wire.Wrap(raw)
	defer request.Close()
	if err := request.Send(&pb.Message{Kind: "request", RequestId: wire.ID(), Operation: "runtime.attach", Target: "machine", Incarnation: "boot", ConnectionGeneration: 1, RouteRecovery: g.recovery, RouteEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	var flow *Stream
	select {
	case flow = <-opened:
	case <-ctx.Done():
		t.Fatal("local handler not reached")
	}
	select {
	case <-flow.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("local stream outlived execution ownership")
	}
	if g.Online("machine") {
		t.Fatal("expired owner remained online")
	}
}
