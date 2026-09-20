package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/sdk"
	"github.com/aiomni/dune/pkg/transport/ws"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

// A transparent test relay pauses an actual request before Gateway admission,
// or its actual fabricd result before SDK delivery. Neither side fabricates a
// model, rewrites a target, nor changes the ACP subprocess's scheduling.
func conversationRelay(t *testing.T, h *harness, beforeRequest, beforeResult func(*pb.Message)) *sdk.Client {
	t.Helper()
	tls, err := h.c.TLS()
	must(t, err)
	conn, err := ws.Dial(h.ctx, h.c.Gateway, h.c.Token, tls)
	must(t, err)
	downstream, err := yamux.Client(conn, wire.Config())
	must(t, err)
	left, right := net.Pipe()
	upstream, err := yamux.Server(left, wire.Config())
	must(t, err)
	t.Cleanup(func() { upstream.Close(); downstream.Close() })
	go func() {
		for {
			raw, err := upstream.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
				next, err := downstream.OpenStream()
				if err != nil {
					return
				}
				defer next.Close()
				from, to := wire.Wrap(raw), wire.Wrap(next)
				first, err := from.Recv()
				if err != nil {
					return
				}
				if beforeRequest != nil {
					beforeRequest(first)
				}
				if to.Send(first) != nil {
					return
				}
				go func() {
					for {
						m, err := from.Recv()
						if err != nil {
							next.Close()
							return
						}
						if to.Send(m) != nil {
							return
						}
					}
				}()
				for {
					m, err := to.Recv()
					if err != nil {
						return
					}
					if beforeResult != nil && m.Kind == "result" {
						beforeResult(m)
					}
					if from.Send(m) != nil {
						return
					}
				}
			}()
		}
	}()
	client, err := sdk.Connect(h.ctx, right, h.c.Target)
	must(t, err)
	t.Cleanup(func() { client.Close() })
	return client
}

type conversationBarrier struct {
	reached, release chan struct{}
	once             sync.Once
}

func newConversationBarrier(t *testing.T) *conversationBarrier {
	b := &conversationBarrier{reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(b.open)
	return b
}
func (b *conversationBarrier) hold(ctx context.Context) {
	close(b.reached)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
}
func (b *conversationBarrier) open() { b.once.Do(func() { close(b.release) }) }
func (b *conversationBarrier) wait(t *testing.T) {
	t.Helper()
	select {
	case <-b.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("barrier was not reached")
	}
}

func TestACPConversationGenerationBarriersThroughGateway(t *testing.T) {
	h := start(t)
	mock := filepath.Join(h.dir, "mock-acp")
	output, err := exec.Command("go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput()
	if err != nil {
		t.Fatalf("mock build: %s %v", output, err)
	}
	p := profile(h.dir, "acp", mock)
	p.ManagedACP = true
	log := filepath.Join(h.dir, "rpc.log")
	p.Env = map[string]string{"DUNE_MOCK_HISTORY": "1", "DUNE_MOCK_RPC_LOG": log}
	runtime, stream, err := h.client.Start(h.ctx, p)
	must(t, err)
	stream.Close()
	waitManagedACPReady(t, h, runtime)
	created, err := h.client.ACPSubmit(h.ctx, runtime, api.ACPAction{Action: "new"})
	must(t, err)
	wait := func(operation api.AgentOperation) api.AgentOperation {
		t.Helper()
		result, err := h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 5000})
		must(t, err)
		if !result.Terminal() {
			t.Fatalf("operation did not settle: %+v", result)
		}
		return result
	}
	created = wait(created)
	id := created.ConversationID
	if id == "" {
		t.Fatal("missing first generation")
	}
	// An actual Agent permission request is the dispatch barrier. Its prompt RPC
	// stays pending until the test explicitly answers through the control API.
	active, err := h.client.ACPSubmit(h.ctx, runtime, api.ACPAction{Action: "prompt", ExpectedConversationID: id, Text: "permission barrier"})
	must(t, err)
	sub, err := h.client.SubscribeACPConversation(h.ctx, runtime)
	must(t, err)
	defer sub.Close()
	var permission string
	for permission == "" {
		m, err := sub.Recv()
		must(t, err)
		if m.Kind != "acp_state" {
			continue
		}
		var state api.ACPState
		must(t, json.Unmarshal(m.Payload, &state))
		if len(state.Permissions) > 0 {
			permission = state.Permissions[0].ID
		}
	}
	load, err := h.client.ACPSubmit(h.ctx, runtime, api.ACPAction{Action: "load", SessionID: "mock-session", Cwd: h.dir})
	must(t, err)
	shared, err := h.client.ACPSubmit(h.ctx, runtime, api.ACPAction{Action: "load", SessionID: "mock-session", Cwd: h.dir})
	must(t, err)
	if shared.Ref != load.Ref {
		t.Fatal("equivalent queued loads did not share an operation")
	}
	queued, err := h.client.ACPSubmit(h.ctx, runtime, api.ACPAction{Action: "prompt", ExpectedConversationID: id, Text: "stale queued"})
	must(t, err)
	if queued.State != "pending" {
		t.Fatal("dispatch barrier did not hold prompt")
	}
	admission := newConversationBarrier(t)
	delayed := conversationRelay(t, h, func(m *pb.Message) {
		if m.Operation == "acp.action" {
			admission.hold(h.ctx)
		}
	}, nil)
	delayedResult := make(chan error, 1)
	go func() {
		_, err := delayed.ACPSubmit(h.ctx, runtime, api.ACPAction{Action: "prompt", ExpectedConversationID: id, Text: "stale delayed"})
		delayedResult <- err
	}()
	admission.wait(t)
	pageBarrier, getBarrier := newConversationBarrier(t), newConversationBarrier(t)
	reader := conversationRelay(t, h, nil, func(m *pb.Message) {
		var shape map[string]json.RawMessage
		_ = json.Unmarshal(m.Payload, &shape)
		if shape["through_order"] != nil {
			pageBarrier.hold(h.ctx)
		} else if shape["unprocessed_entry_ids"] != nil {
			getBarrier.hold(h.ctx)
		}
	})
	pages := make(chan api.ACPConversationPage, 1)
	entries := make(chan api.ACPConversationEntries, 1)
	readErrors := make(chan error, 2)
	go func() {
		p, err := reader.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: id})
		readErrors <- err
		pages <- p
	}()
	go func() {
		p, err := reader.GetACPConversationEntries(h.ctx, runtime, api.ACPConversationGet{ConversationID: id, EntryIDs: []string{"e-2"}})
		readErrors <- err
		entries <- p
	}()
	pageBarrier.wait(t)
	getBarrier.wait(t)
	// Cancelling a caller's wait cannot cancel the shared load.
	canceled, cancel := context.WithCancel(h.ctx)
	cancel()
	_, _ = h.client.WaitAgentOperation(canceled, runtime, api.AgentOperationWait{Ref: shared.Ref})
	var accepted json.RawMessage
	must(t, h.client.CallID(h.ctx, "acp.action", wire.ID(), api.ACPAction{Action: "permission", PermissionID: permission, OptionID: "allow"}, &accepted, &runtime))
	if wait(active).State != "completed" || wait(load).State != "completed" {
		t.Fatal("explicit release did not complete prompt and shared load")
	}
	rejected := wait(queued)
	if rejected.State != "failed" || rejected.ErrorCode != "CONVERSATION_CHANGED" {
		t.Fatalf("queued stale generation: %+v", rejected)
	}
	current, err := h.client.ACPState(h.ctx, runtime)
	must(t, err)
	if current.Conversation.ID == id {
		t.Fatal("same native ID did not switch generation")
	}
	admission.open()
	if err := <-delayedResult; !conversationErrorCode(err, "CONVERSATION_CHANGED") {
		t.Fatal("delayed request rebound at admission", err)
	}
	pageBarrier.open()
	getBarrier.open()
	must(t, <-readErrors)
	must(t, <-readErrors)
	oldPage, oldGet := <-pages, <-entries
	if oldPage.Conversation.ID != id || oldGet.Conversation.ID != id || len(oldGet.Entries) != 1 || !strings.Contains(string(api.Payload(oldGet.Entries)), "permission barrier") {
		t.Fatal("in-flight snapshots were relabeled or mixed after switch")
	}
	if _, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: id}); !conversationErrorCode(err, "CONVERSATION_CHANGED") {
		t.Fatal("old read accepted after switch", err)
	}
	if _, err := h.client.GetACPConversationEntries(h.ctx, runtime, api.ACPConversationGet{ConversationID: id, EntryIDs: []string{"e-2"}}); !conversationErrorCode(err, "CONVERSATION_CHANGED") {
		t.Fatal("old get accepted after switch", err)
	}
	page, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: current.Conversation.ID})
	must(t, err)
	if strings.Contains(string(api.Payload(page)), "stale queued") || strings.Contains(string(api.Payload(page)), "stale delayed") {
		t.Fatal("rejected prompts appeared as sent body")
	}
	rpcs, err := os.ReadFile(log)
	must(t, err)
	if got, want := string(rpcs), "initialize\nsession/new\nsession/prompt\ninitialize\nsession/load\n"; got != want {
		t.Fatalf("unexpected Agent RPC order: %q want %q", got, want)
	}
}

func conversationErrorCode(err error, code string) bool {
	var result *api.Error
	return errors.As(err, &result) && result.Code == code
}

func waitManagedACPReady(t *testing.T, h *harness, runtime api.Runtime) api.ACPState {
	t.Helper()
	stream, err := h.client.SubscribeACPConversation(h.ctx, runtime)
	must(t, err)
	defer stream.Close()
	for {
		message, err := stream.Recv()
		must(t, err)
		if message.Kind == "exit" {
			t.Fatal("Agent exited before initialize")
		}
		if message.Kind != "acp_state" {
			continue
		}
		var state api.ACPState
		must(t, json.Unmarshal(message.Payload, &state))
		if state.Ready {
			return state
		}
		if state.Error != "" {
			t.Fatal("Agent initialization failed:", state.Error)
		}
	}
}
