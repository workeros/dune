package gateway

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/observe"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type observedGatewayEvents struct {
	mu     sync.Mutex
	events []observe.Event
}

func (e *observedGatewayEvents) add(event observe.Event) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *observedGatewayEvents) wait(t *testing.T, match func(observe.Event) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.mu.Lock()
		found := false
		for _, event := range e.events {
			found = found || match(event)
		}
		e.mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway event was not observed")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGatewayEmitsPeerAndBackpressureEvents(t *testing.T) {
	t.Run("peer", func(t *testing.T) {
		f := newPeerFixture(t, peerHandler{open: func(_ context.Context, _ *pb.Message, stream *Stream) (StreamHandler, error) {
			return ownedHandler{}, stream.Forward()
		}})
		events := &observedGatewayEvents{}
		f.entry.SetObserver(events.add)
		client, err := f.client(t, delegateHandler())
		if err != nil {
			t.Fatal(err)
		}
		request := f.request(t, client, nil)
		raw, err := f.daemon.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		remote := wire.Wrap(raw)
		remote.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := remote.Recv(); err != nil {
			t.Fatal(err)
		}
		request.Close()
		remote.Close()
		select {
		case <-f.entry.Drain():
		case <-f.ctx.Done():
			t.Fatal("peer stream did not finish")
		}
		events.wait(t, func(event observe.Event) bool {
			return event.Name == observe.GatewayPeerDial && event.Outcome == "connected" && event.Route == "peer" && event.OwnerID == f.owner.BootID() && event.Epoch > 0
		})
		events.wait(t, func(event observe.Event) bool {
			return event.Name == observe.GatewayStream && event.Route == "peer" && event.Target == f.binding.Target
		})
	})

	t.Run("backpressure", func(t *testing.T) {
		g := New()
		defer g.Close()
		events := &observedGatewayEvents{}
		g.SetObserver(events.add)
		g.slots = make(chan struct{}, 1)
		g.slots <- struct{}{}
		left, right := net.Pipe()
		defer left.Close()
		if err := g.ServeConn(context.Background(), right, BindingContext{Target: "busy", Role: RoleSDK}, ownedHandler{}); err == nil {
			t.Fatal("full session capacity was accepted")
		}
		events.wait(t, func(event observe.Event) bool {
			return event.Name == observe.GatewayBackpressure && event.Outcome == "session_limit" && event.Target == "busy" && event.Role == RoleSDK
		})
	})

	t.Run("observer panic", func(t *testing.T) {
		g := New()
		g.SetObserver(func(observe.Event) { panic("diagnostic failure") })
		g.streams = make(chan struct{})
		if g.acquireStream("busy") {
			t.Fatal("zero stream capacity was accepted")
		}
	})
}
