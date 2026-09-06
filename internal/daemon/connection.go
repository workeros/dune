package daemon

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
func (d *Daemon) ServeConn(ctx context.Context, conn net.Conn, target string) error {
	if conn == nil {
		return fmt.Errorf("nil connection")
	}
	defer conn.Close()
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
	ctrl, _, err := wire.Handshake(sess, &pb.Message{Kind: "hello", Target: target, Incarnation: d.inc, ConnectionGeneration: gen, Payload: api.Payload(api.Hello{Version: api.Version, Role: "daemon"}), Data: api.Payload(b)})
	if err != nil {
		return err
	}
	log.Printf("fabricd connected incarnation=%s generation=%d", d.inc, gen)
	go func() { _, _ = ctrl.Recv(); sess.Close() }()
	sem := make(chan struct{}, wire.MaxStreams)
	for {
		raw, err := sess.AcceptStream()
		if err != nil {
			return nil
		}
		select {
		case sem <- struct{}{}:
			go func() { defer func() { <-sem }(); d.handle(wire.Wrap(raw), target, gen) }()
		default:
			raw.Close()
		}
	}
}
