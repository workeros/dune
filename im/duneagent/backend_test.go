package duneagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/host"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type fakeScopes struct{ scope host.AgentScope }

func (f fakeScopes) ResolveAgentScope(context.Context, channel.ConversationSession) (host.AgentScope, error) {
	return f.scope, nil
}

type fakeExecutor struct{ connection *fakeConnection }

func (f fakeExecutor) Open(context.Context, host.AgentScope) (host.AgentConnection, error) {
	return f.connection, nil
}

type fakeSubscription struct{ messages []*pb.Message }

func (s *fakeSubscription) Recv() (*pb.Message, error) {
	if len(s.messages) == 0 {
		return nil, io.EOF
	}
	message := s.messages[0]
	s.messages = s.messages[1:]
	return message, nil
}
func (*fakeSubscription) Close() error { return nil }

type fakeConnection struct {
	config       api.AgentConfig
	runtime      api.Runtime
	state        host.AgentState
	subscription *fakeSubscription
	profile      api.Profile
	actions      []host.AgentAction
	getErr       error
	starts       int
}

func (f *fakeConnection) AgentConfig(_ context.Context, id string) (api.AgentConfig, error) {
	if id != f.config.ID {
		return api.AgentConfig{}, errors.New("not found")
	}
	return f.config, nil
}
func (f *fakeConnection) Start(_ context.Context, profile api.Profile) (api.Runtime, error) {
	f.profile = profile
	f.starts++
	return f.runtime, nil
}
func (f *fakeConnection) Get(context.Context, api.Runtime) (api.Runtime, error) {
	if f.getErr != nil {
		return api.Runtime{}, f.getErr
	}
	return f.runtime, nil
}
func (f *fakeConnection) Observe(context.Context, api.Runtime) (host.AgentSubscription, error) {
	return f.subscription, nil
}
func (f *fakeConnection) State(context.Context, api.Runtime) (host.AgentState, error) {
	return f.state, nil
}
func (f *fakeConnection) Action(_ context.Context, _ api.Runtime, action host.AgentAction) error {
	f.actions = append(f.actions, action)
	if action.Action == "new" {
		f.state.SessionID = "acp-session-a"
	}
	if action.Action == "load" {
		f.state.SessionID = action.SessionID
	}
	return nil
}
func (*fakeConnection) Stop(context.Context, api.Runtime) error { return nil }
func (*fakeConnection) Close() error                            { return nil }

func testBackend() (Backend, channel.ConversationSession, *fakeConnection) {
	connection := &fakeConnection{
		config:  api.AgentConfig{ID: "agent-a", Adapter: "acp", Command: "/bin/echo", Args: []string{"--acp"}},
		runtime: api.Runtime{ID: "runtime-a", Incarnation: "inc-a", Generation: 2, Adapter: "acp", State: "running"},
		state:   host.AgentState{Ready: true, Revision: 3},
	}
	conversation := channel.ConversationSession{Key: channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a"}, Target: channel.AgentTarget{RunnerID: "runner-a", AgentConfigID: "agent-a", WorkingDirectory: "/tmp/im-test"}}
	backend := Backend{Executor: fakeExecutor{connection: connection}, Scopes: fakeScopes{scope: host.AgentScope{OwnerID: "tenant-a", RunnerID: "runner-a"}}}
	return backend, conversation, connection
}

func TestStartCreatesManagedACPRuntimeAndSession(t *testing.T) {
	backend, conversation, connection := testBackend()
	capabilities, err := backend.Capabilities(context.Background(), conversation)
	if err != nil || capabilities.Adapter != "acp" || !capabilities.AssistantDeltas || !capabilities.ReliableFinal {
		t.Fatalf("ACP capabilities: %+v %v", capabilities, err)
	}
	session, err := backend.Start(context.Background(), conversation)
	if err != nil {
		t.Fatal(err)
	}
	if !connection.profile.ManagedACP || connection.profile.WorkingDirectory != conversation.Target.WorkingDirectory || session.Runtime.ID != "runtime-a" || session.ACPSessionID != "acp-session-a" || len(connection.actions) != 1 || connection.actions[0].Action != "new" {
		t.Fatalf("incorrect managed ACP start: profile=%+v session=%+v actions=%v", connection.profile, session, connection.actions)
	}
}

func TestBackendRejectsWrongTenantOrRunnerScope(t *testing.T) {
	backend, conversation, _ := testBackend()
	backend.Scopes = fakeScopes{scope: host.AgentScope{OwnerID: "other-tenant", RunnerID: "runner-a"}}
	if _, err := backend.Capabilities(context.Background(), conversation); err == nil {
		t.Fatal("cross-tenant Agent scope accepted")
	}
	backend.Scopes = fakeScopes{scope: host.AgentScope{OwnerID: "tenant-a", RunnerID: "other-runner"}}
	if _, err := backend.Start(context.Background(), conversation); err == nil {
		t.Fatal("different Runner Agent scope accepted")
	}
}

func TestCapabilitiesRejectsInvalidProfileBeforePromptSubmission(t *testing.T) {
	backend, conversation, connection := testBackend()
	conversation.Target.WorkingDirectory = "relative/path"
	if _, err := backend.Capabilities(context.Background(), conversation); err == nil || connection.starts != 0 {
		t.Fatalf("invalid Agent working directory reached runtime start: starts=%d err=%v", connection.starts, err)
	}
}

func TestACPInputPreflightMatchesFabricdLimit(t *testing.T) {
	backend := Backend{}
	for _, input := range []string{"", " \n ", strings.Repeat("x", 64*1024+1)} {
		if err := backend.ValidateInput(input); err == nil {
			t.Fatalf("invalid prompt length %d accepted", len(input))
		}
	}
	if err := backend.ValidateInput(strings.Repeat("x", 64*1024)); err != nil {
		t.Fatalf("maximum valid prompt rejected: %v", err)
	}
}

func TestAttachRequiresExactRuntimeAndSessionIdentity(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.state.SessionID = "acp-session-a"
	stored := channel.AgentSession{Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
	if _, err := backend.Attach(context.Background(), conversation, stored); err != nil {
		t.Fatal(err)
	}
	connection.runtime.Incarnation = "another-incarnation"
	if _, err := backend.Attach(context.Background(), conversation, stored); err == nil {
		t.Fatal("changed Runtime incarnation was attached")
	}
}

func TestAttachLoadsPreviousSessionAfterDefinitiveRuntimeLoss(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.getErr = &api.Error{Code: "STALE_RUNTIME", Detail: "old process gone"}
	connection.state.CanLoad = true
	previous := channel.AgentSession{Runtime: channel.RuntimeHandle{ID: "old-runtime", Incarnation: "old-inc", Generation: 1, Adapter: "acp"}, ACPSessionID: "prior-session"}
	recovered, err := backend.Attach(context.Background(), conversation, previous)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Runtime.ID != "runtime-a" || recovered.ACPSessionID != "prior-session" || recovered.ContextLost || connection.starts != 1 || len(connection.actions) != 1 || connection.actions[0].Action != "load" {
		t.Fatalf("previous ACP session was not loaded: recovered=%+v starts=%d actions=%v", recovered, connection.starts, connection.actions)
	}
}

func TestAttachStartsFreshSessionAndSignalsContextLossWhenLoadUnsupported(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.getErr = &api.Error{Code: "STALE_RUNTIME", Detail: "old process gone"}
	previous := channel.AgentSession{Runtime: channel.RuntimeHandle{ID: "old-runtime", Incarnation: "old-inc", Generation: 1, Adapter: "acp"}, ACPSessionID: "prior-session"}
	recovered, err := backend.Attach(context.Background(), conversation, previous)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.ContextLost || recovered.ACPSessionID != "acp-session-a" || len(connection.actions) != 1 || connection.actions[0].Action != "new" {
		t.Fatalf("context loss was not reported: recovered=%+v actions=%v", recovered, connection.actions)
	}
}

func TestAttachDoesNotRestartOnUnknownRuntimeLookup(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.getErr = errors.New("transport interrupted")
	previous := channel.AgentSession{Runtime: channel.RuntimeHandle{ID: "old-runtime", Incarnation: "old-inc", Generation: 1, Adapter: "acp"}, ACPSessionID: "prior-session"}
	if _, err := backend.Attach(context.Background(), conversation, previous); err == nil || connection.starts != 0 {
		t.Fatalf("unknown lookup triggered a new Runtime: starts=%d err=%v", connection.starts, err)
	}
}

func TestPromptStreamsOnlyAssistantAnswerAndDoesNotDuplicateFullFinal(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.state.SessionID = "acp-session-a"
	connection.subscription = &fakeSubscription{messages: []*pb.Message{
		update("thought_message_chunk", "hidden"),
		update("agent_message_chunk", "hello"),
		update("tool_call", "tool output"),
		update("agent_message_chunk", " "),
		update("agent_message_chunk", "world"),
		update("agent_message", "hello world"),
		{Kind: "acp_state", Payload: jsonPayload(host.AgentState{Revision: 5, Ready: true, SessionID: "acp-session-a", StopReason: "end_turn"})},
	}}
	session := channel.AgentSession{Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
	var events []channel.AgentEvent
	final, err := backend.Prompt(context.Background(), conversation, session, "question", func(event channel.AgentEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil || final != "hello world" || !reflect.DeepEqual(events, []channel.AgentEvent{
		{Kind: channel.AgentDelta, Text: "hello"}, {Kind: channel.AgentDelta, Text: " "},
		{Kind: channel.AgentDelta, Text: "world"}, {Kind: channel.AgentFinal, Text: "hello world"},
	}) {
		t.Fatalf("assistant stream was contaminated or duplicated: final=%q events=%v err=%v", final, events, err)
	}
	if len(connection.actions) != 1 || connection.actions[0].Action != "prompt" || connection.actions[0].Text != "question" {
		t.Fatalf("prompt was not submitted once: %v", connection.actions)
	}
}

func TestPromptStreamInterruptionIsUnknownNotSuccess(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.state.SessionID = "acp-session-a"
	connection.subscription = &fakeSubscription{messages: []*pb.Message{update("agent_message_chunk", "partial")}}
	session := channel.AgentSession{Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
	if _, err := backend.Prompt(context.Background(), conversation, session, "question", func(channel.AgentEvent) error { return nil }); err == nil {
		t.Fatal("interrupted ACP stream was reported complete")
	}
}

func TestPromptFullAssistantMessageCanReviseStreamedDeltas(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.state.SessionID = "acp-session-a"
	connection.subscription = &fakeSubscription{messages: []*pb.Message{
		update("agent_message_chunk", "hel"),
		update("agent_message_chunk", "lo"),
		update("agent_message", "HELLO revised"),
		{Kind: "acp_state", Payload: jsonPayload(host.AgentState{Revision: 5, Ready: true, SessionID: "acp-session-a", StopReason: "end_turn"})},
	}}
	session := channel.AgentSession{Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
	var events []channel.AgentEvent
	final, err := backend.Prompt(context.Background(), conversation, session, "question", func(event channel.AgentEvent) error {
		events = append(events, event)
		return nil
	})
	want := []channel.AgentEvent{{Kind: channel.AgentDelta, Text: "hel"}, {Kind: channel.AgentDelta, Text: "lo"}, {Kind: channel.AgentFinal, Text: "HELLO revised"}}
	if err != nil || final != "HELLO revised" || !reflect.DeepEqual(events, want) {
		t.Fatalf("final revision was not authoritative: final=%q events=%v err=%v", final, events, err)
	}
}

func TestPromptIgnoresOutOfOrderACPCompletionState(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.state.SessionID = "acp-session-a"
	connection.subscription = &fakeSubscription{messages: []*pb.Message{
		update("agent_message_chunk", "partial"),
		{Kind: "acp_state", Payload: jsonPayload(host.AgentState{Revision: 5, Ready: true, Busy: "prompt", SessionID: "acp-session-a"})},
		{Kind: "acp_state", Payload: jsonPayload(host.AgentState{Revision: 4, Ready: true, SessionID: "acp-session-a", StopReason: "end_turn"})},
	}}
	session := channel.AgentSession{Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
	if _, err := backend.Prompt(context.Background(), conversation, session, "question", func(channel.AgentEvent) error { return nil }); err == nil {
		t.Fatal("out-of-order completion state ended the prompt")
	}
}

func update(kind, text string) *pb.Message {
	return &pb.Message{Kind: "acp_update", Payload: jsonPayload(map[string]any{"sessionId": "acp-session-a", "update": map[string]any{"sessionUpdate": kind, "content": map[string]string{"type": "text", "text": text}}})}
}

func jsonPayload(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
