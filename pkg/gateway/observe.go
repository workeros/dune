package gateway

import (
	"time"

	"github.com/aiomni/dune/pkg/observe"
)

// SetObserver replaces the best-effort event callback for this Gateway. The
// callback may be changed concurrently and must return promptly. Protocol core
// emits controlled identifiers and timing only; it never emits message payloads
// or error text. Passing nil disables events.
func (g *Gateway) SetObserver(observer func(observe.Event)) {
	g.observeMu.Lock()
	g.observer = observer
	g.observeMu.Unlock()
}

func (g *Gateway) emit(event observe.Event) {
	g.observeMu.RLock()
	observer := g.observer
	g.observeMu.RUnlock()
	if observer == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	defer func() { _ = recover() }()
	observer(event)
}

func elapsedMicros(started time.Time) int64 {
	elapsed := time.Since(started).Microseconds()
	if elapsed == 0 {
		return 1
	}
	return elapsed
}

func (g *Gateway) observeConnection(target, role, incarnation string, generation uint64) func() {
	started := time.Now()
	base := observe.Event{Name: observe.GatewayConnection, Outcome: "opened", Target: target, Role: role, Incarnation: incarnation, Generation: generation}
	g.emit(base)
	return func() {
		base.Outcome = "closed"
		base.DurationMicros = elapsedMicros(started)
		g.emit(base)
	}
}
