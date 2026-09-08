package gateway

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type drainCleanupHandler struct {
	ownedHandler
	entered, release chan struct{}
}

func (h *drainCleanupHandler) Open(_ context.Context, _ *pb.Message, s *Stream) (StreamHandler, error) {
	return h, s.Forward()
}

func (h *drainCleanupHandler) Closed(error) {
	close(h.entered)
	<-h.release
}

func TestDrainWaitsForApplicationCleanup(t *testing.T) {
	h := &drainCleanupHandler{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(h.release) }) }
	defer release()
	f := newPeerFixture(t, h)
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
	defer remote.Close()
	remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := remote.Recv(); err != nil {
		t.Fatal(err)
	}
	done := f.owner.Drain()
	request.Close()
	select {
	case <-h.entered:
	case <-f.ctx.Done():
		t.Fatal("application cleanup not invoked")
	}
	f.owner.Close()
	select {
	case <-done:
		t.Fatal("Close reported drain before callback returned")
	default:
	}
	release()
	select {
	case <-done:
	case <-f.ctx.Done():
		t.Fatal("completed cleanup retained stream")
	}
}

func TestDrainPreservesAcceptedPeerWork(t *testing.T) {
	for _, side := range []string{"entry", "owner"} {
		t.Run(side, func(t *testing.T) {
			f := newPeerFixture(t, peerHandler{open: func(_ context.Context, _ *pb.Message, s *Stream) (StreamHandler, error) {
				return ownedHandler{}, s.Forward()
			}})
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
			defer remote.Close()
			remote.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := remote.Recv(); err != nil {
				t.Fatal(err)
			}
			core := f.entry
			if side == "owner" {
				core = f.owner
			}
			done := core.Drain()
			if core.Drain() != done || core.Status().Accepting || core.Status().Streams != 1 {
				t.Fatal("incorrect drain state", core.Status())
			}
			select {
			case <-done:
				t.Fatal("drain completed with an active stream")
			default:
			}
			left, right := net.Pipe()
			defer left.Close()
			if err := core.ServeConn(f.ctx, right, BindingContext{Target: "machine", Role: RoleSDK}, ownedHandler{}); err == nil {
				t.Fatal("new connection admitted during drain")
			}
			// The original stream continues in both directions through both cores.
			if err := request.Send(&pb.Message{Kind: "input", Data: []byte("finish")}); err != nil {
				t.Fatal(err)
			}
			if m, err := remote.Recv(); err != nil || string(m.GetData()) != "finish" {
				t.Fatal("accepted input interrupted", m, err)
			}
			if err := remote.Send(&pb.Message{Kind: "result", Data: []byte("complete")}); err != nil {
				t.Fatal(err)
			}
			if m, err := request.Recv(); err != nil || string(m.GetData()) != "complete" {
				t.Fatal("accepted response interrupted", m, err)
			}
			request.Close()
			remote.Close()
			select {
			case <-done:
			case <-f.ctx.Done():
				t.Fatal("idle connections blocked drain")
			}
			// An existing SDK connection cannot open another business stream.
			raw, err = client.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			rejected := wire.Wrap(raw)
			defer rejected.Close()
			rejected.SetReadDeadline(time.Now().Add(time.Second))
			_ = rejected.Send(&pb.Message{Kind: "request", RequestId: "late", Operation: "runtime.attach", Target: f.binding.Target, Incarnation: f.binding.Incarnation, ConnectionGeneration: f.binding.Generation, RouteEpoch: f.binding.RouteEpoch, RouteRecovery: f.binding.RouteRecovery})
			// Entry drain closes the stream; owner drain may return an explicit
			// transport error through the entry. Neither can reach fabric.
			if m, err := rejected.Recv(); err == nil && m.Kind != "error" {
				t.Fatal("late stream succeeded", m)
			}
			if core.Status().Streams != 0 || f.daemon.NumStreams() != 1 || f.dials.Load() != 1 {
				t.Fatal("drain forwarded or replayed new work", core.Status())
			}
			core.Close()
		})
	}
}
