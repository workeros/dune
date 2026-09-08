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
	if r.admission.Remaining() <= 0 {
		return false
	}
	if r.peer != nil {
		return r.ctx.Err() == nil && !r.s.IsClosed()
	}
	id, _ := r.input.Current()
	return id != "" && (r.owner == nil || r.owner.remaining() > 0)
}

func (r *route) send(stream *wire.Stream, message *pb.Message) error {
	if r.admission.Remaining() <= 0 {
		return fmt.Errorf("application admission expired")
	}
	if r.peer != nil {
		// Only the receiving owner can issue the execution input grant.
		message.InputLeaseId, message.InputLeaseMs = "", 0
		return stream.Send(message)
	}
	// User authorization context ends at the owning Gateway.
	message.AccessContext = nil
	id, _ := r.input.Current()
	if r.owner != nil && r.owner.remaining() <= 0 {
		return &api.Error{Code: "ROUTE_STALE", Detail: "connection ownership expired"}
	}
	if id == "" {
		return &api.Error{Code: "STALE_BINDING", Detail: "execution input lease expired"}
	}
	// A client or an access hook cannot choose the execution grant. A blocked
	// send retains this ID, even if a later control exchange grants more time.
	message.InputLeaseId, message.InputLeaseMs = id, 0
	message.RouteRecovery, message.RouteEpoch = r.b.RouteRecovery, r.b.RouteEpoch
	return stream.Send(message)
}

func (g *Gateway) serveDaemon(ctx context.Context, session *yamux.Session, control *wire.Stream, hello *pb.Message, admission BindingContext) error {
	target := admission.Target
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
	binding.RouteRecovery, binding.RouteEpoch = "", 0
	r := &route{ctx: ctx, s: session, b: binding, input: wire.NewInputWindow(), admission: admission.Admission}
	if !wire.ValidID(hello.InputLeaseId) {
		control.Fail("HANDSHAKE", fmt.Errorf("input lease challenge required"))
		return nil
	}
	if g.directory != nil {
		owner, err := g.acquireOwner(ctx, binding)
		if err != nil {
			control.Fail("ROUTE_STALE", err)
			return nil
		}
		r.owner = owner
		r.b.RouteRecovery, r.b.RouteEpoch = owner.route.RecoveryGeneration, owner.route.Epoch
		defer func() { session.Close(); owner.release(ctx) }()
		go owner.watch(ctx, func() { session.Close() })
	}
	if err := r.grant(ctx, control, hello.InputLeaseId, "welcome", api.Payload(r.b)); err != nil {
		control.Fail("HANDSHAKE", err)
		return nil
	}
	if r.owner != nil {
		if err := r.owner.publish(ctx); err != nil {
			control.Fail("ROUTE_STALE", err)
			return nil
		}
	}
	if admission.Online != nil {
		bounded, cancel := context.WithTimeout(ctx, HandlerTimeout)
		onlineBinding := r.b
		if r.b.Capabilities != nil {
			onlineBinding.Capabilities = append([]string{}, r.b.Capabilities...)
		}
		if r.b.Limits != nil {
			onlineBinding.Limits = make(map[string]int, len(r.b.Limits))
			for key, value := range r.b.Limits {
				onlineBinding.Limits[key] = value
			}
		}
		err := admission.Online(bounded, onlineBinding)
		if err == nil {
			err = bounded.Err()
		}
		cancel()
		if err != nil {
			control.Fail("HANDSHAKE", err)
			return nil
		}
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
		if request.Kind != "lease_request" || request.InputLeaseMs != 0 || request.RouteRecovery != r.b.RouteRecovery || request.RouteEpoch != r.b.RouteEpoch {
			control.Fail("STALE_BINDING", fmt.Errorf("invalid lease request"))
			return nil
		}
		if err := r.grant(ctx, control, request.InputLeaseId, "lease_grant", nil); err != nil {
			control.Fail("STALE_BINDING", err)
			return nil
		}
	}
}

func (r *route) grant(ctx context.Context, control *wire.Stream, id, kind string, payload []byte) error {
	if err := r.input.Begin(id); err != nil {
		return err
	}
	duration := wire.InputLeaseDuration
	if r.owner != nil {
		if kind != "welcome" {
			if err := r.owner.renew(ctx); err != nil {
				return err
			}
		}
		duration = min(duration, r.owner.remaining()).Truncate(time.Millisecond)
		if duration <= 0 {
			return ErrRouteStale
		}
	}
	duration = min(duration, r.admission.Remaining()).Truncate(time.Millisecond)
	if duration <= 0 {
		return fmt.Errorf("application admission expired")
	}
	deadline := time.Now().Add(5 * time.Second)
	if _, remaining := r.input.Current(); remaining > 0 && remaining < 5*time.Second {
		deadline = time.Now().Add(remaining)
	}
	_ = control.SetReadDeadline(deadline)
	if err := control.Send(&pb.Message{Kind: kind, Payload: payload, InputLeaseId: id, InputLeaseMs: uint32(duration / time.Millisecond), RouteRecovery: r.b.RouteRecovery, RouteEpoch: r.b.RouteEpoch}); err != nil {
		return err
	}
	ready, err := control.Recv()
	if err != nil {
		return err
	}
	if ready.Kind != "lease_ready" || ready.InputLeaseId != id || ready.InputLeaseMs != 0 || ready.RouteRecovery != r.b.RouteRecovery || ready.RouteEpoch != r.b.RouteEpoch {
		return fmt.Errorf("input lease confirmation required")
	}
	return r.input.Confirm(id, duration)
}
