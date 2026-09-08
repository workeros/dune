package gateway_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/observe"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type gatewayEvents struct {
	mu     sync.Mutex
	events []observe.Event
}

func (e *gatewayEvents) add(event observe.Event) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *gatewayEvents) has(match func(observe.Event) bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, event := range e.events {
		if match(event) {
			return true
		}
	}
	return false
}

func waitGatewayEvent(t *testing.T, events *gatewayEvents, match func(observe.Event) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !events.has(match) {
		if time.Now().After(deadline) {
			t.Fatal("gateway event was not observed")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGatewayEmitsConnectionRouteAndStreamEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	g := gateway.New()
	defer g.Close()
	events := &gatewayEvents{}
	g.SetObserver(events.add)
	_, daemonHandler, err := (access.Grant{Target: "machine", Role: gateway.RoleDaemon}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	daemon := session(t, ctx, g, gateway.RoleDaemon, daemonHandler)
	client := session(t, ctx, g, gateway.RoleSDK, &handler{})
	stream := request(t, client)
	raw, err := daemon.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	remote := wire.Wrap(raw)
	remote.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := remote.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := remote.Send(&pb.Message{Kind: "result"}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	stream.Close()
	remote.Close()
	drained := g.Drain()
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal("stream did not finish")
	}
	client.Close()
	daemon.Close()

	waitGatewayEvent(t, events, func(event observe.Event) bool {
		return event.Name == observe.GatewayRoute && event.Outcome == "online" && event.Target == "machine" && event.Incarnation == "boot" && event.Generation == 1
	})
	waitGatewayEvent(t, events, func(event observe.Event) bool {
		return event.Name == observe.GatewayRoute && event.Outcome == "offline" && event.Target == "machine"
	})
	for _, role := range []string{gateway.RoleDaemon, gateway.RoleSDK} {
		role := role
		waitGatewayEvent(t, events, func(event observe.Event) bool {
			return event.Name == observe.GatewayConnection && event.Outcome == "opened" && event.Role == role && event.Target == "machine"
		})
	}
	waitGatewayEvent(t, events, func(event observe.Event) bool {
		return event.Name == observe.GatewayStream && event.Outcome == "completed" && event.Route == "local" && event.Target == "machine" && event.RequestID == "request-1" && event.Operation == "runtime.attach" && event.DurationMicros > 0
	})
}
