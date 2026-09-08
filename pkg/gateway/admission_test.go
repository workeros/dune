package gateway

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func TestAdmissionExpiryDoesNotDependOnTimerScheduling(t *testing.T) {
	lease, err := NewAdmissionLease(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	input := wire.NewInputWindow()
	id := wire.ID()
	if err := input.Begin(id); err != nil {
		t.Fatal(err)
	}
	if err := input.Confirm(id, time.Second); err != nil {
		t.Fatal(err)
	}
	// Model a process resumed after expiry, before its timer callback can run.
	lease.mu.Lock()
	lease.timer.Stop()
	lease.until = time.Now().Add(-time.Millisecond)
	lease.mu.Unlock()
	if lease.Remaining() != 0 || lease.Renew(time.Now().Add(time.Second)) == nil {
		t.Fatal("expired admission was revived")
	}
	if (BindingContext{Target: "machine", Role: RoleSDK, Admission: lease}).valid() {
		t.Fatal("expired connection admitted")
	}
	for _, remote := range []bool{false, true} {
		r := &route{admission: lease, input: input}
		if remote {
			r.peer = &Route{}
		}
		if r.inputAlive() || r.send(nil, &pb.Message{}) == nil {
			t.Fatal("route accepted input after application expiry")
		}
	}
	flow := &Stream{ctx: context.Background(), admission: lease}
	called := false
	if err := flow.hook(func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatal("expired input reached application hook")
	}
	if flow.Send(&pb.Message{}) == nil {
		t.Fatal("expired local response accepted")
	}
	lease.expire()
	select {
	case <-lease.Done():
	default:
		t.Fatal("timer did not close expired admission")
	}
}

func TestAdmissionRenewalAndLateHook(t *testing.T) {
	lease, err := NewAdmissionLease(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	until := time.Now().Add(2 * time.Second)
	if err := lease.Renew(until); err != nil {
		t.Fatal(err)
	}
	lease.expire() // A callback queued for the previous deadline must recheck it.
	select {
	case <-lease.Done():
		t.Fatal("old timer expired renewed admission")
	default:
	}
	if lease.Remaining() < time.Second {
		t.Fatal("renewal did not preserve new deadline")
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	flow := &Stream{ctx: ctx, cancel: cancel, admission: lease}
	if err := flow.hook(func(context.Context) error { lease.Close(); return nil }); err == nil {
		t.Fatal("callback returned success after admission closed")
	}
	if lease.Renew(time.Now().Add(time.Second)) == nil {
		t.Fatal("closed admission revived")
	}
}

func TestAdmissionBoundsFabricInputAndClosesIdleConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := NewAdmissionLease(time.Now().Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	core := New()
	defer core.Close()
	left, right := net.Pipe()
	done := make(chan struct{})
	go func() {
		core.ServeConn(ctx, right, BindingContext{Target: "machine", Role: RoleDaemon, Admission: lease}, ownedHandler{})
		close(done)
	}()
	fabric, err := yamux.Client(left, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()
	control, welcome, err := wire.Handshake(fabric, &pb.Message{Kind: "hello", InputLeaseId: wire.ID(), Target: "machine", Incarnation: wire.ID(), ConnectionGeneration: 1, Payload: api.Payload(api.Hello{Version: api.Version, Role: RoleDaemon}), Data: api.Payload(api.Binding{})})
	if err != nil {
		t.Fatal(err)
	}
	if welcome.InputLeaseMs == 0 || welcome.InputLeaseMs > 2000 {
		t.Fatal("fabric input outlived admission deadline", welcome.InputLeaseMs)
	}
	if err := control.Send(&pb.Message{Kind: "lease_ready", InputLeaseId: welcome.InputLeaseId}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("idle connection survived admission expiry")
	}
	if core.Online("machine") {
		t.Fatal("expired application advertised an online route")
	}
}
