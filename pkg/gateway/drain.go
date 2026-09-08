package gateway

// Status describes this core's intake and current work, without target IDs.
// Connections include idle control sessions; Streams counts accepted requests
// until their forwarding and application callbacks have finished. OnlineRoutes
// counts locally owned, confirmed routes whose input permission is still valid;
// it does not query the directory or imply that every request will succeed.
type Status struct {
	Accepting    bool `json:"accepting"`
	Connections  int  `json:"connections"`
	OnlineRoutes int  `json:"online_routes"`
	Streams      int  `json:"streams"`
}

func (g *Gateway) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	status := Status{Accepting: !g.closed && !g.draining, Connections: len(g.sessions), Streams: len(g.streams)}
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
		if len(g.streams) == 0 {
			close(g.drained)
		}
	}
}

func (g *Gateway) acquireStream(target string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, targetBlocked := g.blocked[target]
	if g.closed || g.draining || targetBlocked {
		return false
	}
	select {
	case g.streams <- struct{}{}:
		g.targetStreams[target]++
		return true
	default:
		return false
	}
}

func (g *Gateway) releaseStream(target string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	<-g.streams
	g.targetStreams[target]--
	if g.targetStreams[target] == 0 {
		delete(g.targetStreams, target)
	}
	g.finishDisconnectLocked(target)
	if g.draining && len(g.streams) == 0 {
		close(g.drained)
	}
}
