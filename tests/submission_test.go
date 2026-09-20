package tests

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

// This tracer bullet exercises public receipt reads against durable fixture
// evidence. It does not yet claim that Agent submission or process survival is
// implemented: no Agent is started, and the admission fixture uses the registry.
func TestSubmissionQueryAcrossFabricdRestart(t *testing.T) {
	h := start(t)
	key := api.SubmissionKey{SubmissionID: "saved-before-send", Target: api.SubmissionTarget{
		OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric",
		MachineID: h.c.Target, BindingRevision: 1,
		RuntimeID: "original-runtime", RuntimeIncarnation: "original-incarnation", RuntimeGeneration: 1,
	}}
	receipt, err := h.client.QuerySubmission(h.ctx, key)
	must(t, err)
	if receipt.Admission != api.SubmissionUnknown || receipt.SubmissionKey != key {
		t.Fatal("missing receipt was treated as a negative admission", receipt)
	}
	runtime := api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration}
	const readID = "same-transport-read"
	must(t, h.client.CallID(h.ctx, "submission.get", readID, key, &receipt, &runtime))
	registry, err := sessionregistry.Open(h.ctx, filepath.Join(h.c.SessionDir, "registry"), sessionregistry.Options{})
	must(t, err)
	defer registry.Close()
	claim, _, err := registry.ClaimKey(h.ctx, key, sessionregistry.Digest("prompt", []byte("fixture")), "original-host")
	must(t, err)
	if !claim.Acquired() {
		t.Fatal("read-only queries consumed the admission key")
	}
	_, err = registry.Accept(h.ctx, claim, "original-operation")
	must(t, err)
	must(t, h.client.CallID(h.ctx, "submission.get", readID, key, &receipt, &runtime))
	if receipt.Admission != api.SubmissionAccepted || receipt.OperationRef != "original-operation" {
		t.Fatal("transport cache hid newer admission evidence", receipt)
	}
	previous := h.client.Binding.Incarnation
	h.client.Close()
	h.stopProcess("fabricd", syscall.SIGKILL)
	h.startProcess("fabricd")
	h.reconnect()
	if h.client.Binding.Incarnation == previous {
		t.Fatal("test did not replace the connector process")
	}
	receipt, err = h.client.QuerySubmission(h.ctx, key)
	must(t, err)
	if receipt.Admission != api.SubmissionAccepted || receipt.OperationRef != "original-operation" || receipt.SubmissionKey != key {
		t.Fatal("restart lost or rebound the original receipt", receipt)
	}
	runtimes, err := h.client.List(h.ctx)
	must(t, err)
	if len(runtimes) != 0 {
		t.Fatal("receipt lookup created a replacement Runtime", runtimes)
	}
}
