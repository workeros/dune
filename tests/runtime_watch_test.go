package tests

import (
	"context"
	"encoding/json"
	"net"
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
	updates, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer updates.Close()
	_ = updates.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Second))
	journal := filepath.Join(h.dir, "watch-rpcs.log")
	p.Env = map[string]string{"DUNE_MOCK_SESSION_TITLE": "unopened title", "DUNE_MOCK_RPC_LOG": journal, "DUNE_MOCK_UPDATE_SOURCE": updates.Addr().String()}
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
	producer, err := updates.Accept()
	must(t, err)
	defer producer.Close()
	encoder := json.NewEncoder(producer)
	var revision uint64
	var observation api.ObservationVersion
	for _, title := range []string{"A", "B", "", "final before reconnect"} {
		must(t, encoder.Encode(map[string]any{"sessionUpdate": "session_info_update", "title": title}))
		for {
			change, err := subscription.Next()
			must(t, err)
			metadata := change.Runtime.SessionMetadata
			if metadata == nil || metadata.Revision <= revision || (title == "" && metadata.Title != nil) || (title != "" && (metadata.Title == nil || *metadata.Title != title)) {
				continue
			}
			revision = metadata.Revision
			if !change.Runtime.Observation.Valid() || (observation.Valid() && (observation.Epoch != change.Runtime.Observation.Epoch || observation.Revision >= change.Runtime.Observation.Revision)) {
				t.Fatal("stream did not advance full observation", change)
			}
			observation = change.Runtime.Observation
			page, err := h.client.List(ctx)
			must(t, err)
			if len(page.Items) != 1 || page.Items[0].Observation.Epoch != observation.Epoch || page.Items[0].Observation.Revision < observation.Revision || string(api.Payload(page.Items[0].SessionMetadata)) != string(api.Payload(metadata)) || page.Items[0].Title != runtime.Title {
				t.Fatal("barrier-confirmed notification and discovery disagree", page)
			}
			read, err := h.client.Get(ctx, runtime)
			must(t, err)
			if read.Observation.Epoch != observation.Epoch || read.Observation.Revision < page.Items[0].Observation.Revision || string(api.Payload(read.SessionMetadata)) != string(api.Payload(metadata)) {
				t.Fatal("single read exposed a different observation order", read)
			}
			break
		}
	}
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
			if change.Runtime.Observation.Epoch == observation.Epoch || !change.Runtime.Observation.Valid() || change.Runtime.SessionMetadata.Revision != revision || *change.Runtime.SessionMetadata.Title != "final before reconnect" {
				t.Fatal("reconnect lost the final title or revision", change)
			}
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
