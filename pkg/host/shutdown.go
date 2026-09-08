package host

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/aiomni/dune/pkg/gateway"
)

// Readiness reports local permission to serve, not the health of external
// identity, policy, database or fabric services. Serving remains true during
// drain while existing work is permitted; Accepting becomes false immediately.
// An expired local peer certificate also makes Accepting false. Requests counts
// HTTP/admin work, excluding tunnel connections and health probes.
type Readiness struct {
	Accepting bool           `json:"accepting"`
	Serving   bool           `json:"serving"`
	Draining  bool           `json:"draining"`
	Requests  int            `json:"requests"`
	Gateway   gateway.Status `json:"gateway"`
}

func (a *App) Readiness() Readiness {
	a.mu.Lock()
	defer a.mu.Unlock()
	core := a.core.Status()
	serving := !a.closed && a.ctx.Err() == nil && a.admission.Remaining() > 0
	peerReady := a.peer == nil || a.peer.Ready()
	return Readiness{Accepting: serving && !a.draining && core.Accepting && peerReady, Serving: serving, Draining: a.draining, Requests: a.requests, Gateway: core}
}

func (a *App) health(w http.ResponseWriter, r *http.Request) bool {
	ready := r.URL.Path == a.publicPath+"health/ready"
	if !ready && r.URL.Path != a.publicPath+"health/live" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	status := a.Readiness()
	w.Header().Set("Content-Type", "application/json")
	if !status.Serving || (ready && !status.Accepting) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(status)
	}
	return true
}

func (a *App) finishRequest() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests--
	if a.draining && a.requests == 0 {
		close(a.requestsDone)
	}
}

func (a *App) beginDrainLocked() {
	if a.draining {
		return
	}
	a.draining = true
	close(a.managedDrain)
	if a.requests == 0 {
		close(a.requestsDone)
	}
}

// Shutdown stops new HTTP/admin work and Gateway connections/streams, waits for
// accepted work, flushes owned HTTP servers, then closes the application. Idle
// tunnel connections do not hold up drain. A context without a deadline receives
// a five-second budget. Expiry cancels remaining work and returns the context
// error after Close releases resources; trusted callbacks must honor cancellation.
// Parent cancellation and Close still stop immediately. Do not call Shutdown
// from an active Dune handler or callback. Externally owned servers remain open.
func (a *App) Shutdown(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	a.mu.Lock()
	a.beginDrainLocked()
	streamsDone := a.core.Drain()
	servers := make([]*http.Server, 0, len(a.servers))
	for srv := range a.servers {
		servers = append(servers, srv)
	}
	a.mu.Unlock()
	for _, done := range []<-chan struct{}{a.requestsDone, streamsDone, a.managedDone} {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(ctx.Err(), a.Close())
		}
	}
	var err error
	for _, srv := range servers {
		err = errors.Join(err, srv.Shutdown(ctx))
	}
	return errors.Join(err, ctx.Err(), a.Close())
}
