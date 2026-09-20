package access

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func TestConversationAuthorizationIdentityAndRetainedExit(t *testing.T) {
	var denied, revoked atomic.Bool
	seen := make(chan Request, 128)
	ctx, client := policyFixture(t, checkFunc(func(ctx context.Context, request Request) (Decision, error) {
		decision, err := (Owner{}).Check(ctx, request)
		if strings.HasPrefix(request.Operation, "acp.") || request.Operation == "runtime.attach" {
			seen <- request
			decision.Allowed = decision.Allowed && !denied.Load()
		}
		return decision, err
	}), func() bool { return !revoked.Load() })
	script := `import sys,json
for line in sys.stdin:
 m=json.loads(line)
 method=m.get('method')
 if method=='initialize': result={'protocolVersion':1,'agentCapabilities':{}}
 elif method=='session/new': result={'sessionId':'auth-native'}
 elif method=='session/prompt':
  print(json.dumps({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'auth-native','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':'retained final answer'}}}}),flush=True)
  print(json.dumps({'jsonrpc':'2.0','id':m['id'],'result':{'stopReason':'end_turn'}}),flush=True)
  sys.exit(0)
 else: continue
 print(json.dumps({'jsonrpc':'2.0','id':m['id'],'result':result}),flush=True)
`
	runtime, initial, err := testStartProfile(client, ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: t.TempDir(), Start: api.Command{Argv: []string{"python3", "-u", "-c", script}}})
	if err != nil {
		t.Fatal(err)
	}
	initial.Close()
	sub, err := client.SubscribeACPConversation(ctx, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for {
		event, err := sub.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind != "acp_state" {
			continue
		}
		var state api.ACPState
		if err := json.Unmarshal(event.Payload, &state); err != nil {
			t.Fatal(err)
		}
		if state.Ready {
			if state.Conversation != nil {
				t.Fatal("unopened controller fabricated empty history")
			}
			break
		}
	}
	assertCode := func(err error, code string) {
		t.Helper()
		var failure *api.Error
		if !errors.As(err, &failure) || failure.Code != code {
			t.Fatalf("want %s got %v", code, err)
		}
	}
	_, err = client.ReadACPConversation(ctx, runtime, api.ACPConversationRead{ConversationID: "nonexistent"})
	assertCode(err, "CONVERSATION_UNAVAILABLE")
	operation, err := testACPSubmit(client, ctx, runtime, api.ACPAction{Action: "new"})
	if err != nil {
		t.Fatal(err)
	}
	operation, err = client.WaitAgentOperation(ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 3000})
	if err != nil || operation.State != "completed" {
		t.Fatal(operation, err)
	}
	id := operation.ConversationID
	for _, expected := range []string{"", "old-generation"} {
		_, err := testACPSubmit(client, ctx, runtime, api.ACPAction{Action: "prompt", Text: "not sent", ExpectedConversationID: expected})
		code := "CONVERSATION_CHANGED"
		if expected == "" {
			code = "INVALID_ARGUMENT"
		}
		assertCode(err, code)
	}
	operation, err = testACPSubmit(client, ctx, runtime, api.ACPAction{Action: "prompt", Text: "retained input", ExpectedConversationID: id})
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := sub.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == "exit" {
			break
		}
	}
	current, err := client.Get(ctx, runtime)
	if err != nil || current.NativeSession == nil || current.NativeSession.ID != "auth-native" || current.State != "exited" {
		t.Fatal("retained Runtime lost confirmed metadata", current, err)
	}
	checks := map[string]func(api.Runtime) error{
		"acp.state": func(target api.Runtime) error { _, err := client.ACPState(ctx, target); return err },
		"acp.conversation.read": func(target api.Runtime) error {
			_, err := client.ReadACPConversation(ctx, target, api.ACPConversationRead{ConversationID: id})
			return err
		},
		"acp.conversation.get": func(target api.Runtime) error {
			_, err := client.GetACPConversationEntries(ctx, target, api.ACPConversationGet{ConversationID: id, EntryIDs: []string{"e-2"}})
			return err
		},
		"runtime.attach": func(target api.Runtime) error {
			s, err := client.SubscribeACPConversation(ctx, target)
			if s != nil {
				s.Close()
			}
			return err
		},
	}
	for name, read := range checks {
		t.Run(name, func(t *testing.T) {
			if err := read(runtime); err != nil {
				t.Fatal(err)
			}
			for _, change := range []func(*api.Runtime){func(r *api.Runtime) { r.ID = wire.ID() }, func(r *api.Runtime) { r.Incarnation = wire.ID() }, func(r *api.Runtime) { r.Generation++ }} {
				wrong := runtime
				change(&wrong)
				assertCode(read(wrong), "STALE_RUNTIME")
			}
			denied.Store(true)
			if err := read(runtime); err == nil {
				t.Fatal("current policy denial was ignored")
			}
			denied.Store(false)
		})
	}
	for len(seen) > 0 {
		request := <-seen
		if request.Scope != testScope() || request.Runtime.ID == "" {
			t.Fatal("request lost authenticated scope or exact Runtime", request)
		}
	}
	page, err := client.ReadACPConversation(ctx, runtime, api.ACPConversationRead{ConversationID: id})
	if err != nil || page.Conversation.Phase != "exited" || page.Conversation.OpenOutcome != "succeeded" || !strings.Contains(string(api.Payload(page)), "retained final answer") {
		t.Fatal("exit lost tail or changed open outcome", err)
	}
	binding := client.Binding
	client.Binding.Generation++
	if err := checks["acp.state"](runtime); err == nil {
		t.Fatal("stale Runner binding accepted")
	}
	client.Binding = binding
	client.Binding.Capabilities = nil
	assertCode(checks["acp.conversation.read"](runtime), "UNSUPPORTED")
	assertCode(checks["acp.conversation.get"](runtime), "UNSUPPORTED")
	assertCode(checks["runtime.attach"](runtime), "UNSUPPORTED")
	client.Binding = binding
	if err := client.CallID(ctx, "runtime.forget", wire.ID(), struct{}{}, nil, &runtime); err != nil {
		t.Fatal(err)
	}
	for _, read := range checks {
		assertCode(read(runtime), "STALE_RUNTIME")
	}
	list, err := client.List(ctx)
	if err != nil || len(list) != 0 {
		t.Fatal("forgotten Runtime remained discoverable", list, err)
	}
	raw, stream, err := testStartProfile(client, ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", WorkingDirectory: t.TempDir(), Start: api.Command{Argv: []string{"/bin/cat"}}})
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	for _, read := range checks {
		assertCode(read(raw), "UNSUPPORTED")
	}
	revoked.Store(true)
	if err := checks["acp.state"](raw); err == nil {
		t.Fatal("revoked connection opened a new request")
	}
}
