package host

import (
	"context"
	"sync"
	"testing"
	"time"

	managedmodule "github.com/aiomni/dune/internal/managed"
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

func TestManagedRenewalDecisionObservationIsBounded(t *testing.T) {
	events := make(chan observe.Event, 1)
	recorder := newObservationRecorder(observe.SinkFunc(func(_ context.Context, event observe.Event) { events <- event }))
	defer recorder.close()
	observeManagedRenewalDecision(recorder)(managedmodule.RenewalDecisionObservation{
		ObservedAt: time.Unix(123, 0).UTC(), NextCheckAt: time.Unix(153, 0).UTC(),
		PrincipalID: "principal", Namespace: "https://identity.example.test", RunnerID: "runner", FabricID: "fabric", ResourceRef: "resource",
		PolicyVersion: "enterprise-v2", Reason: "POLICY_ERROR", Outcome: "policy_error",
	})
	select {
	case event := <-events:
		if event.Name != observe.ManagedRenewalDecision || event.Outcome != "policy_error" || event.Time.Unix() != 123 || event.PrincipalID != "principal" || event.Namespace != "https://identity.example.test" || event.RunnerID != "runner" || event.FabricID != "fabric" || event.ResourceRef != "resource" || event.Operation != "renewal" || event.Suboperation != "policy" || event.PolicyVersion != "enterprise-v2" || event.Reason != "POLICY_ERROR" {
			t.Fatal("unexpected renewal decision event", event)
		}
	case <-time.After(time.Second):
		t.Fatal("renewal decision event was not delivered")
	}
}
