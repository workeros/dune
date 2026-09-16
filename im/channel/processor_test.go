package channel_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/im/sqlite"
)

type fakeActiveBindings struct{ active channel.ActiveBinding }

func (f fakeActiveBindings) LookupBinding(_ context.Context, id string) (channel.ActiveBinding, error) {
	if id != f.active.Binding.ID {
		return channel.ActiveBinding{}, errors.New("unknown binding")
	}
	return f.active, nil
}

type fakeRoots struct{}

func (fakeRoots) ResolveGroup(_ context.Context, message channel.InboundMessage) (channel.GroupRoute, error) {
	if message.Address.Provider == "test" && string(message.Address.Data) != "" {
		var root string
		if err := json.Unmarshal(message.Address.Data, &root); err != nil {
			return channel.GroupRoute{}, err
		}
		return channel.GroupRoute{SubjectID: root}, nil
	}
	return channel.GroupRoute{SubjectID: message.MessageID}, nil
}

type fakeAdmission struct{ botID string }

func (f fakeAdmission) AddressedToBot(_ context.Context, message channel.InboundMessage) (bool, error) {
	return slices.Contains(message.BotMentionOpenIDs, f.botID), nil
}

type fakeReplyChannel struct {
	mu       sync.Mutex
	sends    []channel.OutboundMessage
	updates  []string
	complete []string
}

func (*fakeReplyChannel) Start(context.Context) error { return nil }
func (*fakeReplyChannel) Stop(context.Context) error  { return nil }
func (f *fakeReplyChannel) Send(_ context.Context, _ channel.ReplyAddress, message channel.OutboundMessage) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, message)
	return json.RawMessage(`{"message_id":"test"}`), nil
}
func (f *fakeReplyChannel) OpenStream(_ context.Context, _ channel.ReplyAddress, _ channel.OutboundMessage) (channel.ReplyStream, error) {
	return f, nil
}
func (f *fakeReplyChannel) Update(_ context.Context, message channel.OutboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, message.Text)
	return nil
}
func (f *fakeReplyChannel) Complete(_ context.Context, message channel.OutboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.complete = append(f.complete, message.Text)
	return nil
}

type fakeAgentBackend struct {
	caps                channel.AgentCapabilities
	starts              int
	attaches            int
	prompts             []string
	promptErr           error
	finalText           string
	contextLostOnAttach bool
}

func (f *fakeAgentBackend) Capabilities(context.Context, channel.ConversationSession) (channel.AgentCapabilities, error) {
	return f.caps, nil
}
func (f *fakeAgentBackend) Start(_ context.Context, _ channel.ConversationSession) (channel.AgentSession, error) {
	f.starts++
	id := fmt.Sprintf("runtime-%d", f.starts)
	return channel.AgentSession{Runtime: channel.RuntimeHandle{ID: id, Incarnation: "inc-1", Generation: 1, Adapter: "acp"}, ACPSessionID: id + "-acp"}, nil
}
func (f *fakeAgentBackend) Attach(_ context.Context, _ channel.ConversationSession, session channel.AgentSession) (channel.AgentSession, error) {
	f.attaches++
	session.ContextLost = f.contextLostOnAttach
	return session, nil
}
func (f *fakeAgentBackend) Prompt(_ context.Context, _ channel.ConversationSession, session channel.AgentSession, input string, emit func(channel.AgentEvent) error) (string, error) {
	f.prompts = append(f.prompts, session.Runtime.ID+":"+input)
	if f.promptErr != nil {
		return "", f.promptErr
	}
	if err := emit(channel.AgentEvent{Kind: channel.AgentDelta, Text: "hel"}); err != nil {
		return "", err
	}
	if err := emit(channel.AgentEvent{Kind: channel.AgentDelta, Text: "lo"}); err != nil {
		return "", err
	}
	final := f.finalText
	if final == "" {
		final = "hello"
	}
	if err := emit(channel.AgentEvent{Kind: channel.AgentFinal, Text: final}); err != nil {
		return "", err
	}
	return final, nil
}
func (*fakeAgentBackend) Stop(context.Context, channel.ConversationSession, channel.AgentSession) error {
	return nil
}

type rejectingInputBackend struct{ *fakeAgentBackend }

func (rejectingInputBackend) ValidateInput(string) error {
	return errors.New("deterministic prompt validation failure")
}

func testProcessor(t *testing.T, streaming bool) (channel.Processor, *sqlite.Store, *fakeAgentBackend, *fakeReplyChannel) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	backend := &fakeAgentBackend{caps: channel.AgentCapabilities{Adapter: "acp", AssistantDeltas: true, ReliableFinal: true}}
	replies := &fakeReplyChannel{}
	binding := channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Enabled: true, Target: channel.AgentTarget{RunnerID: "runner-a", AgentConfigID: "agent-a"}}
	processor := channel.Processor{Work: store, Conversations: store, Deliveries: store, Agents: backend,
		Bindings: fakeActiveBindings{active: channel.ActiveBinding{Binding: binding, Channel: replies,
			Subjects: fakeRoots{}, Admission: fakeAdmission{botID: "bot-open-id"}, Streaming: streaming}},
		ClaimLease: time.Minute, SessionLease: time.Minute}
	return processor, store, backend, replies
}

func TestProcessorIsolatesPrivateUsersAndReusesTheirOwnSessions(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, false)
	for _, input := range []struct{ event, sender string }{{"event-a1", "user-a"}, {"event-b1", "user-b"}, {"event-a2", "user-a"}} {
		message := channel.InboundMessage{BindingID: "bot-a", EventID: input.event, MessageID: input.event, ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: input.sender, Text: input.event}
		if err := store.Insert(ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if found, err := processor.ProcessOne(ctx); err != nil || !found {
			t.Fatalf("process private message: found=%t err=%v", found, err)
		}
	}
	if backend.starts != 2 || backend.attaches != 1 || len(replies.sends) != 3 ||
		backend.prompts[0] != "runtime-1:event-a1" || backend.prompts[1] != "runtime-2:event-b1" || backend.prompts[2] != "runtime-1:event-a2" {
		t.Fatalf("private users shared or lost a session: starts=%d attaches=%d prompts=%v sends=%v", backend.starts, backend.attaches, backend.prompts, replies.sends)
	}
	for _, sent := range replies.sends {
		delivery, found, err := store.GetDelivery(ctx, "bot-a", sent.DeliveryID)
		if err != nil || !found || delivery.Phase != "complete" || delivery.Mode != "final_text" || string(delivery.ProviderState) != `{"message_id":"test"}` {
			t.Fatalf("final reply not durably completed: %+v found=%t err=%v", delivery, found, err)
		}
	}
}

func TestProcessorRejectsDeterministicInputBeforeSubmissionBarrier(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, false)
	processor.Agents = rejectingInputBackend{backend}
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "invalid-input", MessageID: "invalid-input", ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user", Text: "invalid"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); !found || err == nil {
		t.Fatalf("deterministically invalid prompt was accepted: found=%t err=%v", found, err)
	}
	if backend.starts != 0 || len(backend.prompts) != 0 || len(replies.sends) != 0 {
		t.Fatalf("invalid prompt caused an external operation: starts=%d prompts=%v replies=%v", backend.starts, backend.prompts, replies.sends)
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "p2p", SubjectID: "user"}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationReady {
		t.Fatalf("invalid input made the session unknown: found=%t state=%s err=%v", found, state, err)
	}
	if item, found, err := store.Claim(ctx, time.Minute); err != nil || !found || item.Message.EventID != message.EventID {
		t.Fatalf("preflight failure crossed the submission barrier: item=%+v found=%t err=%v", item, found, err)
	}
}

func TestProcessorFinalCardUsesGenericDelivery(t *testing.T) {
	ctx := context.Background()
	processor, store, _, replies := testProcessor(t, false)
	active := processor.Bindings.(fakeActiveBindings)
	active.active.ReplyMode = "final_card"
	processor.Bindings = active
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "card-event", MessageID: "card-event", ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user", Text: "hello"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("process: %t %v", found, err)
	}
	if len(replies.sends) != 1 {
		t.Fatalf("sends=%d", len(replies.sends))
	}
	delivery, found, err := store.GetDelivery(ctx, "bot-a", replies.sends[0].DeliveryID)
	if err != nil || !found || delivery.Mode != "final_card" || delivery.Phase != "complete" {
		t.Fatalf("delivery: %+v %t %v", delivery, found, err)
	}
}

func TestProcessorStreamAcceptsAuthoritativeFinalRevision(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, true)
	backend.finalText = "HELLO revised"
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "revision-event", MessageID: "om-root", ChatKind: channel.ChatGroup,
		ChatID: "group-a", SenderID: "user-a", BotMentionOpenIDs: []string{"bot-open-id"}, Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("process: %t %v", found, err)
	}
	if len(replies.complete) != 1 || replies.complete[0] != "HELLO revised" {
		t.Fatalf("final revision not applied: %+v", replies.complete)
	}
}

func TestProcessorClosesStreamingCardAfterAgentFailureWithoutReplayingPrompt(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, true)
	backend.promptErr = errors.New("Agent connection lost")
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "failed-stream", MessageID: "om-root", ChatKind: channel.ChatGroup,
		ChatID: "group-a", SenderID: "user-a", BotMentionOpenIDs: []string{"bot-open-id"}, Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err == nil || !found {
		t.Fatalf("expected Agent failure: %t %v", found, err)
	}
	if len(replies.complete) != 1 || !strings.Contains(replies.complete[0], "结果未知") {
		t.Fatalf("failure card not closed: %v", replies.complete)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || found || len(backend.prompts) != 1 {
		t.Fatalf("unknown prompt replayed: found=%t err=%v prompts=%v", found, err, backend.prompts)
	}
}

func TestProcessorAdmitsMentionedGroupTopicAndSharesSessionAcrossMembers(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, true)
	first := channel.InboundMessage{BindingID: "bot-a", EventID: "event-1", MessageID: "om-root", ChatKind: channel.ChatGroup, ChatID: "group-a", SenderID: "user-a", BotMentionOpenIDs: []string{"bot-open-id"}, Text: "first"}
	second := channel.InboundMessage{BindingID: "bot-a", EventID: "event-2", MessageID: "om-reply", ChatKind: channel.ChatGroup, ChatID: "group-a", SenderID: "user-b", Text: "second", Address: channel.ReplyAddress{Provider: "test", Version: 1, Data: json.RawMessage(`"om-root"`)}}
	third := channel.InboundMessage{BindingID: "bot-a", EventID: "event-3", MessageID: "om-other", ChatKind: channel.ChatGroup, ChatID: "group-a", SenderID: "user-a", Text: "unmentioned"}
	for _, message := range []channel.InboundMessage{first, second, third} {
		if err := store.Insert(ctx, message); err != nil {
			t.Fatal(err)
		}
		if found, err := processor.ProcessOne(ctx); err != nil || !found {
			t.Fatalf("process group message: found=%t err=%v", found, err)
		}
	}
	if backend.starts != 1 || backend.attaches != 1 || len(backend.prompts) != 2 || len(replies.sends) != 0 ||
		len(replies.complete) != 2 || fmt.Sprint(replies.updates) != "[hel hello hel hello]" {
		t.Fatalf("group thread or streaming behavior wrong: starts=%d attaches=%d prompts=%v sends=%v updates=%v complete=%v", backend.starts, backend.attaches, backend.prompts, replies.sends, replies.updates, replies.complete)
	}
}

func TestProcessorRejectsPTYStreamingBeforeStartingAgent(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, true)
	backend.caps.Adapter = "pty"
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "event", MessageID: "om", ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user", Text: "hello"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.ProcessOne(ctx); err == nil {
		t.Fatal("PTY streaming configuration was accepted")
	}
	if backend.starts != 0 || len(replies.sends) != 0 {
		t.Fatalf("Agent or reply started despite invalid mode: starts=%d sends=%v", backend.starts, replies.sends)
	}
}

func TestProcessorDoesNotReplayUnknownPrompt(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, _ := testProcessor(t, false)
	backend.promptErr = errors.New("stream interrupted")
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "event", MessageID: "om", ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user", Text: "hello"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.ProcessOne(ctx); err == nil {
		t.Fatal("unknown prompt was reported as success")
	}
	if found, err := processor.ProcessOne(ctx); err != nil || found || len(backend.prompts) != 1 {
		t.Fatalf("unknown prompt was replayed: found=%t err=%v prompts=%v", found, err, backend.prompts)
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "p2p", SubjectID: "user"}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationUnknown {
		t.Fatalf("unknown session not fenced: state=%s found=%t err=%v", state, found, err)
	}
}

func TestProcessorReportsContextLossInSameFinalReply(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, false)
	for _, event := range []string{"first", "second"} {
		message := channel.InboundMessage{BindingID: "bot-a", EventID: event, MessageID: event, ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user", Text: event}
		if err := store.Insert(ctx, message); err != nil {
			t.Fatal(err)
		}
		if event == "second" {
			backend.contextLostOnAttach = true
		}
		if found, err := processor.ProcessOne(ctx); err != nil || !found {
			t.Fatalf("process %s: found=%t err=%v", event, found, err)
		}
	}
	if len(replies.sends) != 2 || replies.sends[0].Text != "hello" || !strings.Contains(replies.sends[1].Text, "上下文未恢复") || !strings.HasSuffix(replies.sends[1].Text, "hello") {
		t.Fatalf("context loss was not reported in final reply: %v", replies.sends)
	}
}
