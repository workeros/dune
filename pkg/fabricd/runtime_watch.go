package fabricd

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/latest"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type runtimeWatchMailbox = latest.Mailbox[string, api.RuntimeChange]

// The hub distributes observations; it neither merges ACP nor owns Runtime
// membership. Engine.mu serializes publication with removal from the directory.
type runtimeWatchHub struct {
	mu          sync.Mutex
	subscribers map[*runtimeWatchMailbox]struct{}
	current     map[string]api.RuntimeChange
}

func (h *runtimeWatchHub) subscribe() (*runtimeWatchMailbox, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subscribers == nil {
		h.subscribers = make(map[*runtimeWatchMailbox]struct{})
	}
	mailbox := latest.New[string, api.RuntimeChange](api.MaxRuntimeWatchPending)
	for id, change := range h.current {
		mailbox.Offer(id, change)
	}
	h.subscribers[mailbox] = struct{}{}
	return mailbox, func() {
		h.mu.Lock()
		delete(h.subscribers, mailbox)
		h.mu.Unlock()
	}
}

func (h *runtimeWatchHub) publish(change api.RuntimeChange) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current == nil {
		h.current = make(map[string]api.RuntimeChange)
	}
	if !change.Removed {
		previous, exists := h.current[change.Runtime.ID]
		if exists && previous.Runtime.Incarnation == change.Runtime.Incarnation && previous.Runtime.Generation == change.Runtime.Generation {
			if previous.Runtime.Observation.Epoch != change.Runtime.Observation.Epoch {
				for mailbox := range h.subscribers {
					mailbox.Fail(fmt.Errorf("Runtime observation epoch changed; resynchronize"))
				}
			} else if change.Runtime.Observation.Revision <= previous.Runtime.Observation.Revision {
				return
			}
		}
		h.current[change.Runtime.ID] = change
	} else {
		delete(h.current, change.Runtime.ID)
	}
	// Runtime IDs are unique in one Engine; removal dominates any pending
	// update for that member because the Engine checks membership before publish.
	for mailbox := range h.subscribers {
		mailbox.Offer(change.Runtime.ID, change)
	}
}

func (d *Engine) publishRuntime(r *runtime, current api.Runtime) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() == nil && d.runtimes[r.id] == r {
		d.runtimeWatches.publish(api.RuntimeChange{Runtime: current})
	}
}

// removeRuntimeLocked retires membership and cached discovery diagnostics at
// the same boundary. A completed forget cannot leave an old registration issue
// that makes the retired identity appear temporarily recoverable. Requires mu.
func (d *Engine) removeRuntimeLocked(r *runtime) {
	delete(d.runtimes, r.id)
	issues := d.discoveryIssues[:0]
	for _, issue := range d.discoveryIssues {
		if issue.Runtime == nil || issue.Runtime.ID != r.id || issue.Runtime.Incarnation != r.inc {
			issues = append(issues, issue)
		}
	}
	d.discoveryIssues = issues
	d.runtimeWatches.publish(api.RuntimeChange{Runtime: api.Runtime{ID: r.id, Incarnation: r.inc, Generation: 1, Adapter: r.adapter}, Removed: true})
}

func (r *runtime) publishObservation() {
	if r.observer == nil {
		return
	}
	r.observationMu.Lock()
	defer r.observationMu.Unlock()
	r.observer(r, r.info())
}

func (d *Engine) watchRuntimes(s *executionStream, request *pb.Message) {
	mailbox, unsubscribe := d.runtimeWatches.subscribe()
	defer unsubscribe()
	// Install the observer before discovering sources. Each source subscription
	// starts with a full snapshot, closing the gap until that host is connected.
	d.refreshSessionRegistrations(s.ctx)
	d.startRuntimeObservers()
	d.mu.Lock()
	if d.ctx.Err() == nil {
		d.runtimeObserveOnce.Do(func() {
			d.active.Add(1)
			go d.observeRuntimeDirectory()
		})
	}
	runtimes := make([]*runtime, 0, len(d.runtimes))
	for _, r := range d.runtimes {
		runtimes = append(runtimes, r)
	}
	d.mu.Unlock()
	for _, r := range runtimes {
		if r.host == nil {
			r.observationMu.Lock()
			d.publishRuntime(r, r.info())
			r.observationMu.Unlock()
		} else {
			r.observationMu.Lock()
			d.publishRuntime(r, r.host.lastObservation())
			r.observationMu.Unlock()
		}
	}
	if err := s.Send(&pb.Message{Kind: "accepted"}); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	go func() {
		_, _ = s.Recv() // Observation streams accept no client messages.
		cancel()
	}()
	for {
		change, err := mailbox.Next(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.Fail("RESYNC_REQUIRED", err)
			}
			return
		}
		if err := s.Send(&pb.Message{Kind: "runtime_changed", Payload: api.Payload(change)}); err != nil {
			return
		}
	}
}

// One source per independent host, shared by all Runner subscriptions. Only
// registration membership is scanned; title changes arrive over IPC, without
// polling state/get or attaching transcript/diagnostic streams.
func (d *Engine) observeRuntimeDirectory() {
	defer d.active.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.refreshSessionRegistrations(d.ctx)
			d.startRuntimeObservers()
		}
	}
}

func (d *Engine) startRuntimeObservers() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		return
	}
	for _, r := range d.runtimes {
		if r.host == nil || r.observing {
			continue
		}
		r.observing = true
		d.active.Add(1)
		go d.observeRuntimeHost(r)
	}
}

func (d *Engine) observeRuntimeHost(r *runtime) {
	defer d.active.Done()
	for {
		d.mu.Lock()
		current := d.runtimes[r.id] == r && d.ctx.Err() == nil
		d.mu.Unlock()
		if !current {
			return
		}
		err := r.host.watch(d.ctx, func(value api.Runtime) {
			r.observationMu.Lock()
			defer r.observationMu.Unlock()
			d.publishRuntime(r, value)
		})
		if d.ctx.Err() != nil {
			return
		}
		if err != nil {
			r.observationMu.Lock()
			value := r.host.lastObservation()
			value = r.host.observations.unavailable(value.Observation, r.host.lossProven())
			d.publishRuntime(r, value)
			r.observationMu.Unlock()
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-d.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *sessionProxy) lastObservation() api.Runtime {
	// Registration is immutable after construction; only observations are live.
	return p.observations.snapshot(p.registration.Runtime)
}

func (p *sessionProxy) watch(ctx context.Context, publish func(api.Runtime)) error {
	request := p.request("runtime.watch", struct{}{})
	stream, err := p.open(ctx, request)
	if err != nil {
		return err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		if message.Kind == "accepted" {
			// The host installed its subscription before admission. The initial
			// state follows on that same stream, not a separate runtime.get.
			continue
		}
		var change api.RuntimeChange
		if message.Kind != "runtime_changed" || wire.Decode(message, &change) != nil || change.Runtime.ID != request.RuntimeId || change.Runtime.Incarnation != request.RuntimeIncarnation || change.Runtime.Generation != request.RuntimeGeneration {
			return fmt.Errorf("invalid ACP host observation")
		}
		observed, err := p.observations.confirm(change.Runtime)
		if err != nil {
			return err
		}
		publish(observed)
	}
}
