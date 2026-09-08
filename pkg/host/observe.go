package host

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/observe"
)

const (
	observationQueue   = 256
	observationTimeout = 100 * time.Millisecond
)

type observationItem struct {
	event observe.Event
}

type observationRecorder struct {
	sink    observe.Sink
	queue   chan observationItem
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	dropped atomic.Uint64
	pending atomic.Uint64
}

// ObservationStatus reports local dispatcher pressure. Dropped is cumulative
// for this App lifetime; Queued is an instantaneous best-effort count.
type ObservationStatus struct {
	Queued  int    `json:"queued"`
	Dropped uint64 `json:"dropped"`
}

func newObservationRecorder(sink observe.Sink) *observationRecorder {
	r := &observationRecorder{sink: sink, stop: make(chan struct{}), done: make(chan struct{})}
	if sink == nil {
		close(r.done)
		return r
	}
	r.queue = make(chan observationItem, observationQueue)
	go r.run()
	return r
}

func (r *observationRecorder) run() {
	defer close(r.done)
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		select {
		case <-r.stop:
			return
		case item := <-r.queue:
			r.deliver(item.event)
		}
	}
}

func (r *observationRecorder) deliver(event observe.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), observationTimeout)
	defer cancel()
	defer func() { _ = recover() }()
	r.sink.Record(ctx, event)
}

func (r *observationRecorder) enqueue(event observe.Event) bool {
	select {
	case <-r.stop:
		return false
	default:
	}
	select {
	case r.queue <- observationItem{event: event}:
		return true
	default:
		return false
	}
}

func (r *observationRecorder) emit(event observe.Event) {
	if r == nil || r.sink == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	if pending := r.pending.Swap(0); pending > 0 {
		if !r.enqueue(observe.Event{Time: event.Time, Name: observe.Dropped, Outcome: "dropped", Count: pending}) {
			r.pending.Add(pending + 1)
			r.dropped.Add(1)
			return
		}
	}
	if !r.enqueue(event) {
		r.pending.Add(1)
		r.dropped.Add(1)
	}
}

func observationMicros(started time.Time) int64 {
	duration := time.Since(started).Microseconds()
	if duration < 1 {
		return 1
	}
	return duration
}

func (r *observationRecorder) status() ObservationStatus {
	if r == nil || r.sink == nil {
		return ObservationStatus{}
	}
	return ObservationStatus{Queued: len(r.queue), Dropped: r.dropped.Load()}
}

func (r *observationRecorder) close() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		close(r.stop)
		<-r.done
		if r.queue != nil {
			remaining := uint64(len(r.queue))
			if remaining > 0 {
				r.dropped.Add(remaining)
				r.pending.Add(remaining)
			}
		}
	})
}

func accessObservation(recorder *observationRecorder) access.CheckObserver {
	if recorder == nil || recorder.sink == nil {
		return nil
	}
	return func(request access.Request, decision access.Decision, err error, elapsed time.Duration) {
		outcome := "allowed"
		if err != nil {
			outcome = "denied"
			if errors.Is(err, access.ErrUnavailable) {
				outcome = "unavailable"
			}
		}
		duration := elapsed.Microseconds()
		if duration == 0 && elapsed > 0 {
			duration = 1
		}
		recorder.emit(observe.Event{
			Name: observe.AccessCheck, Outcome: outcome, DurationMicros: duration,
			PrincipalID: request.PrincipalID, Namespace: request.Namespace,
			RunnerID: request.Binding.RunnerID, MachineID: request.Binding.MachineID,
			Operation: request.Operation, Suboperation: request.Suboperation,
			RequestID: request.RequestID, DecisionID: decision.ID,
			FabricID: request.Resource.FabricID,
		})
	}
}
