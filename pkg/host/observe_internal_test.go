package host

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/observe"
)

type blockingObservationSink struct {
	once    sync.Once
	started chan struct{}
}

func (s *blockingObservationSink) Record(ctx context.Context, _ observe.Event) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
}

func TestObservationDispatcherIsBounded(t *testing.T) {
	sink := &blockingObservationSink{started: make(chan struct{})}
	recorder := newObservationRecorder(sink)
	for range observationQueue * 2 {
		recorder.emit(observe.Event{Name: observe.AccessCheck})
	}
	select {
	case <-sink.started:
	case <-time.After(time.Second):
		t.Fatal("observer was not called")
	}
	if status := recorder.status(); status.Queued > observationQueue || status.Dropped == 0 {
		t.Fatal("dispatcher did not bound a slow sink", status)
	}
	started := time.Now()
	recorder.close()
	if time.Since(started) > time.Second {
		t.Fatal("observer cancellation did not bound close")
	}
}
