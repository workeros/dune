package tests

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func TestRuntimeWatchIncludesUnopenedAndNewRuntimes(t *testing.T) {
	h := start(t)
	ctx, cancel := context.WithTimeout(h.ctx, 45*time.Second)
	defer cancel()
	subscription, err := h.client.SubscribeRuntimes(ctx)
	must(t, err)
	defer subscription.Close()
	p := profile(h.dir, "acp", mockACPBinary(t))
	p.ManagedACP = true
	journal := filepath.Join(h.dir, "watch-rpcs.log")
	p.Env = map[string]string{"DUNE_MOCK_SESSION_TITLE": "unopened title", "DUNE_MOCK_RPC_LOG": journal}
	runtime, launch, err := testStartProfile(h.client, h.ctx, p)
	must(t, err)
	launch.Close()
	defer testStopRuntime(h.client, h.ctx, runtime)
	waitManagedACPReady(t, h, runtime)
	operation, err := testACPSubmit(h.client, h.ctx, runtime, api.ACPAction{Action: "new", Cwd: h.dir})
	must(t, err)
	_, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 3000})
	must(t, err)
	for {
		change, err := subscription.Next()
		must(t, err)
		metadata := change.Runtime.SessionMetadata
		if change.Runtime.ID != runtime.ID || metadata == nil || metadata.Title == nil {
			continue
		}
		if *metadata.Title != "unopened title" || change.Runtime.Incarnation != runtime.Incarnation || change.Runtime.Title != runtime.Title {
			t.Fatal("incomplete or mismatched directory notification", change)
		}
		break
	}
	before, err := os.ReadFile(journal)
	must(t, err)
	// A new subscription recovers the current value without requiring a new
	// title event, a per-Runtime attach, or a native session replay.
	subscription.Close()
	h.client.Close()
	h.stopProcess("fabricd", syscall.SIGKILL)
	h.startProcess("fabricd")
	h.reconnect()
	recovered, err := h.client.SubscribeRuntimes(ctx)
	must(t, err)
	defer recovered.Close()
	for {
		change, err := recovered.Next()
		must(t, err)
		if change.Runtime.ID == runtime.ID && change.Runtime.SessionMetadata != nil && change.Runtime.SessionMetadata.Title != nil {
			break
		}
	}
	after, err := os.ReadFile(journal)
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("subscription/recovery invoked native controls", string(after))
	}
	must(t, testStopRuntime(h.client, h.ctx, runtime))
	must(t, testForgetRuntime(h.client, h.ctx, runtime))
	for {
		change, err := recovered.Next()
		must(t, err)
		if change.Runtime.ID == runtime.ID && change.Removed {
			break
		}
	}
}
