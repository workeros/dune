package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/observe"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func (g *Gateway) forward(parent context.Context, c *wire.Stream, r *route, binding BindingContext, handler ConnectionHandler) {
	started := time.Now()
	var err error
	route := "local"
	if r.peer != nil {
		route = "peer"
	}
	event := observe.Event{Name: observe.GatewayStream, Outcome: "rejected", Target: binding.Target, Role: binding.Role, Route: route, Incarnation: r.b.Incarnation, Generation: r.b.Generation, Epoch: r.b.RouteEpoch}
	defer func() {
		if event.Outcome == "accepted" {
			event.Outcome = "completed"
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				event.Outcome = "interrupted"
			}
		}
		event.DurationMicros = elapsedMicros(started)
		g.emit(event)
	}()
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(context.Canceled)
	// Locally handled streams also belong to the selected reverse connection.
	// Link their lifetime before invoking application hooks, even without relay.
	stopRoute := context.AfterFunc(r.ctx, func() { cancel(fmt.Errorf("execution route closed")) })
	defer stopRoute()
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := c.Recv()
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}
	if m.Kind != "request" || m.RequestId == "" || m.Target != r.b.Target || m.Incarnation != r.b.Incarnation || m.ConnectionGeneration != r.b.Generation {
		c.Fail("STALE_BINDING", fmt.Errorf("invalid target or binding"))
		return
	}
	event.RequestID, event.Operation = m.RequestId, m.Operation
	if m.RouteRecovery != r.b.RouteRecovery || m.RouteEpoch != r.b.RouteEpoch {
		c.Fail("ROUTE_STALE", fmt.Errorf("request belongs to another ownership term"))
		return
	}
	if (binding.Role == RolePeer && (len(m.AccessContext) == 0 || len(m.AccessContext) > MaxAccessContext)) || (binding.Role != RolePeer && len(m.AccessContext) != 0) {
		c.Fail("ACCESS_DENIED", fmt.Errorf("invalid peer access context"))
		return
	}
	if failure := g.routeError(binding.Target, r); failure != nil {
		c.Fail(failure.Code, failure)
		return
	}
	flow := &Stream{ctx: ctx, cancel: cancel, client: c, peer: r.peer, admission: binding.Admission}
	var policy StreamHandler
	err = flow.hook(func(ctx context.Context) error {
		var e error
		policy, e = handler.Open(ctx, m, flow)
		return e
	})
	if policy != nil {
		defer func() {
			if cause := context.Cause(ctx); cause != nil {
				err = cause
			}
			policy.Closed(err)
		}()
	}
	if err == nil && policy == nil {
		err = fmt.Errorf("stream handler required")
	}
	forward := flow.finishOpen()
	if err != nil {
		c.Fail("ACCESS_DENIED", err)
		return
	}
	event.Outcome = "accepted"
	if !forward {
		for {
			var message *pb.Message
			message, err = c.Recv()
			if err != nil {
				return
			}
			if len(message.AccessContext) != 0 {
				err = fmt.Errorf("access context is only valid on the first peer request")
				c.Fail("ACCESS_DENIED", err)
				return
			}
			if failure := g.routeError(binding.Target, r); failure != nil {
				err = failure
				c.Fail(failure.Code, failure)
				return
			}
			err = flow.hook(func(ctx context.Context) error { return policy.Message(ctx, ToFabric, message) })
			if err != nil {
				c.Fail("ACCESS_DENIED", err)
				return
			}
		}
	}
	// The access check may have taken time. Revalidate the fixed route before
	// opening the fabric stream; never transfer a request to a new binding.
	if failure := g.routeError(binding.Target, r); failure != nil {
		err = failure
		c.Fail(failure.Code, failure)
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	raw, err := r.s.OpenStream()
	if err != nil {
		c.Fail("OFFLINE", err)
		return
	}
	d := wire.Wrap(raw)
	defer d.Close()
	stopFabric := context.AfterFunc(ctx, func() { d.Close() })
	defer stopFabric()
	m.AccessContext = flow.accessContext
	if err = r.send(d, m); err != nil {
		event.Outcome = "result_unknown"
		c.Fail("RESULT_UNKNOWN", err)
		return
	}
	done := make(chan error, 2)
	relay := func(dst, src *wire.Stream, direction Direction) {
		for {
			message, e := src.Recv()
			if e == nil && len(message.AccessContext) != 0 {
				e = fmt.Errorf("access context is only valid on the first peer request")
			}
			if e == nil && direction == ToFabric {
				if failure := g.routeError(binding.Target, r); failure != nil {
					e = failure
				}
			}
			if e == nil {
				e = flow.hook(func(ctx context.Context) error { return policy.Message(ctx, direction, message) })
			}
			if e == nil {
				if direction == ToFabric {
					e = r.send(dst, message)
				} else {
					e = dst.Send(message)
				}
			}
			if e != nil {
				done <- e
				return
			}
		}
	}
	go relay(d, c, ToFabric)
	go relay(c, d, ToClient)
	err = <-done
	if err != io.EOF {
		c.Fail("STREAM_INTERRUPTED", err)
	}
	cancel(err)
	c.Close()
	d.Close()
	<-done
}
