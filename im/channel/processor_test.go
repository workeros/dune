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
	return slices.Contains(message.MentionedIDs, f.botID), nil
}

type fakeReplyChannel struct {
	mu       sync.Mutex
	sends    []channel.OutboundMessage
	updates  []string
	complete []string
	outcomes []bool
}

type cancelAfterSendChannel struct {
	*fakeReplyChannel
	cancel context.CancelFunc
}

type rejectingReplyChannel struct{ *fakeReplyChannel }

func (rejectingReplyChannel) Send(context.Context, channel.ReplyAddress, channel.OutboundMessage) (json.RawMessage, error) {
	return nil, channel.RejectOutbound(errors.New("reply exceeds local provider limit"))
}

func (c cancelAfterSendChannel) Send(ctx context.Context, address channel.ReplyAddress, message channel.OutboundMessage) (json.RawMessage, error) {
	result, err := c.fakeReplyChannel.Send(ctx, address, message)
	c.cancel()
	return result, err
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
	f.outcomes = append(f.outcomes, message.AgentTurnCompleted)
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

type rotatingBackend struct {
	*fakeAgentBackend
	rotate func() error
}

type slowCapabilityBackend struct {
	*fakeAgentBackend
	once sync.Once
}

func (b *slowCapabilityBackend) Capabilities(ctx context.Context, session channel.ConversationSession) (channel.AgentCapabilities, error) {
	b.once.Do(func() { time.Sleep(1200 * time.Millisecond) })
	return b.fakeAgentBackend.Capabilities(ctx, session)
}

type waitingPromptBackend struct {
	*fakeAgentBackend
	started chan struct{}
	release chan struct{}
}

func (b waitingPromptBackend) Prompt(ctx context.Context, _ channel.ConversationSession, _ channel.AgentSession, _ string, _ func(channel.AgentEvent) error) (string, error) {
	close(b.started)
	select {
	case <-b.release:
		return "hello", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type trackingConversationStore struct {
	channel.ConversationStore
	renewed chan struct{}
	fail    bool
}

func (s trackingConversationStore) Renew(ctx context.Context, lease channel.ConversationLease, duration time.Duration) error {
	if s.fail {
		return errors.New("simulated lease renewal failure")
	}
	if err := s.ConversationStore.Renew(ctx, lease, duration); err != nil {
		return err
	}
	select {
	case s.renewed <- struct{}{}:
	default:
	}
	return nil
}

func (b rotatingBackend) Capabilities(ctx context.Context, session channel.ConversationSession) (channel.AgentCapabilities, error) {
	if err := b.rotate(); err != nil {
		return channel.AgentCapabilities{}, err
	}
	return b.fakeAgentBackend.Capabilities(ctx, session)
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
	binding := channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Enabled: true, Target: channel.AgentTarget{RunnerID: "runner-a", ProfileID: "agent-a", ProfileRevision: 1}}
	processor := channel.Processor{Work: store, Conversations: store, Deliveries: store, Agents: backend,
		Bindings: fakeActiveBindings{active: channel.ActiveBinding{Binding: binding, Channel: replies,
			Subjects: fakeRoots{}, Admission: fakeAdmission{botID: "bot-open-id"}, Streaming: streaming}},
		ClaimLease: time.Minute, SessionLease: time.Minute}
	return processor, store, backend, replies
}

func TestProcessorRenewsLeaseDuringSlowAgentTurn(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, _ := testProcessor(t, false)
	processor.SessionLease = time.Second
	renewed := make(chan struct{}, 1)
	processor.Conversations = trackingConversationStore{ConversationStore: store, renewed: renewed}
	started, release := make(chan struct{}), make(chan struct{})
	processor.Agents = waitingPromptBackend{fakeAgentBackend: backend, started: started, release: release}
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "slow-event", MessageID: "slow-message", ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user-a", Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := processor.ProcessOne(ctx)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Agent prompt never started")
	}
	select {
	case <-renewed:
	case <-time.After(5 * time.Second):
		t.Fatal("slow Agent turn did not renew its conversation lease")
	}
	// The first lease would have expired by now without the heartbeat.
	time.Sleep(time.Second)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow Agent turn did not finish")
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "p2p", SubjectID: "user-a"}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationReady {
		t.Fatalf("completed slow turn: state=%s found=%t err=%v", state, found, err)
	}
}

func TestProcessorBusyConversationPreservesFIFOWithoutFailingQueuedEvents(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.OpenWithOptions(ctx, filepath.Join(t.TempDir(), "im.db"), sqlite.Options{MaxClaimAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend := &fakeAgentBackend{caps: channel.AgentCapabilities{Adapter: "acp", AssistantDeltas: true, ReliableFinal: true}}
	replies := &fakeReplyChannel{}
	binding := channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Enabled: true,
		Target: channel.AgentTarget{RunnerID: "runner", ProfileID: "agent", ProfileRevision: 1}}
	processor := channel.Processor{Work: store, Conversations: store, Deliveries: store,
		Bindings:   fakeActiveBindings{active: channel.ActiveBinding{Binding: binding, Channel: replies}},
		ClaimLease: time.Minute, SessionLease: time.Minute}
	started, release := make(chan struct{}), make(chan struct{})
	processor.Agents = waitingPromptBackend{fakeAgentBackend: backend, started: started, release: release}
	for _, id := range []string{"first", "second", "third"} {
		if err := store.Insert(ctx, channel.InboundMessage{BindingID: binding.ID, EventID: id, MessageID: id,
			ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user", Text: id}); err != nil {
			t.Fatal(err)
		}
	}
	firstDone := make(chan error, 1)
	go func() { _, err := processor.ProcessOne(ctx); firstDone <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first Agent turn did not start")
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("busy second event was not deferred: found=%t err=%v", found, err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("later same-session event was not deferred: found=%t err=%v", found, err)
	}
	if stats, err := store.BindingStats(ctx, binding.ID); err != nil || stats.FailedEvents != 0 || stats.Queued != 2 {
		t.Fatalf("normal session contention failed its queued event: %+v err=%v", stats, err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	processor.Agents = backend
	time.Sleep(1100 * time.Millisecond)
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("deferred second event was not processed: found=%t err=%v", found, err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("deferred third event was not processed: found=%t err=%v", found, err)
	}
	if backend.starts != 1 || backend.attaches != 2 || len(backend.prompts) != 2 ||
		!strings.HasSuffix(backend.prompts[0], ":second") || !strings.HasSuffix(backend.prompts[1], ":third") || len(replies.sends) != 3 {
		t.Fatalf("busy conversation was replayed or dropped: starts=%d attaches=%d prompts=%v sends=%d", backend.starts, backend.attaches, backend.prompts, len(replies.sends))
	}
}

func TestProcessorDoesNotBlockIndependentGroupTopics(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, false)
	started, release := make(chan struct{}), make(chan struct{})
	processor.Agents = waitingPromptBackend{fakeAgentBackend: backend, started: started, release: release}
	for _, id := range []string{"om_topic_one", "om_topic_two"} {
		if err := store.Insert(ctx, channel.InboundMessage{BindingID: "bot-a", EventID: id, MessageID: id,
			ChatKind: channel.ChatGroup, ChatID: "oc_group", SenderID: "ou_user", MentionedIDs: []string{"bot-open-id"}, Text: id}); err != nil {
			t.Fatal(err)
		}
	}
	firstDone := make(chan error, 1)
	go func() { _, err := processor.ProcessOne(ctx); firstDone <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first group topic did not start")
	}
	processor.Agents = backend // the first ProcessOne owns its copy of the waiting backend
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("independent group topic was blocked: found=%t err=%v", found, err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if backend.starts != 2 || len(replies.sends) != 2 {
		t.Fatalf("group topics did not progress independently: starts=%d sends=%d", backend.starts, len(replies.sends))
	}
}

func TestProcessorRetriesExpiredClaimBeforeAgentSubmission(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, _ := testProcessor(t, false)
	processor.ClaimLease = time.Second
	processor.Agents = &slowCapabilityBackend{fakeAgentBackend: backend}
	active := processor.Bindings.(fakeActiveBindings).active
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: "feishu", ConfigVersion: 1,
		Config: json.RawMessage(`{"app_id":"cli_test"}`), CredentialRef: "secret", Target: active.Binding.Target, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	active.Binding = binding
	processor.Bindings = fakeActiveBindings{active: active}
	message := channel.InboundMessage{BindingID: binding.ID, BindingRevision: binding.Revision, EventID: "slow-preflight", MessageID: "slow-preflight",
		ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user-a", Text: "question"}
	if err := store.InsertForBinding(ctx, binding, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("expired pre-submission claim was not safely deferred: found=%t err=%v", found, err)
	}
	if backend.starts != 0 {
		t.Fatalf("expired claim started an Agent: %d", backend.starts)
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: binding.ID, ChatID: "p2p", SubjectID: "user-a"}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationReady {
		t.Fatalf("pre-submission failure stranded session: state=%s found=%t err=%v", state, found, err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found || backend.starts != 1 || len(backend.prompts) != 1 {
		t.Fatalf("queued event was not safely retried: found=%t starts=%d prompts=%v err=%v", found, backend.starts, backend.prompts, err)
	}
}

func TestProcessorFencesTurnWhenLeaseRenewalFails(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, _ := testProcessor(t, false)
	processor.SessionLease = time.Second
	processor.Conversations = trackingConversationStore{ConversationStore: store, fail: true}
	started := make(chan struct{})
	processor.Agents = waitingPromptBackend{fakeAgentBackend: backend, started: started, release: make(chan struct{})}
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "renew-failed", MessageID: "renew-failed", ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user-a", Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err == nil || !found {
		t.Fatalf("failed renewal was not reported: found=%t err=%v", found, err)
	}
	select {
	case <-started:
	default:
		t.Fatal("test did not enter an in-flight prompt")
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "p2p", SubjectID: "user-a"}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationUnknown {
		t.Fatalf("failed renewal did not fence the turn: state=%s found=%t err=%v", state, found, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("failed renewal made prompt replayable: found=%t err=%v", found, err)
	}
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
		if err != nil || !found || delivery.Phase != "complete" || !delivery.AgentTurnCompleted || delivery.Mode != "final_text" || string(delivery.ProviderState) != `{"message_id":"test"}` {
			t.Fatalf("final reply not durably completed: %+v found=%t err=%v", delivery, found, err)
		}
	}
}

func TestProcessorDoesNotRunQueuedEventUnderNewBindingRevision(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, false)
	active := processor.Bindings.(fakeActiveBindings).active
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: "feishu", ConfigVersion: 1,
		Config: json.RawMessage(`{"app_id":"cli_test"}`), CredentialRef: "secret", Target: active.Binding.Target, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	binding.Target.ProfileID = "new-agent"
	binding, err = store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	active.Binding = binding
	processor.Bindings = fakeActiveBindings{active: active}
	old := channel.InboundMessage{BindingID: "bot-a", BindingRevision: 1, EventID: "old", MessageID: "old", ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user", Text: "old question"}
	if err := store.Insert(ctx, old); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("discard old revision: found=%t err=%v", found, err)
	}
	if backend.starts != 0 || len(replies.sends) != 0 {
		t.Fatal("old event ran against the new Agent target")
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "p2p", SubjectID: "user"}
	if _, _, exists, err := store.Get(ctx, key); err != nil || exists {
		t.Fatalf("old event created a conversation: exists=%t err=%v", exists, err)
	}
	current := old
	current.BindingRevision, current.EventID, current.MessageID = 2, "current", "current"
	if err := store.Insert(ctx, current); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found || backend.starts != 1 || len(replies.sends) != 1 {
		t.Fatalf("new revision did not run normally: found=%t starts=%d sends=%d err=%v", found, backend.starts, len(replies.sends), err)
	}
}

func TestProcessorKeepsEstablishedAgentTargetAfterBindingRotation(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, false)
	active := processor.Bindings.(fakeActiveBindings).active
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: "feishu", ConfigVersion: 1,
		Config: json.RawMessage(`{"app_id":"cli_test"}`), CredentialRef: "secret", Target: active.Binding.Target, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	active.Binding = binding
	processor.Bindings = fakeActiveBindings{active: active}
	message := channel.InboundMessage{BindingID: "bot-a", BindingRevision: binding.Revision, EventID: "first", MessageID: "first",
		ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user", Text: "first question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("first Agent turn: found=%t err=%v", found, err)
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "p2p", SubjectID: "user"}
	before, _, exists, err := store.Get(ctx, key)
	if err != nil || !exists || before.Runtime.ID == "" {
		t.Fatalf("first turn did not establish Runtime: %+v exists=%t err=%v", before, exists, err)
	}
	binding.Target.ProfileID = "replacement-agent"
	binding, err = store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	active.Binding = binding
	processor.Bindings = fakeActiveBindings{active: active}
	message.BindingRevision, message.EventID, message.MessageID, message.Text = binding.Revision, "second", "second", "second question"
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("new Binding revision's turn: found=%t err=%v", found, err)
	}
	after, _, exists, err := store.Get(ctx, key)
	if err != nil || !exists || after.Target != before.Target || after.Runtime != before.Runtime || after.ACPSessionID != before.ACPSessionID {
		t.Fatalf("established conversation changed Agent target or Runtime: before=%+v after=%+v exists=%t err=%v", before, after, exists, err)
	}
	if backend.starts != 1 || backend.attaches != 1 || len(backend.prompts) != 2 || len(replies.sends) != 2 {
		t.Fatalf("rotated Binding did not reuse original Agent session: starts=%d attaches=%d prompts=%v sends=%d",
			backend.starts, backend.attaches, backend.prompts, len(replies.sends))
	}
}

func TestProcessorRecordsKnownOutboundRejectionWithoutUnknownTurn(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, false)
	active := processor.Bindings.(fakeActiveBindings).active
	active.Channel = rejectingReplyChannel{fakeReplyChannel: replies}
	processor.Bindings = fakeActiveBindings{active: active}
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "first", MessageID: "first", ChatKind: channel.ChatDirect,
		ChatID: "p2p", SenderID: "user", Text: "question"}
	for _, eventID := range []string{"first", "second"} {
		message.EventID, message.MessageID = eventID, eventID
		if err := store.Insert(ctx, message); err != nil {
			t.Fatal(err)
		}
		if found, err := processor.ProcessOne(ctx); !found || err == nil || !strings.Contains(err.Error(), "reply rejected without an external request") {
			t.Fatalf("known outbound rejection: found=%t err=%v", found, err)
		}
		delivery, exists, err := store.GetDelivery(ctx, "bot-a", channel.TurnDeliveryID("bot-a", eventID))
		if err != nil || !exists || delivery.Phase != "failed" || delivery.Operation != "" || !delivery.AgentTurnCompleted {
			t.Fatalf("known rejection was not durable: %+v exists=%t err=%v", delivery, exists, err)
		}
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "p2p", SubjectID: "user"}
	if _, state, exists, err := store.Get(ctx, key); err != nil || !exists || state != channel.ConversationReady {
		t.Fatalf("known rejection blocked the next turn: state=%s exists=%t err=%v", state, exists, err)
	}
	stats, err := store.BindingStats(ctx, "bot-a")
	if err != nil || stats.FailedEvents != 2 || stats.FailedDeliveries != 2 || stats.UnknownEvents != 0 || stats.UnknownConversations != 0 || stats.UnknownDeliveries != 0 {
		t.Fatalf("known rejection counted as unknown: %+v err=%v", stats, err)
	}
	issues, err := store.ListIssues(ctx, "bot-a", 10)
	if err != nil || len(issues.Events) != 2 || len(issues.Deliveries) != 2 || len(issues.Conversations) != 0 {
		t.Fatalf("known failures were not observable: %+v err=%v", issues, err)
	}
	for _, issue := range issues.Deliveries {
		if issue.Phase != "failed" || !issue.AgentTurnCompleted || issue.Operation != "" {
			t.Fatalf("rejected delivery issue is not terminal: %+v", issue)
		}
	}
	if backend.starts != 1 || backend.attaches != 1 || len(backend.prompts) != 2 || len(replies.sends) != 0 {
		t.Fatalf("unexpected Agent or platform activity: starts=%d attaches=%d prompts=%v sends=%d",
			backend.starts, backend.attaches, backend.prompts, len(replies.sends))
	}
}

func TestProcessorFencesBindingRotationAtSubmissionBarrier(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, false)
	active := processor.Bindings.(fakeActiveBindings).active
	config := json.RawMessage(`{"app_id":"cli_test"}`)
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: "feishu", ConfigVersion: 1,
		Config: config, CredentialRef: "secret", Target: active.Binding.Target, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	active.Binding = binding
	processor.Bindings = fakeActiveBindings{active: active}
	var newBinding channel.BotBinding
	processor.Agents = rotatingBackend{fakeAgentBackend: backend, rotate: func() error {
		binding.Target.ProfileID = "new-agent"
		newBinding, err = store.Put(ctx, binding)
		return err
	}}
	message := channel.InboundMessage{BindingID: "bot-a", BindingRevision: binding.Revision, EventID: "event", MessageID: "event", ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user", Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("rotated Binding was not cleanly discarded: found=%t err=%v", found, err)
	}
	if backend.starts != 0 || len(backend.prompts) != 0 || len(replies.sends) != 0 {
		t.Fatalf("stale event crossed Agent submission: starts=%d prompts=%v sends=%v", backend.starts, backend.prompts, replies.sends)
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "p2p", SubjectID: "user"}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationReady {
		t.Fatalf("provisional conversation was not unlocked: state=%s found=%t err=%v", state, found, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("stale event remained claimable: found=%t err=%v", found, err)
	}
	active.Binding = newBinding
	processor.Bindings = fakeActiveBindings{active: active}
	processor.Agents = backend
	current := message
	current.BindingRevision, current.EventID, current.MessageID = newBinding.Revision, "current", "current"
	if err := store.Insert(ctx, current); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found || backend.starts != 1 {
		t.Fatalf("current revision did not start Agent: found=%t starts=%d err=%v", found, backend.starts, err)
	}
	if session, _, found, err := store.Get(ctx, key); err != nil || !found || session.Target.ProfileID != "new-agent" {
		t.Fatalf("empty session retained stale Agent target: %+v found=%t err=%v", session, found, err)
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

func TestProcessorPersistsConfirmedSendAfterRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	processor, store, backend, replies := testProcessor(t, false)
	active := processor.Bindings.(fakeActiveBindings)
	active.active.Channel = cancelAfterSendChannel{fakeReplyChannel: replies, cancel: cancel}
	processor.Bindings = active
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "cancel-after-send", MessageID: "om-input", ChatKind: channel.ChatDirect,
		ChatID: "p2p", SenderID: "user", Text: "hello"}
	if err := store.Insert(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	second := message
	second.EventID, second.MessageID, second.Text = "after-recovery", "om-next", "continue"
	if err := store.Insert(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); !found || err == nil {
		t.Fatalf("canceled request unexpectedly completed inbox: found=%t err=%v", found, err)
	}
	if len(replies.sends) != 1 || len(backend.prompts) != 1 {
		t.Fatalf("turn was replayed after cancellation: sends=%d prompts=%d", len(replies.sends), len(backend.prompts))
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "p2p", SubjectID: "user"}
	delivery, found, err := store.GetDelivery(context.Background(), "bot-a", channel.TurnDeliveryID("bot-a", message.EventID))
	if err != nil || !found || delivery.Phase != "complete" || !delivery.AgentTurnCompleted {
		t.Fatalf("confirmed remote send was not durably completed: %+v found=%t err=%v", delivery, found, err)
	}
	if found, err := processor.ProcessOne(context.Background()); err != nil || !found || len(backend.prompts) != 1 {
		t.Fatalf("successor ran before recovery: found=%t err=%v prompts=%v", found, err, backend.prompts)
	}
	if err := store.ReconcileConfirmedDelivery(context.Background(), key, message.EventID); err != nil {
		t.Fatalf("confirmed send could not recover unfinished inbox: %v", err)
	}
	if _, state, found, err := store.Get(context.Background(), key); err != nil || !found || state != channel.ConversationReady {
		t.Fatalf("recovered conversation: state=%s found=%t err=%v", state, found, err)
	}
	active.active.Channel = replies
	processor.Bindings = active
	time.Sleep(1100 * time.Millisecond)
	if found, err := processor.ProcessOne(context.Background()); err != nil || !found {
		t.Fatalf("successor did not resume after reconciliation: found=%t err=%v", found, err)
	}
	if backend.starts != 1 || backend.attaches != 1 || len(backend.prompts) != 2 || len(replies.sends) != 2 ||
		!strings.HasSuffix(backend.prompts[1], ":continue") {
		t.Fatalf("recovered session did not continue exactly once: starts=%d attaches=%d prompts=%v sends=%d", backend.starts, backend.attaches, backend.prompts, len(replies.sends))
	}
}

func TestProcessorStreamAcceptsAuthoritativeFinalRevision(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, true)
	backend.finalText = "HELLO revised"
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "revision-event", MessageID: "om-root", ChatKind: channel.ChatGroup,
		ChatID: "group-a", SenderID: "user-a", MentionedIDs: []string{"bot-open-id"}, Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("process: %t %v", found, err)
	}
	if len(replies.complete) != 1 || replies.complete[0] != "HELLO revised" || len(replies.outcomes) != 1 || !replies.outcomes[0] {
		t.Fatalf("final revision not applied: %+v", replies.complete)
	}
}

func TestProcessorClosesStreamingCardAfterAgentFailureWithoutReplayingPrompt(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, true)
	backend.promptErr = errors.New("Agent connection lost")
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "failed-stream", MessageID: "om-root", ChatKind: channel.ChatGroup,
		ChatID: "group-a", SenderID: "user-a", MentionedIDs: []string{"bot-open-id"}, Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	if found, err := processor.ProcessOne(ctx); err == nil || !found {
		t.Fatalf("expected Agent failure: %t %v", found, err)
	}
	if len(replies.complete) != 1 || !strings.Contains(replies.complete[0], "结果未知") || len(replies.outcomes) != 1 || replies.outcomes[0] {
		t.Fatalf("failure card not closed: %v", replies.complete)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || found || len(backend.prompts) != 1 {
		t.Fatalf("unknown prompt replayed: found=%t err=%v prompts=%v", found, err, backend.prompts)
	}
}

func TestProcessorAdmitsMentionedGroupTopicAndSharesSessionAcrossMembers(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, true)
	first := channel.InboundMessage{BindingID: "bot-a", EventID: "event-1", MessageID: "om-root", ChatKind: channel.ChatGroup, ChatID: "group-a", SenderID: "user-a", MentionedIDs: []string{"bot-open-id"}, Text: "first"}
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
	backend.promptErr = errors.New("private-user-question")
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
	issues, err := store.ListIssues(ctx, "bot-a", 10)
	if err != nil || len(issues.Events) != 1 || len(issues.Conversations) != 1 ||
		strings.Contains(issues.Events[0].Failure, "private-user-question") || strings.Contains(issues.Conversations[0].Failure, "private-user-question") {
		t.Fatalf("unknown prompt leaked its error text through diagnostics: %+v err=%v", issues, err)
	}
}

func TestProcessorUnknownTurnBlocksLaterMessageInSameSession(t *testing.T) {
	ctx := context.Background()
	processor, store, backend, replies := testProcessor(t, false)
	backend.promptErr = errors.New("Agent result uncertain")
	for _, id := range []string{"first", "second"} {
		if err := store.Insert(ctx, channel.InboundMessage{BindingID: "bot-a", EventID: id, MessageID: id,
			ChatKind: channel.ChatDirect, ChatID: "p2p", SenderID: "user", Text: id}); err != nil {
			t.Fatal(err)
		}
	}
	if found, err := processor.ProcessOne(ctx); !found || err == nil {
		t.Fatalf("uncertain first turn was not fenced: found=%t err=%v", found, err)
	}
	if found, err := processor.ProcessOne(ctx); !found || err != nil {
		t.Fatalf("later same-session message was not deferred: found=%t err=%v", found, err)
	}
	if len(backend.prompts) != 1 || backend.starts != 1 || len(replies.sends) != 0 {
		t.Fatalf("later prompt ran despite unknown predecessor: starts=%d prompts=%v sends=%d", backend.starts, backend.prompts, len(replies.sends))
	}
	if stats, err := store.BindingStats(ctx, "bot-a"); err != nil || stats.UnknownEvents != 1 || stats.Queued != 1 || stats.FailedEvents != 0 {
		t.Fatalf("unknown predecessor or deferred successor state is wrong: %+v err=%v", stats, err)
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
