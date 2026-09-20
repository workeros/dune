package tests

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func TestStopReceiptSurvivesLostResponseAndConnectorRestart(t *testing.T) {
	h := start(t)
	mock := filepath.Join(h.dir, "mock-acp")
	if output, err := exec.Command("go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput(); err != nil {
		t.Fatalf("mock build: %s %v", output, err)
	}
	p := profile(h.dir, "acp", mock)
	p.ManagedACP = true
	processLog := filepath.Join(h.dir, "process.log")
	p.Env = map[string]string{"DUNE_MOCK_PROCESS_LOG": processLog}
	original, stream, err := testStartProfile(h.client, h.ctx, p)
	must(t, err)
	stream.Close()
	waitManagedACPReady(t, h, original)
	other, stream, err := testStartProfile(h.client, h.ctx, p)
	must(t, err)
	stream.Close()
	waitManagedACPReady(t, h, other)
	defer func() { _ = testStopRuntime(h.client, h.ctx, other) }()
	key := api.SubmissionKey{SubmissionID: "stop-saved-before-send", Target: api.SubmissionTarget{OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: h.c.Target, BindingRevision: 1, RuntimeID: original.ID, RuntimeIncarnation: original.Incarnation, RuntimeGeneration: original.Generation}}
	barrier := newConversationBarrier(t)
	relay := conversationRelay(t, h, nil, func(message *pb.Message) {
		var receipt api.SubmissionReceipt
		if json.Unmarshal(message.Payload, &receipt) == nil && receipt.SubmissionID == key.SubmissionID {
			barrier.hold(h.ctx)
		}
	})
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	failed := make(chan error, 1)
	go func() { _, err := relay.Stop(ctx, key); failed <- err }()
	barrier.wait(t)
	// The actual response is held outside the SDK; the original caller has no
	// business receipt when its connection is cancelled.
	cancel()
	err = <-failed
	var local *api.SubmissionError
	if !errors.As(err, &local) || local.Key != key {
		t.Fatal("missing first stop response lost caller identity", err)
	}
	barrier.open()
	h.client.Close()
	h.stopProcess("fabricd", syscall.SIGKILL)
	h.startProcess("fabricd")
	h.reconnect()
	receipt, err := h.client.QuerySubmission(h.ctx, key)
	must(t, err)
	if receipt.Admission != api.SubmissionAccepted || receipt.Stage != "stopped" || receipt.Runtime == nil || receipt.Runtime.State != "exited" || receipt.Runtime.StopReason != "stopped" {
		t.Fatal("original stop outcome did not survive connector restart", receipt)
	}
	duplicate, err := h.client.Stop(h.ctx, key)
	must(t, err)
	if duplicate.OperationRef != receipt.OperationRef || duplicate.Stage != "stopped" {
		t.Fatal("duplicate changed stop result", duplicate)
	}
	untouched, err := h.client.Get(h.ctx, other)
	must(t, err)
	if untouched.State != "running" || untouched.Incarnation != other.Incarnation {
		t.Fatal("stop affected the neighboring Runtime", untouched)
	}
	data, err := os.ReadFile(processLog)
	must(t, err)
	if strings.Count(strings.TrimSpace(string(data)), "\n")+1 != 2 {
		t.Fatal("stop query/reconnect started another Agent", string(data))
	}
}
