package gateway

import (
	"fmt"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/observe"
)

// Status describes this core's intake and current work, without target IDs.
// Connections include idle control sessions; Streams counts accepted requests
// until their forwarding and application callbacks have finished. OnlineRoutes
// counts locally owned, confirmed routes whose input permission is still valid;
// it does not query the directory or imply that every request will succeed.
type Status struct {
	Accepting      bool                         `json:"accepting"`
	Connections    int                          `json:"connections"`
	OnlineRoutes   int                          `json:"online_routes"`
	Streams        int                          `json:"streams"`
	StreamCapacity map[string]api.CapacityUsage `json:"stream_capacity"`
}

func (g *Gateway) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	status := Status{Accepting: !g.closed && !g.draining, Connections: len(g.sessions), Streams: g.activeStreams, StreamCapacity: g.streams.Snapshot()}
	for _, route := range g.routes {
		if !route.s.IsClosed() && route.inputAlive() {
			status.OnlineRoutes++
		}
	}
	return status
}

// Drain irreversibly rejects new connections and request streams. Accepted
// streams and control connections keep their usual validity checks and renewals.
// The returned channel closes once all accepted streams and callbacks finish;
// idle connections do not delay it. Call Close to release those connections.
// Do not wait for Drain from an active stream callback.
func (g *Gateway) Drain() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.beginDrainLocked()
	return g.drained
}

func (g *Gateway) beginDrainLocked() {
	if !g.draining {
		g.draining = true
		if g.activeStreams == 0 {
			close(g.drained)
		}
	}
}

func (g *Gateway) acquireStream(target string) *wire.StreamLease {
	g.mu.Lock()
	_, targetBlocked := g.blocked[target]
	if g.closed || g.draining || targetBlocked {
		g.mu.Unlock()
		return nil
	}
	if lease := g.streams.Acquire(wire.StreamOpening); lease != nil {
		g.targetStreams[target]++
		g.activeStreams++
		g.mu.Unlock()
		return lease
	} else {
		g.mu.Unlock()
		g.emit(observe.Event{Name: observe.GatewayBackpressure, Outcome: "gateway_stream_limit", Target: target})
		return nil
	}
}

func (g *Gateway) classifyStream(target string, class wire.StreamClass, local, global *wire.StreamLease) error {
	g.mu.Lock()
	_, blocked := g.blocked[target]
	available := !g.closed && !g.draining && !blocked
	if available {
		available = local.Move(class) && global.Move(class)
	}
	g.mu.Unlock()
	if !available {
		g.emit(observe.Event{Name: observe.GatewayBackpressure, Outcome: string(class) + "_stream_limit", Target: target})
		return fmt.Errorf("Gateway %s stream capacity unavailable", class)
	}
	return nil
}

func (g *Gateway) releaseStream(target string, lease *wire.StreamLease) {
	g.mu.Lock()
	defer g.mu.Unlock()
	lease.Release()
	g.activeStreams--
	g.targetStreams[target]--
	if g.targetStreams[target] == 0 {
		delete(g.targetStreams, target)
	}
	g.finishDisconnectLocked(target)
	if g.draining && g.activeStreams == 0 {
		close(g.drained)
	}
}
