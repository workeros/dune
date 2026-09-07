package fabricd

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

// ServeConn serves one reverse connection, taking ownership of conn even on
// failure. Cancellation closes the tunnel without destroying tmux sessions.
// Callers reconnect sequentially; reconnection never replays business requests.
func (d *Engine) ServeConn(ctx context.Context, conn net.Conn, target string) error {
	if conn == nil {
		return fmt.Errorf("nil connection")
	}
	defer conn.Close()
	d.mu.Lock()
	if err := d.ctx.Err(); err != nil {
		d.mu.Unlock()
		return err
	}
	d.active.Add(1)
	d.mu.Unlock()
	defer d.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopEngine := context.AfterFunc(d.ctx, cancel)
	defer stopEngine()
	if err := d.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sess, err := yamux.Client(conn, wire.Config())
	if err != nil {
		return err
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	d.mu.Lock()
	d.generation++
	gen := d.generation
	d.mu.Unlock()
	b := api.Binding{Capabilities: capabilities, Limits: map[string]int{"message_bytes": wire.MaxMessage, "streams": wire.MaxStreams, "bulk": 4, "runtimes": 64, "uploads": 64, "dedup_entries": 256, "chunk_bytes": wire.ChunkSize}}
	input := wire.NewInputWindow()
	challenge := wire.ID()
	if err := input.Begin(challenge); err != nil {
		return err
	}
	ctrl, welcome, err := wire.Handshake(sess, &pb.Message{Kind: "hello", InputLeaseId: challenge, Target: target, Incarnation: d.inc, ConnectionGeneration: gen, Payload: api.Payload(api.Hello{Version: api.Version, Role: "daemon"}), Data: api.Payload(b)})
	if err != nil {
		return err
	}
	var accepted api.Binding
	if err := wire.Decode(welcome, &accepted); err != nil || accepted.Target != target || accepted.Incarnation != d.inc || accepted.Generation != gen || accepted.Version != api.Version || ((accepted.RouteEpoch == 0) != (accepted.RouteRecovery == "")) || (accepted.RouteEpoch != 0 && !wire.ValidID(accepted.RouteRecovery)) {
		return fmt.Errorf("invalid accepted execution binding")
	}
	if err := confirmInputLease(input, ctrl, welcome, accepted); err != nil {
		return err
	}
	go input.Watch(ctx, func() { sess.Close() })
	log.Printf("fabricd connected incarnation=%s generation=%d", d.inc, gen)
	go func() { _ = renewInputLease(ctx, input, ctrl, accepted); sess.Close() }()
	sem := make(chan struct{}, wire.MaxStreams)
	for {
		raw, err := sess.AcceptStream()
		if err != nil {
			return nil
		}
		select {
		case sem <- struct{}{}:
			d.mu.Lock()
			if d.ctx.Err() != nil {
				d.mu.Unlock()
				<-sem
				raw.Close()
				continue
			}
			d.active.Add(1)
			d.mu.Unlock()
			go func() {
				defer d.active.Done()
				defer func() { <-sem }()
				d.handle(&executionStream{Stream: wire.Wrap(raw), ctx: ctx, engine: d, generation: gen, input: input, recovery: accepted.RouteRecovery, epoch: accepted.RouteEpoch}, target, gen)
			}()
		default:
			raw.Close()
		}
	}
}
