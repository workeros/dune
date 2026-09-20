package fabricd

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func TestDiscoverySharesProbeBoundAndBackoffWithoutLockingLookups(t *testing.T) {
	d := newEngine(t.Context())
	defer d.Close()
	var active, maximum, calls atomic.Int32
	started := make(chan struct{}, 32)
	var peers []*yamux.Session
	var readers sync.WaitGroup
	defer func() {
		for _, peer := range peers {
			peer.Close()
		}
		readers.Wait()
	}()
	for range 9 {
		local, remote := net.Pipe()
		connector, err := yamux.Client(local, wire.Config())
		if err != nil {
			t.Fatal(err)
		}
		peer, err := yamux.Server(remote, wire.Config())
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, peer)
		identity := api.Runtime{ID: wire.ID(), Incarnation: wire.ID(), Generation: 1, Adapter: "acp", State: "running"}
		d.runtimes[identity.ID] = &runtime{id: identity.ID, inc: identity.Incarnation, host: &sessionProxy{connection: connector, registration: sessionRegistration{Runtime: identity}}}
		readers.Go(func() {
			for {
				stream, err := peer.AcceptStream()
				if err != nil {
					return
				}
				readers.Go(func() {
					defer stream.Close()
					message, err := wire.Read(stream)
					if err != nil {
						return
					}
					if message.Operation != "runtime.get" {
						t.Error("probe performed control", message.Operation)
						return
					}
					current := active.Add(1)
					defer active.Add(-1)
					for prior := maximum.Load(); current > prior; prior = maximum.Load() {
						if maximum.CompareAndSwap(prior, current) {
							break
						}
					}
					calls.Add(1)
					started <- struct{}{}
					_, _ = wire.Read(stream) // Deliberately never return a Runtime result.
				})
			}
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	results := make(chan api.RuntimeList, 3)
	go func() { results <- d.list(ctx) }()
	for range 8 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("probe pool did not start", ctx.Err())
		}
	}
	lookupStarted := time.Now()
	for _, r := range d.runtimes {
		found, err := d.lookup(&pb.Message{RuntimeId: r.id, RuntimeIncarnation: r.inc, RuntimeGeneration: 1})
		if err != nil || found != r {
			t.Fatal("lookup failed while probes pending", err)
		}
	}
	lookupDuration := time.Since(lookupStarted)
	if lookupDuration > 100*time.Millisecond {
		t.Fatal("network probes held Engine lock")
	}
	for range 2 {
		go func() { results <- d.list(ctx) }()
	}
	for range 3 {
		select {
		case page := <-results:
			if page.Complete || len(page.Items) != 9 || len(page.Issues) != 9 {
				t.Fatal(page)
			}
		case <-ctx.Done():
			t.Fatal("discovery did not respect probe timeout")
		}
	}
	if maximum.Load() > 8 || calls.Load() < 9 {
		t.Fatal("unbounded or missing probes", maximum.Load(), calls.Load())
	}
	// Use the most recent failure, whose backoff cannot expire while older
	// endpoints finish. Repeated reads must not create more requests.
	var recent *sessionProxy
	for _, r := range d.runtimes {
		if recent == nil || r.host.nextProbe.After(recent.nextProbe) {
			recent = r.host
		}
	}
	before := calls.Load()
	for range 50 {
		if result := recent.informationContext(ctx); result.Availability != "unavailable" || result.State != "running" {
			t.Fatal("probe failure fabricated an exit", result)
		}
	}
	if calls.Load() != before {
		t.Fatal("backoff admitted repeated probes", before, calls.Load())
	}
	t.Logf("maximum concurrent probes=%d; exact lookup=%s; failed reads coalesced", maximum.Load(), lookupDuration)
}
