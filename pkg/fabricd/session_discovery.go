package fabricd

import (
	"context"
	"math/rand/v2"
	"time"
)

// Restore existing IPC connections within a bounded startup window. Per-host
// backoff and the Engine-wide probe pool also apply to concurrent list readers.
// Subsequent ordinary reads can retry a still-unavailable original endpoint.
func (d *Engine) restoreSessionConnections() {
	defer d.active.Done()
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	for {
		page := d.list(ctx)
		pending := false
		for _, runtime := range page.Items {
			if runtime.Availability == "unavailable" {
				pending = true
				break
			}
		}
		if !pending || ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(250*time.Millisecond + time.Duration(rand.Int64N(int64(250*time.Millisecond))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
