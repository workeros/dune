package gateway_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func capacityRequest(t *testing.T, session *yamux.Session, operation string, payload any) (*wire.Stream, *pb.Message) {
	t.Helper()
	raw, err := session.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	s := wire.Wrap(raw)
	t.Cleanup(func() { s.Close() })
	_ = s.SetReadDeadline(time.Now().Add(3 * time.Second))
	err = s.Send(&pb.Message{Kind: "request", RequestId: wire.ID(), Operation: operation, Target: "machine", Incarnation: "boot", ConnectionGeneration: 1, RuntimeId: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1, Payload: api.Payload(payload)})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	return s, m
}

func TestGatewayReservesStreamsAcrossConnectionAndGlobalOrdinarySaturation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	g := gateway.New()
	defer g.Close()
	daemon := session(t, ctx, g, gateway.RoleDaemon, &handler{})
	var received atomic.Int32
	go func() {
		for {
			raw, err := daemon.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				s := wire.Wrap(raw)
				defer s.Close()
				if _, err := s.Recv(); err != nil {
					return
				}
				received.Add(1)
				_ = s.Send(&pb.Message{Kind: "accepted"})
				_, _ = s.Recv()
			}()
		}
	}()
	var clients []*yamux.Session
	var ordinary []*wire.Stream
	for range 8 {
		c := session(t, ctx, g, gateway.RoleSDK, &handler{})
		clients = append(clients, c)
		for range wire.MaxStreams {
			s, m := capacityRequest(t, c, "runtime.attach", api.Attach{Observe: true})
			if m.Kind != "accepted" {
				t.Fatal("ordinary limit changed", m)
			}
			ordinary = append(ordinary, s)
		}
	}
	if usage := g.Status().StreamCapacity["ordinary"]; usage.Used != 512 || usage.Limit != 512 || received.Load() != 512 {
		t.Fatal("global ordinary capacity was not exercised", usage, received.Load())
	}
	// A full connection and a fresh connection both reject ordinary work.
	for _, c := range []*yamux.Session{clients[0], session(t, ctx, g, gateway.RoleSDK, &handler{})} {
		s, m := capacityRequest(t, c, "runtime.attach", api.Attach{Observe: true})
		s.Close()
		if m.Code != "RESOURCE_EXHAUSTED" {
			t.Fatal("ordinary traffic borrowed protected slots", m)
		}
	}
	key := api.SubmissionKey{SubmissionID: "original", Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1, RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1}}
	// Fill permission/cancel and long-poll execution slots as well. Stop and
	// forget must remain independent of these classes, not just of ordinary work.
	for _, action := range []string{"permission", "elicitation", "cancel", "wait"} {
		for i := range wire.MaxReservedStreams {
			key.SubmissionID = fmt.Sprintf("%s-%d", action, i)
			op := "submission.acp"
			var payload any = api.SubmissionRequest{SubmissionKey: key, Operation: "acp.action", Payload: api.Payload(api.ACPAction{Action: action, PermissionID: "permission", OptionID: "allow", ElicitationID: "question", ElicitationResponse: &api.ACPElicitationResponse{Action: "decline"}, OperationRef: "operation"})}
			if action == "wait" {
				op, payload = "agent.operation.wait", api.AgentOperationWait{Ref: "operation", TimeoutMS: 30000}
			}
			s, m := capacityRequest(t, clients[0], op, payload)
			if m.Kind != "accepted" {
				t.Fatal("class did not retain its independent budget", action, m)
			}
			ordinary = append(ordinary, s)
		}
	}
	for _, op := range []string{"submission.get", "acp.state", "runtime.stop", "runtime.forget"} {
		var payload any = api.SubmissionRequest{SubmissionKey: key, Operation: op}
		if op == "submission.get" {
			payload = key
		}
		s, m := capacityRequest(t, clients[0], op, payload)
		if m.Kind != "accepted" {
			t.Fatal("ordinary/other-control saturation blocked protected request", op, m)
		}
		s.Close()
	}
	// Permission-looking payloads cannot turn ordinary work into a control.
	spoof := api.SubmissionRequest{SubmissionKey: key, Operation: "acp.action", Payload: api.Payload(api.ACPAction{Action: "prompt", Text: "permission"})}
	s, m := capacityRequest(t, clients[0], "runtime.stop", spoof)
	s.Close()
	if m.Code != "RESOURCE_EXHAUSTED" {
		t.Fatal("forged class bypassed ordinary quota", m)
	}
	// Reserved capacity is not an authorization grant.
	denied := session(t, ctx, g, gateway.RoleSDK, &handler{open: func(context.Context, *pb.Message, *gateway.Stream) (gateway.StreamHandler, error) {
		return nil, fmt.Errorf("policy denied control")
	}})
	before := received.Load()
	s, m = capacityRequest(t, denied, "runtime.stop", api.SubmissionRequest{SubmissionKey: key, Operation: "runtime.stop"})
	s.Close()
	if m.Code != "ACCESS_DENIED" || received.Load() != before {
		t.Fatal("control skipped authorization", m, received.Load())
	}
	drained := g.Drain()
	select {
	case <-drained:
		t.Fatal("drain ignored classified streams")
	default:
	}
	for _, s := range ordinary {
		s.Close()
	}
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal("classified streams did not drain", g.Status())
	}
	for class, usage := range g.Status().StreamCapacity {
		if usage.Used != 0 {
			t.Fatal("stream reservation leaked", class, usage)
		}
	}
}

func TestGatewayFirstMessageReadersStayBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	g := gateway.New()
	defer g.Close()
	_ = session(t, ctx, g, gateway.RoleDaemon, &handler{})
	c := session(t, ctx, g, gateway.RoleSDK, &handler{})
	for range wire.MaxRequestReaders {
		raw, err := c.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { raw.Close() })
	}
	for g.Status().StreamCapacity["opening"].Used != wire.MaxRequestReaders {
		if ctx.Err() != nil {
			t.Fatal("first-message readers were not accounted", g.Status())
		}
		time.Sleep(time.Millisecond)
	}
	raw, err := c.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := wire.Read(raw); err == nil {
		t.Fatal("unbounded undecoded request")
	}
	if g.Status().StreamCapacity["opening"].Used != wire.MaxRequestReaders {
		t.Fatal("read limit was changed to accept more first messages")
	}
}
