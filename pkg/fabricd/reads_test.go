package fabricd

import (
	"net"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func TestSessionReadsBypassFullOrdinaryTransportCache(t *testing.T) {
	a, requests := queueFixture(t)
	a.r.id, a.r.inc = "runtime", "host"
	d := newEngine(t.Context())
	d.runtimes[a.r.id] = a.r
	t.Cleanup(func() { delete(d.runtimes, a.r.id); d.Close() })
	for range 256 {
		d.cache[wire.ID()] = &cached{at: time.Now()}
	}
	operation, err := a.operations.create()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runtime.get", "acp.state", "agent.operation.wait", "agent.operation.read"} {
		local, remote := net.Pipe()
		server, err := yamux.Server(local, wire.Config())
		if err != nil {
			t.Fatal(err)
		}
		client, err := yamux.Client(remote, wire.Config())
		if err != nil {
			t.Fatal(err)
		}
		caller, err := client.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		handler, err := server.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		incoming := &executionStream{Stream: wire.Wrap(handler), ctx: t.Context()}
		out := wire.Wrap(caller)
		_ = out.SetReadDeadline(time.Now().Add(3 * time.Second))
		message := &pb.Message{Operation: name, RequestId: "same-read-request", RuntimeId: a.r.id, RuntimeIncarnation: a.r.inc, RuntimeGeneration: 1, Payload: api.Payload(api.AgentOperationWait{Ref: operation.Ref})}
		done := make(chan struct{})
		go func() { defer close(done); d.dispatch(incoming, message, "machine") }()
		accepted, err := out.Recv()
		if err != nil || accepted.Kind != "accepted" {
			t.Fatal(name, accepted, err)
		}
		result, err := out.Recv()
		if err != nil || result.Kind != "result" {
			t.Fatal(name, result, err)
		}
		<-done
		server.Close()
		client.Close()
	}
	if len(d.cache) != 256 {
		t.Fatal("reads consumed ordinary cache entries")
	}
	select {
	case request := <-requests:
		t.Fatal("read dispatched ACP control", request)
	default:
	}
}
