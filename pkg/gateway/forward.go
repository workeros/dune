package gateway

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aiomni/dune/internal/wire"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func (g *Gateway) forward(parent context.Context, c *wire.Stream, r *route, binding BindingContext, handler ConnectionHandler) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(context.Canceled)
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
	flow := &Stream{ctx: ctx, cancel: cancel, client: c}
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
	if !forward {
		for {
			var message *pb.Message
			message, err = c.Recv()
			if err != nil {
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
	g.mu.Lock()
	current := g.routes[binding.Target]
	g.mu.Unlock()
	if current != r {
		err = fmt.Errorf("reconnect SDK for current binding")
		c.Fail("STALE_BINDING", err)
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
	if err = d.Send(m); err != nil {
		c.Fail("RESULT_UNKNOWN", err)
		return
	}
	done := make(chan error, 2)
	relay := func(dst, src *wire.Stream, direction Direction) {
		for {
			message, e := src.Recv()
			if e == nil {
				e = flow.hook(func(ctx context.Context) error { return policy.Message(ctx, direction, message) })
			}
			if e == nil {
				e = dst.Send(message)
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
