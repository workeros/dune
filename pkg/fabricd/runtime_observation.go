package fabricd

import (
	"fmt"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func advanceObservation(version api.ObservationVersion) api.ObservationVersion {
	if version.Epoch == "" {
		version.Epoch = wire.ID()
	}
	version.Revision++
	return version
}

// hostObservations serializes IPC reads and watch events into one connector
// projection. Host versions fence late IPC responses; projection versions also
// order connector-local availability changes. No title semantics live here.
type hostObservations struct {
	mu      sync.Mutex
	source  api.ObservationVersion
	current api.Runtime
}

func (o *hostObservations) snapshot(initial api.Runtime) api.Runtime {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.current.Observation.Valid() {
		initial.Availability = "unavailable"
		o.replaceLocked(initial)
	}
	return o.current
}

func (o *hostObservations) confirm(next api.Runtime) (api.Runtime, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !next.Observation.Valid() || (o.source.Valid() && next.Observation.Epoch != o.source.Epoch) {
		return api.Runtime{}, fmt.Errorf("invalid ACP host observation epoch")
	}
	if o.source.Valid() && next.Observation.Revision <= o.source.Revision {
		return o.current, nil
	}
	o.source = next.Observation
	now := time.Now().UTC()
	next.LastConfirmedAt = &now
	return o.replaceLocked(next), nil
}

func (o *hostObservations) unavailable(expected api.ObservationVersion, lost bool) api.Runtime {
	o.mu.Lock()
	defer o.mu.Unlock()
	// A probe started before a newer confirmation cannot make that observation
	// unavailable when its older request finally fails.
	if expected != o.current.Observation {
		return o.current
	}
	next := o.current
	next.Availability = "unavailable"
	if lost {
		next.State, next.Availability, next.StopReason, next.ExitCode = "lost", "lost", "host_lost", nil
	}
	return o.replaceLocked(next)
}

func (o *hostObservations) failed(next api.Runtime) api.Runtime {
	o.mu.Lock()
	defer o.mu.Unlock()
	// A sealed startup failure has no live host observations. A late registry
	// read must never replace a host that has already confirmed its state.
	if o.source.Valid() {
		return o.current
	}
	return o.replaceLocked(next)
}

func (o *hostObservations) replaceLocked(next api.Runtime) api.Runtime {
	next.Observation = advanceObservation(o.current.Observation)
	o.current = next
	return next
}
