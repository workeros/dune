package duneagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/runner"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type fakeScopes struct{ scope host.AgentScope }

func (f fakeScopes) ResolveAgentScope(context.Context, channel.ConversationSession) (host.AgentScope, error) {
	return f.scope, nil
}

type fakeProfiles struct{ profile api.Profile }

func (f fakeProfiles) ResolveAgentProfile(context.Context, channel.ConversationSession) (api.Profile, error) {
	return f.profile, nil
}

type fakeExecutor struct{ connection *fakeConnection }

func (f fakeExecutor) Open(context.Context, host.AgentScope) (host.AgentConnection, error) {
	return f.connection, nil
}

type fakeConnection struct {
	runtime       api.Runtime
	state         host.AgentState
	outputs       []api.AgentOperationOutput
	readStarted   chan struct{}
	readReturned  chan struct{}
	profile       api.Profile
	actions       []host.AgentAction
	getErr        error
	starts        int
	startErr      error
	launchQueries []string
	launchReceipt api.SubmissionReceipt
	stopErr       error
	stops         int
}

func (f *fakeConnection) Start(_ context.Context, submissionID string, profile api.Profile) (api.Runtime, error) {
	f.profile = profile
	f.starts++
	return f.runtime, f.startErr
}
func (f *fakeConnection) Get(context.Context, api.Runtime) (api.Runtime, error) {
	if f.getErr != nil {
		return api.Runtime{}, f.getErr
	}
	return f.runtime, nil
}
func (f *fakeConnection) Observe(context.Context, api.Runtime) (host.AgentSubscription, error) {
	return nil, errors.New("IM prompt must use operation output")
}
func (f *fakeConnection) State(context.Context, api.Runtime) (host.AgentState, error) {
	return f.state, nil
}
func (f *fakeConnection) Submit(_ context.Context, _ api.Runtime, action host.AgentAction) (api.AgentOperation, error) {
	f.actions = append(f.actions, action)
	if action.Action == "new" {
		f.state.SessionID = "acp-session-a"
	}
	if action.Action == "load" {
		f.state.SessionID = action.SessionID
	}
	if action.Action == "prompt" {
		return api.AgentOperation{Ref: "prompt-operation", State: "pending"}, nil
	}
	return api.AgentOperation{ConversationID: "conversation-a", Ref: "lifecycle-operation", State: "completed", NativeSession: &api.NativeSession{ID: f.state.SessionID}}, nil
}
func (f *fakeConnection) WaitOperation(ctx context.Context, _ api.Runtime, request api.AgentOperationWait) (api.AgentOperation, error) {
	return api.AgentOperation{Ref: request.Ref, State: "running"}, ctx.Err()
}
func (f *fakeConnection) ReadOperation(ctx context.Context, _ api.Runtime, request api.AgentOperationRead) (api.AgentOperationOutput, error) {
	if f.readStarted != nil {
		close(f.readStarted)
		<-ctx.Done()
		close(f.readReturned)
		return api.AgentOperationOutput{}, ctx.Err()
	}
	if len(f.outputs) == 0 {
		return api.AgentOperationOutput{}, io.ErrUnexpectedEOF
	}
	output := f.outputs[0]
	f.outputs = f.outputs[1:]
	return output, nil
}
func (f *fakeConnection) Stop(_ context.Context, runtime api.Runtime, id string) (api.SubmissionReceipt, error) {
	f.stops++
	receipt, _ := f.QuerySubmission(context.Background(), runtime, id)
	return receipt, f.stopErr
}
func (f *fakeConnection) QuerySubmission(_ context.Context, runtime api.Runtime, id string) (api.SubmissionReceipt, error) {
	if runtime.ID == "" {
		f.launchQueries = append(f.launchQueries, id)
		return f.launchReceipt, nil
	}
	stage := "stopping"
	if f.runtime.State == "exited" {
		stage = "stopped"
	}
	if runtime.Generation != f.runtime.Generation {
		return api.SubmissionReceipt{}, errors.New("changed identity")
	}
	return api.SubmissionReceipt{SubmissionKey: api.SubmissionKey{SubmissionID: id}, Admission: api.SubmissionAccepted, Stage: stage}, f.getErr
}
func (*fakeConnection) Close() error { return nil }

func testBackend() (Backend, channel.ConversationSession, *fakeConnection) {
	connection := &fakeConnection{
		runtime: api.Runtime{ID: "runtime-a", Incarnation: "inc-a", Generation: 2, Adapter: "acp", State: "running"},
		state:   host.AgentState{Ready: true, Revision: 3, Conversation: &api.ACPConversation{ID: "conversation-a"}},
	}
	conversation := channel.ConversationSession{Key: channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a"}, Target: channel.AgentTarget{OwnerID: "dune-owner-a", RunnerID: "runner-a", FabricID: "fabric-a", MachineID: "machine-a", RunnerBindingRevision: 1, ProfileID: "agent-a", ProfileRevision: 1, WorkingDirectory: "/tmp/im-test"}}
	profile := api.Profile{Version: 1, Kind: "agent", Adapter: "acp", WorkingDirectory: "/tmp", Start: api.Command{Argv: []string{"/bin/echo", "--acp"}}, Env: map[string]string{"TEAM": "infra"}}
	profile.Setup.Steps = []api.Command{{Argv: []string{"prepare-agent"}}}
	backend := Backend{Executor: fakeExecutor{connection: connection}, Scopes: fakeScopes{scope: host.AgentScope{OwnerID: "dune-owner-a", Binding: runner.Binding{RunnerID: "runner-a", FabricID: "fabric-a", MachineID: "machine-a", Revision: 1}}}, Profiles: fakeProfiles{profile: profile}}
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
	selected := backend.Profiles.(fakeProfiles).profile
	selected.WorkingDirectory, selected.ManagedACP = conversation.Target.WorkingDirectory, true
	if !reflect.DeepEqual(connection.profile, selected) {
		t.Fatalf("Profile setup, start or environment changed: got %+v want %+v", connection.profile, selected)
	}
}

func TestBackendRejectsMissingOwnerOrWrongRunnerScope(t *testing.T) {
	backend, conversation, _ := testBackend()
	backend.Scopes = fakeScopes{scope: host.AgentScope{Binding: runner.Binding{RunnerID: "runner-a"}}}
	if _, err := backend.Capabilities(context.Background(), conversation); err == nil {
		t.Fatal("Agent scope without a Dune Owner accepted")
	}
	backend.Scopes = fakeScopes{scope: host.AgentScope{OwnerID: "dune-owner-a", Binding: runner.Binding{RunnerID: "other-runner"}}}
	if _, err := backend.Start(context.Background(), conversation); err == nil {
		t.Fatal("different Runner Agent scope accepted")
	}
}

func TestUnknownLaunchOnlyQueriesOriginalBindingAndNeverOpensSession(t *testing.T) {
	backend, conversation, connection := testBackend()
	id := LaunchSubmissionID(conversation)
	connection.startErr = io.EOF
	if _, err := backend.Start(t.Context(), conversation); err == nil {
		t.Fatal("lost launch response reported success")
	}
	key := api.SubmissionKey{SubmissionID: id, Target: api.SubmissionTarget{OwnerID: "dune-owner-a", RunnerID: conversation.Target.RunnerID, FabricID: conversation.Target.FabricID, MachineID: conversation.Target.MachineID, BindingRevision: conversation.Target.RunnerBindingRevision}}
	connection.launchReceipt = api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionAccepted, Runtime: &connection.runtime, Stage: "started"}
	backend.Profiles = nil // The read must not reconstruct launch configuration.
	for range 2 {
		receipt, err := backend.QueryLaunch(t.Context(), conversation, id)
		if err != nil || receipt.SubmissionKey != key || receipt.Admission != api.SubmissionAccepted || receipt.Runtime.ID != connection.runtime.ID {
			t.Fatal("original launch read", receipt, err)
		}
	}
	if connection.starts != 1 || len(connection.actions) != 0 || !reflect.DeepEqual(connection.launchQueries, []string{id, id}) {
		t.Fatal("query repeated startup or new", connection.starts, connection.actions, connection.launchQueries)
	}
	scope := backend.Scopes.(fakeScopes)
	scope.scope.Binding.Revision++
	backend.Scopes = scope
	if _, err := backend.QueryLaunch(t.Context(), conversation, id); err == nil || len(connection.launchQueries) != 2 {
		t.Fatal("query followed replacement binding", err)
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
	stored := channel.AgentSession{ConversationID: "conversation-a", Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
	if _, err := backend.Attach(context.Background(), conversation, stored); err != nil {
		t.Fatal(err)
	}
	connection.runtime.Incarnation = "another-incarnation"
	if _, err := backend.Attach(context.Background(), conversation, stored); err == nil {
		t.Fatal("changed Runtime incarnation was attached")
	}
}

func TestAttachNeverReplacesEndedOrUnavailableRuntime(t *testing.T) {
	for _, outcome := range []string{"stale", "exited", "lost", "unavailable"} {
		for _, canLoad := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/load-%t", outcome, canLoad), func(t *testing.T) {
				backend, conversation, connection := testBackend()
				connection.state.CanLoad = canLoad
				previous := channel.AgentSession{ConversationID: "conversation-a", Runtime: toHandle(connection.runtime), ACPSessionID: "prior-session"}
				switch outcome {
				case "stale":
					connection.getErr = &api.Error{Code: "STALE_RUNTIME", Detail: "original host cannot be verified"}
				case "unavailable":
					connection.runtime.Availability = "unavailable"
				default:
					connection.runtime.State = outcome
				}
				if _, err := backend.Attach(context.Background(), conversation, previous); err == nil {
					t.Fatal("unavailable original Runtime was attached")
				}
				if connection.starts != 0 || len(connection.actions) != 0 {
					t.Fatal("attachment replaced the Agent or submitted new/load", connection.starts, connection.actions)
				}
			})
		}
	}
}

func TestAttachDoesNotRestartOnUnknownRuntimeLookup(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.getErr = errors.New("transport interrupted")
	previous := channel.AgentSession{ConversationID: "conversation-a", Runtime: channel.RuntimeHandle{ID: "old-runtime", Incarnation: "old-inc", Generation: 1, Adapter: "acp"}, ACPSessionID: "prior-session"}
	if _, err := backend.Attach(context.Background(), conversation, previous); err == nil || connection.starts != 0 {
		t.Fatalf("unknown lookup triggered a new Runtime: starts=%d err=%v", connection.starts, err)
	}
}

func TestStopRequiresConfirmedExitWithoutReplayingRequest(t *testing.T) {
	for _, scenario := range []string{"exited", "stale", "query-failed", "stop-failed", "still-running", "identity-changed"} {
		t.Run(scenario, func(t *testing.T) {
			backend, conversation, connection := testBackend()
			session := channel.AgentSession{ConversationID: "conversation-a", Runtime: toHandle(connection.runtime), ACPSessionID: "session-a"}
			switch scenario {
			case "exited":
				connection.runtime.State = "exited"
			case "stale":
				connection.getErr = &api.Error{Code: "STALE_RUNTIME"}
			case "query-failed":
				connection.getErr = io.ErrUnexpectedEOF
			case "stop-failed":
				connection.stopErr = io.ErrUnexpectedEOF
			case "identity-changed":
				connection.runtime.Generation++
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := backend.Stop(ctx, conversation, session)
			wantSuccess := scenario == "exited"
			if (err == nil) != wantSuccess || connection.stops != 1 {
				t.Fatalf("stop must be sent once and confirmed: calls=%d err=%v", connection.stops, err)
			}
			if scenario == "still-running" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("stop did not respect the caller deadline: %v", err)
			}
		})
	}
}

func TestPromptStreamsOnlyAssistantAnswerAndDoesNotDuplicateFullFinal(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.state.SessionID = "acp-session-a"
	connection.outputs = []api.AgentOperationOutput{outputPage("completed",
		update("thought_message_chunk", "hidden"),
		update("agent_message_chunk", "hello"),
		update("tool_call", "tool output"),
		update("agent_message_chunk", " "),
		update("agent_message_chunk", "world"),
		update("agent_message", "hello world"),
	)}
	session := channel.AgentSession{ConversationID: "conversation-a", Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
	var events []channel.AgentEvent
	final, err := backend.Prompt(context.Background(), conversation, session, channel.PromptRequest{SubmissionID: "caller-prompt", Text: "question"}, func(event channel.AgentEvent) error {
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

func TestPromptOperationReadInterruptionIsUnknownNotSuccess(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.state.SessionID = "acp-session-a"
	connection.outputs = []api.AgentOperationOutput{outputPage("running", update("agent_message_chunk", "partial"))}
	session := channel.AgentSession{ConversationID: "conversation-a", Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
	if _, err := backend.Prompt(context.Background(), conversation, session, channel.PromptRequest{SubmissionID: "caller-prompt", Text: "question"}, func(channel.AgentEvent) error { return nil }); err == nil {
		t.Fatal("interrupted ACP stream was reported complete")
	}
}

func TestPromptCancellationStopsOperationRead(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.state.SessionID = "acp-session-a"
	connection.readStarted, connection.readReturned = make(chan struct{}), make(chan struct{})
	session := channel.AgentSession{ConversationID: "conversation-a", Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := backend.Prompt(ctx, conversation, session, channel.PromptRequest{SubmissionID: "caller-prompt", Text: "question"}, func(channel.AgentEvent) error { return nil })
		done <- err
	}()
	select {
	case <-connection.readStarted:
	case <-time.After(time.Second):
		t.Fatal("operation read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("operation read stayed blocked")
	}
	select {
	case <-connection.readReturned:
	default:
		t.Fatal("read not canceled")
	}
}

func TestPromptFullAssistantMessageCanReviseStreamedDeltas(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.state.SessionID = "acp-session-a"
	connection.outputs = []api.AgentOperationOutput{outputPage("completed",
		update("agent_message_chunk", "hel"),
		update("agent_message_chunk", "lo"),
		update("agent_message", "HELLO revised"),
	)}
	session := channel.AgentSession{ConversationID: "conversation-a", Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
	var events []channel.AgentEvent
	final, err := backend.Prompt(context.Background(), conversation, session, channel.PromptRequest{SubmissionID: "caller-prompt", Text: "question"}, func(event channel.AgentEvent) error {
		events = append(events, event)
		return nil
	})
	want := []channel.AgentEvent{{Kind: channel.AgentDelta, Text: "hel"}, {Kind: channel.AgentDelta, Text: "lo"}, {Kind: channel.AgentFinal, Text: "HELLO revised"}}
	if err != nil || final != "HELLO revised" || !reflect.DeepEqual(events, want) {
		t.Fatalf("final revision was not authoritative: final=%q events=%v err=%v", final, events, err)
	}
}

func TestPromptRejectsOutputFromAnotherOperationAndGaps(t *testing.T) {
	for _, scenario := range []string{"reference", "incomplete", "position"} {
		t.Run(scenario, func(t *testing.T) {
			backend, conversation, connection := testBackend()
			connection.state.SessionID = "acp-session-a"
			output := outputPage("completed", update("agent_message_chunk", "wrong answer"))
			switch scenario {
			case "reference":
				output.Ref = "previous-operation"
			case "incomplete":
				output.Incomplete = true
			case "position":
				output.Position = 1
			}
			connection.outputs = []api.AgentOperationOutput{output}
			session := channel.AgentSession{ConversationID: "conversation-a", Runtime: toHandle(connection.runtime), ACPSessionID: "acp-session-a"}
			var emitted bool
			_, err := backend.Prompt(context.Background(), conversation, session, channel.PromptRequest{SubmissionID: "caller-prompt", Text: "question"}, func(channel.AgentEvent) error { emitted = true; return nil })
			if err == nil || emitted {
				t.Fatal("unrelated/incomplete output was accepted")
			}
		})
	}
}

func outputPage(state string, updates ...*pb.Message) api.AgentOperationOutput {
	output := api.AgentOperationOutput{AgentOperation: api.AgentOperation{Ref: "prompt-operation", State: state, StopReason: "end_turn"}, NextPosition: int64(len(updates))}
	for _, update := range updates {
		output.Output = append(output.Output, update.Payload)
	}
	return output
}

func update(kind, text string) *pb.Message {
	return &pb.Message{Kind: "acp_update", Payload: jsonPayload(map[string]any{"sessionId": "acp-session-a", "update": map[string]any{"sessionUpdate": kind, "content": map[string]string{"type": "text", "text": text}}})}
}

func jsonPayload(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func TestPromptKeepsStoredGenerationAfterRuntimeRefresh(t *testing.T) {
	backend, conversation, connection := testBackend()
	connection.runtime.ConversationID = "fresh-c2"
	connection.outputs = []api.AgentOperationOutput{{AgentOperation: api.AgentOperation{Ref: "prompt-operation", State: "completed"}}}
	_, _ = backend.Prompt(context.Background(), conversation, channel.AgentSession{Runtime: toHandle(connection.runtime), ACPSessionID: "session-a", ConversationID: "observed-c1"}, channel.PromptRequest{SubmissionID: "caller-prompt", Text: "task"}, func(channel.AgentEvent) error { return nil })
	if len(connection.actions) != 1 || connection.actions[0].ExpectedConversationID != "observed-c1" || connection.actions[0].SubmissionID != "caller-prompt" {
		t.Fatal("IM rebound the pending prompt to fresh Runtime metadata")
	}
}

func TestPromptRejectsMissingCallerIDBeforeOpeningConnection(t *testing.T) {
	backend := Backend{}
	_, err := backend.Prompt(context.Background(), channel.ConversationSession{}, channel.AgentSession{}, channel.PromptRequest{Text: "task"}, func(channel.AgentEvent) error { return nil })
	var invalid *api.Error
	if !errors.As(err, &invalid) || invalid.Code != "INVALID_ARGUMENT" {
		t.Fatal("missing ID reached the unconfigured connector", err)
	}
}
