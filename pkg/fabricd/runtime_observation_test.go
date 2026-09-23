package fabricd

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func TestRuntimeObservationOrdersNativeConfirmationAndExit(t *testing.T) {
	r := &runtime{id: "runtime", inc: "original", adapter: "acp", acp: &acpController{conversation: conversationFixture(t)}}
	pending := r.info()
	r.nativeSession = &api.NativeSession{ID: "native", Cwd: "/workspace"}
	confirmed := r.info()
	code := 0
	r.exit, r.stopReason = &code, "completed"
	exited := r.info()
	if pending.Observation.Epoch != confirmed.Observation.Epoch || confirmed.Observation.Epoch != exited.Observation.Epoch ||
		pending.Observation.Revision >= confirmed.Observation.Revision || confirmed.Observation.Revision >= exited.Observation.Revision {
		t.Fatal("full observations were not ordered", pending, confirmed, exited)
	}
	if pending.NativeSession != nil || confirmed.NativeSession == nil || exited.State != "exited" ||
		string(api.Payload(pending.SessionMetadata)) != string(api.Payload(exited.SessionMetadata)) {
		t.Fatal("test must cover distinct Runtime facts with unchanged title metadata")
	}
	var projection hostObservations
	first, err := projection.confirm(pending)
	if err != nil {
		t.Fatal(err)
	}
	last, err := projection.confirm(exited)
	if err != nil {
		t.Fatal(err)
	}
	late, err := projection.confirm(confirmed)
	if err != nil || string(api.Payload(late)) != string(api.Payload(last)) {
		t.Fatal("late IPC snapshot replaced newer state", late, err)
	}
	if failed := projection.unavailable(first.Observation, true); failed.Observation != last.Observation || failed.State != "exited" {
		t.Fatal("late failed probe replaced a newer confirmation", failed)
	}
	unavailable := projection.unavailable(last.Observation, false)
	if unavailable.Availability != "unavailable" || unavailable.Observation.Revision <= last.Observation.Revision || unavailable.LastConfirmedAt != last.LastConfirmedAt || unavailable.NativeSession != last.NativeSession {
		t.Fatal("availability projection lost retained facts", unavailable)
	}
	recovered, err := projection.confirm(r.info())
	if err != nil || recovered.Availability != "" || recovered.Observation.Revision <= unavailable.Observation.Revision {
		t.Fatal("fresh confirmation failed to recover availability", recovered, err)
	}
	// Even if delivery to the hub is delayed, a lower complete observation
	// cannot displace its newer sibling in the coalescing mailbox.
	var hub runtimeWatchHub
	mailbox, unsubscribe := hub.subscribe()
	defer unsubscribe()
	hub.publish(api.RuntimeChange{Runtime: recovered})
	hub.publish(api.RuntimeChange{Runtime: unavailable})
	change, err := mailbox.Next(t.Context())
	if err != nil || string(api.Payload(change.Runtime)) != string(api.Payload(recovered)) {
		t.Fatal("late publication regressed the watch", change, err)
	}
}

func TestRuntimeObservationFencesDelayedIPC(t *testing.T) {
	for _, lateGet := range []bool{true, false} {
		name := "late_watch"
		if lateGet {
			name = "late_get"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			local, remote := net.Pipe()
			connector, err := yamux.Client(local, wire.Config())
			if err != nil {
				t.Fatal(err)
			}
			defer connector.Close()
			peer, err := yamux.Server(remote, wire.Config())
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			stop := context.AfterFunc(ctx, func() { connector.Close(); peer.Close() })
			defer stop()
			r := &runtime{id: "runtime", inc: "original", adapter: "acp", acp: &acpController{conversation: conversationFixture(t)}}
			old := r.info()
			r.nativeSession = &api.NativeSession{ID: "confirmed", Cwd: "/workspace"}
			code := 0
			r.exit = &code
			current := r.info()
			proxy := &sessionProxy{connection: connector, registration: sessionRegistration{Runtime: old}}
			getResult, watchResult := make(chan api.Runtime, 1), make(chan api.Runtime, 1)
			watchDone := make(chan error, 1)
			accept := func(operation string) *wire.Stream {
				t.Helper()
				raw, err := peer.AcceptStream()
				if err != nil {
					t.Fatal(err)
				}
				stream := wire.Wrap(raw)
				message, err := stream.Recv()
				if err != nil || message.Operation != operation {
					t.Fatal("unexpected IPC request", message, err)
				}
				if err := stream.Send(&pb.Message{Kind: "accepted"}); err != nil {
					t.Fatal(err)
				}
				return stream
			}
			read := func(values <-chan api.Runtime) api.Runtime {
				t.Helper()
				select {
				case value := <-values:
					return value
				case <-ctx.Done():
					t.Fatal("IPC barrier did not complete", ctx.Err())
					return api.Runtime{}
				}
			}
			// Both responses are captured, then delivered in the opposite order.
			// The barriers require the newer observation to be consumed first.
			go func() { getResult <- proxy.informationContext(ctx) }()
			getStream := accept("runtime.get")
			defer getStream.Close()
			go func() { watchDone <- proxy.watch(ctx, func(value api.Runtime) { watchResult <- value }) }()
			watchStream := accept("runtime.watch")
			defer watchStream.Close()
			getValue, watchValue := current, old
			if lateGet {
				getValue, watchValue = old, current
			}
			sendGet := func() {
				t.Helper()
				if err := getStream.Send(&pb.Message{Kind: "result", Payload: api.Payload(getValue)}); err != nil {
					t.Fatal(err)
				}
			}
			sendWatch := func() {
				t.Helper()
				if err := watchStream.Send(&pb.Message{Kind: "runtime_changed", Payload: api.Payload(api.RuntimeChange{Runtime: watchValue})}); err != nil {
					t.Fatal(err)
				}
			}
			var first, last api.Runtime
			if lateGet {
				sendWatch()
				first = read(watchResult)
				sendGet()
				last = read(getResult)
			} else {
				sendGet()
				first = read(getResult)
				sendWatch()
				last = read(watchResult)
			}
			if first.State != "exited" || first.NativeSession == nil || string(api.Payload(last)) != string(api.Payload(first)) {
				t.Fatal("late IPC observation regressed confirmed facts", first, last)
			}
			cancel()
			select {
			case <-watchDone:
			case <-time.After(time.Second):
				t.Fatal("watch did not close")
			}
		})
	}
}

func TestRuntimeObservationCancelledDiscoveryRetainsConfirmation(t *testing.T) {
	d := newEngine(t.Context())
	defer d.Close()
	current := api.Runtime{ID: "runtime", Incarnation: "original", Generation: 1, Adapter: "acp", State: "exited", Observation: api.ObservationVersion{Epoch: "host", Revision: 5}}
	proxy := &sessionProxy{registration: sessionRegistration{Runtime: current}}
	confirmed, err := proxy.observations.confirm(current)
	if err != nil {
		t.Fatal(err)
	}
	d.runtimes[current.ID] = &runtime{id: current.ID, inc: current.Incarnation, host: proxy}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := proxy.informationContext(ctx); string(api.Payload(got)) != string(api.Payload(confirmed)) {
		t.Fatal("cancelled reader changed the shared observation", got)
	}
	// Hold every discovery slot to exercise cancellation before a probe starts.
	for range cap(d.discoveryReads) {
		d.discoveryReads <- struct{}{}
	}
	page := d.list(ctx)
	if page.Complete || len(page.Items) != 1 || string(api.Payload(page.Items[0])) != string(api.Payload(confirmed)) {
		t.Fatal("cancelled page fabricated a new availability observation", page)
	}
}
