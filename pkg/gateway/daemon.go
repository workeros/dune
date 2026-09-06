package gateway

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func (r *route) inputAlive() bool {
	id, _ := r.input.Current()
	return id != ""
}

func (r *route) send(stream *wire.Stream, message *pb.Message) error {
	id, _ := r.input.Current()
	if id == "" {
		return &api.Error{Code: "STALE_BINDING", Detail: "execution input lease expired"}
	}
	// A client or an access hook cannot choose the execution grant. A blocked
	// send retains this ID, even if a later control exchange grants more time.
	message.InputLeaseId, message.InputLeaseMs = id, 0
	return stream.Send(message)
}

func (g *Gateway) serveDaemon(ctx context.Context, session *yamux.Session, control *wire.Stream, hello *pb.Message, target string) error {
	if hello.Incarnation == "" || hello.ConnectionGeneration == 0 {
		control.Fail("HANDSHAKE", fmt.Errorf("missing incarnation/generation"))
		return nil
	}
	var binding api.Binding
	if len(hello.Data) == 0 || jsonBinding(hello.Data, &binding) != nil {
		control.Fail("HANDSHAKE", fmt.Errorf("invalid fabric binding"))
		return nil
	}
	binding.Target, binding.Incarnation = target, hello.Incarnation
	binding.Generation, binding.Version = hello.ConnectionGeneration, api.Version
	r := &route{s: session, b: binding, input: wire.NewInputWindow()}
	if err := r.grant(control, hello.InputLeaseId, "welcome", api.Payload(binding)); err != nil {
		control.Fail("HANDSHAKE", err)
		return nil
	}
	// The route becomes visible only after fabricd confirms that its original
	// challenge still permits input. Failed handshakes cannot evict a live route.
	g.mu.Lock()
	if g.closed || ctx.Err() != nil || !r.inputAlive() {
		g.mu.Unlock()
		return nil
	}
	old := g.routes[target]
	g.routes[target] = r
	g.mu.Unlock()
	if old != nil {
		old.s.Close()
	}
	defer func() {
		g.mu.Lock()
		if g.routes[target] == r {
			delete(g.routes, target)
		}
		g.mu.Unlock()
	}()
	go r.input.Watch(ctx, func() { session.Close() })
	log.Printf("fabricd registered incarnation=%s generation=%d", binding.Incarnation, binding.Generation)
	go func() {
		for {
			extra, err := session.AcceptStream()
			if err != nil {
				return
			}
			extra.Close()
		}
	}()
	for {
		_, remaining := r.input.Current()
		if remaining <= 0 {
			return nil
		}
		_ = control.SetReadDeadline(time.Now().Add(remaining))
		request, err := control.Recv()
		if err != nil {
			return nil
		}
		if request.Kind != "lease_request" || request.InputLeaseMs != 0 {
			control.Fail("STALE_BINDING", fmt.Errorf("invalid lease request"))
			return nil
		}
		if err := r.grant(control, request.InputLeaseId, "lease_grant", nil); err != nil {
			control.Fail("STALE_BINDING", err)
			return nil
		}
	}
}

func (r *route) grant(control *wire.Stream, id, kind string, payload []byte) error {
	if err := r.input.Begin(id); err != nil {
		return err
	}
	// A clustered owner will additionally cap this duration by its conservative
	// directory lease deadline. Neither endpoint derives validity from wall time.
	duration := wire.InputLeaseDuration
	deadline := time.Now().Add(5 * time.Second)
	if _, remaining := r.input.Current(); remaining > 0 && remaining < 5*time.Second {
		deadline = time.Now().Add(remaining)
	}
	_ = control.SetReadDeadline(deadline)
	if err := control.Send(&pb.Message{Kind: kind, Payload: payload, InputLeaseId: id, InputLeaseMs: uint32(duration / time.Millisecond)}); err != nil {
		return err
	}
	ready, err := control.Recv()
	if err != nil {
		return err
	}
	if ready.Kind != "lease_ready" || ready.InputLeaseId != id || ready.InputLeaseMs != 0 {
		return fmt.Errorf("input lease confirmation required")
	}
	return r.input.Confirm(id, duration)
}
