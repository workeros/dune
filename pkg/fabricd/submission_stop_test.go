package fabricd

import (
	"context"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func TestStopDispatchBypassesFullOrdinaryEvidenceAndTransportCache(t *testing.T) {
	d := newEngine(t.Context())
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	d.registry, err = sessionregistry.Open(t.Context(), dir, sessionregistry.Options{MaxKeys: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	key := api.SubmissionKey{SubmissionID: "ordinary", Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1, RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1}}
	admitTestRuntime(t, d.registry, key.Target)
	if _, _, err := d.registry.ClaimKey(t.Context(), key, sessionregistry.Digest("prompt", nil), "host"); err != nil {
		t.Fatal(err)
	}
	for range 256 {
		d.cache[wire.ID()] = &cached{at: time.Now()}
	}
	r := &runtime{id: "runtime", inc: "host", done: make(chan struct{}), subs: map[*subscription]bool{}}
	d.runtimes[r.id] = r
	key.SubmissionID = "stop"
	local, remote := net.Pipe()
	server, err := yamux.Server(local, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := yamux.Client(remote, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	caller, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	out := wire.Wrap(caller)
	_ = out.SetReadDeadline(time.Now().Add(3 * time.Second))
	message := &pb.Message{Operation: "runtime.stop", RequestId: "stop-transport", RuntimeId: r.id, RuntimeIncarnation: r.inc, RuntimeGeneration: 1, Payload: api.Payload(api.SubmissionRequest{SubmissionKey: key, Operation: "runtime.stop"})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.dispatch(&executionStream{Stream: wire.Wrap(handler), ctx: t.Context()}, message, "machine")
	}()
	if accepted, err := out.Recv(); err != nil || accepted.Kind != "accepted" {
		t.Fatal(accepted, err)
	}
	response, err := out.Recv()
	var receipt api.SubmissionReceipt
	if err != nil || response.Kind != "result" || wire.Decode(response, &receipt) != nil || receipt.Stage != "stopped" || receipt.SubmissionKey != key {
		t.Fatal(response, err)
	}
	<-done
	if len(d.cache) != 256 {
		t.Fatal("stop consumed ordinary transport cache")
	}
	delete(d.runtimes, r.id)
	if receipt, err := d.registry.Get(t.Context(), key); err != nil || receipt.Stage != "stopped" {
		t.Fatal("stop lost its evidence with the active Runtime entry", receipt, err)
	}
}

func stopTestProcess(t *testing.T) *process.Process {
	t.Helper()
	p, err := process.Start([]string{"/bin/sh", "-c", "sleep 300 & wait"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, p.Output); p.Output.Close() }()
	go func() { _, _ = io.Copy(io.Discard, p.Stderr); p.Stderr.Close() }()
	t.Cleanup(func() { p.Close(); <-p.Done })
	return p
}

func TestStopAccountsForConcurrentReplacementWithoutWaitingForACPInput(t *testing.T) {
	registry, key := submissionFixture(t)
	admitTestRuntime(t, registry, key.Target)
	r := &runtime{id: key.Target.RuntimeID, inc: key.Target.RuntimeIncarnation, adapter: "acp", p: stopTestProcess(t), done: make(chan struct{}), subs: map[*subscription]bool{}}
	r.acp = newACPController(r)
	r.acp.reconnecting = true
	entered, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	r.acpStart = func() (*process.Process, error) {
		starts.Add(1)
		close(entered)
		<-release
		return stopTestProcess(t), nil
	}
	// A stuck old writer must not prevent stopping the replacement process.
	r.acp.inputMu.Lock()
	defer r.acp.inputMu.Unlock()
	reconnected := make(chan error, 1)
	go func() { reconnected <- r.acp.reconnect() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not reach its spawn barrier")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		receipt api.SubmissionReceipt
		err     error
	}
	finished := make(chan result, 1)
	d := &Engine{registry: registry}
	go func() {
		receipt, err := d.stopSubmission(ctx, r, key, sessionregistry.Digest("runtime.stop", nil), "runtime:"+r.inc)
		finished <- result{receipt, err}
	}()
	for deadline := time.Now().Add(5 * time.Second); ; {
		r.mu.Lock()
		stopped := r.stopped
		r.mu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop waited for the spawning process or ACP writer")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	observed, err := registry.Get(t.Context(), key)
	if err != nil || observed.Admission != api.SubmissionAccepted || observed.Stage != "stopping" {
		t.Fatal("stop side effect preceded durable admission", observed, err)
	}
	select {
	case result := <-finished:
		t.Fatal("stop completed before the in-flight process was accounted for", result)
	default:
	}
	close(release)
	select {
	case result := <-finished:
		if result.err != nil || result.receipt.Stage != "stopped" || result.receipt.SubmissionKey != key || result.receipt.Runtime.State != "exited" {
			t.Fatal("admitted stop did not finish after caller cancellation", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stop was blocked by an ACP lock")
	}
	if err := <-reconnected; err == nil || starts.Load() != 1 {
		t.Fatal("stopped replacement became usable", err, starts.Load())
	}
	if _, err := d.stopSubmission(t.Context(), r, key, sessionregistry.Digest("runtime.stop", nil), "runtime:"+r.inc); err != nil {
		t.Fatal("duplicate lost original stop result", err)
	}
}

func TestStopBeforeReplacementSpawnPreventsNewProcess(t *testing.T) {
	r := &runtime{p: stopTestProcess(t), done: make(chan struct{}), subs: map[*subscription]bool{}, acpReadDone: make(chan struct{})}
	r.acp = newACPController(r)
	r.acp.reconnecting = true
	var starts atomic.Int32
	r.acpStart = func() (*process.Process, error) { starts.Add(1); return stopTestProcess(t), nil }
	reconnected := make(chan error, 1)
	go func() { reconnected <- r.acp.reconnect() }()
	<-r.p.Done // reconnect is now blocked on old output drainage.
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if err := r.waitStop(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(r.acpReadDone)
	if err := <-reconnected; err == nil || starts.Load() != 0 {
		t.Fatal("replacement started after confirmed stop", err, starts.Load())
	}
}
