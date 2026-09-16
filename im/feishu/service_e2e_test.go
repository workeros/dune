package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/im/sqlite"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type localAgentBackend struct {
	mu       sync.Mutex
	starts   map[string]int
	attaches map[string]int
}

func (a *localAgentBackend) Capabilities(context.Context, channel.ConversationSession) (channel.AgentCapabilities, error) {
	return channel.AgentCapabilities{Adapter: "acp", AssistantDeltas: true, ReliableFinal: true}, nil
}

func (a *localAgentBackend) Start(_ context.Context, session channel.ConversationSession) (channel.AgentSession, error) {
	a.mu.Lock()
	a.starts[session.Key.String()]++
	a.mu.Unlock()
	return channel.AgentSession{Runtime: channel.RuntimeHandle{ID: session.Key.String(), Incarnation: "local-test", Generation: 1, Adapter: "acp"}, ACPSessionID: session.Key.String()}, nil
}

func (a *localAgentBackend) Attach(_ context.Context, session channel.ConversationSession, existing channel.AgentSession) (channel.AgentSession, error) {
	a.mu.Lock()
	a.attaches[session.Key.String()]++
	a.mu.Unlock()
	return existing, nil
}

func (a *localAgentBackend) Prompt(_ context.Context, _ channel.ConversationSession, _ channel.AgentSession, prompt string, emit func(channel.AgentEvent) error) (string, error) {
	answer := "answer: " + prompt
	if err := emit(channel.AgentEvent{Kind: channel.AgentDelta, Text: answer}); err != nil {
		return "", err
	}
	return answer, nil
}

func (a *localAgentBackend) Stop(context.Context, channel.ConversationSession, channel.AgentSession) error {
	return nil
}

func localDirectEvent(eventID, messageID, sender, text string) []byte {
	content, _ := json.Marshal(map[string]string{"text": text})
	event, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]string{"event_id": eventID, "event_type": "im.message.receive_v1", "app_id": testAppID, "token": testToken},
		"event": map[string]any{
			"sender":  map[string]any{"sender_id": map[string]string{"open_id": sender}, "sender_type": "user"},
			"message": map[string]string{"message_id": messageID, "chat_id": "oc_direct", "chat_type": "p2p", "message_type": "text", "content": string(content)},
		},
	})
	return event
}

func localGroupEvent(eventID, messageID, sender, rootID, threadID string, mentionBot bool) []byte {
	content, _ := json.Marshal(map[string]string{"text": "question " + messageID})
	message := map[string]any{
		"message_id": messageID, "chat_id": "oc_group", "chat_type": "group", "message_type": "text", "content": string(content),
	}
	if rootID != "" {
		message["root_id"] = rootID
	}
	if threadID != "" {
		message["thread_id"] = threadID
	}
	if mentionBot {
		message["mentions"] = []any{map[string]any{"id": map[string]string{"open_id": "ou_this_bot"}, "mentioned_type": "bot"}}
	}
	event, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]string{"event_id": eventID, "event_type": "im.message.receive_v1", "app_id": testAppID, "token": testToken},
		"event":  map[string]any{"sender": map[string]any{"sender_id": map[string]string{"open_id": sender}, "sender_type": "user"}, "message": message},
	})
	return event
}

func TestCallbackRunWorkerSeparatesUsersAndReusesDirectSession(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "local-test", Target: channel.AgentTarget{RunnerID: "runner-a", AgentConfigID: "agent-a"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "test-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	agents := &localAgentBackend{starts: map[string]int{}, attaches: map[string]int{}}
	service, err := NewService(store, &testCredentialResolver{data: secret}, agents)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(context.Background())
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	sends := make(chan map[string]any, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		if r.URL.Path != "/open-apis/im/v1/messages" {
			t.Errorf("unexpected Feishu API path %q", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode outbound message: %v", err)
		}
		sends <- body
		_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_answer"}}`)
	}))
	defer server.Close()
	service.bindings[binding.ID].channel.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	handler, err := service.CallbackHandler("tenant-a", binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx, 1, func(err error) { t.Errorf("worker: %v", err) }) }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		service.mu.RLock()
		running := service.running
		service.mu.RUnlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Feishu worker did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := service.Run(ctx, 1, nil); err == nil {
		t.Fatal("concurrent Run accepted and doubled the worker pool")
	}
	inputs := []struct{ eventID, messageID, sender, text string }{
		{"event-a1", "om_a1", "ou_alice", "first"},
		{"event-b1", "om_b1", "ou_bob", "second"},
		{"event-a2", "om_a2", "ou_alice", "third"},
	}
	for i, input := range inputs {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, serviceSignedCallback(t, localDirectEvent(input.eventID, input.messageID, input.sender, input.text), testEncryptKey))
		if response.Code != http.StatusOK {
			t.Fatalf("callback %d: status=%d body=%q", i, response.Code, response.Body.String())
		}
		select {
		case body := <-sends:
			if body["receive_id"] != input.sender || body["uuid"] == "" || body["msg_type"] != "text" {
				t.Fatalf("reply %d sent to wrong user or without deduplication: %+v", i, body)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("callback %d was accepted but no reply was sent", i)
		}
	}
	for _, sender := range []string{"ou_alice", "ou_bob"} {
		key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "oc_direct", SubjectID: sender}
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, state, found, err := store.Get(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if found && state == channel.ConversationReady {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s session did not become ready: found=%t state=%s", sender, found, state)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for _, input := range inputs {
		digest := sha256.Sum256([]byte(binding.ID + "\x00" + input.eventID))
		deliveryID := "im-" + hex.EncodeToString(digest[:])
		delivery, found, err := store.GetDelivery(ctx, binding.ID, deliveryID)
		if err != nil || !found || delivery.Phase != "complete" || delivery.Mode != string(ReplyFinalText) || len(delivery.ProviderState) == 0 {
			t.Fatalf("event %s did not complete one durable delivery: %+v found=%t err=%v", input.eventID, delivery, found, err)
		}
	}
	alice := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "oc_direct", SubjectID: "ou_alice"}
	bob := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "oc_direct", SubjectID: "ou_bob"}
	agents.mu.Lock()
	startsAlice, startsBob := agents.starts[alice.String()], agents.starts[bob.String()]
	attachesAlice, attachesBob := agents.attaches[alice.String()], agents.attaches[bob.String()]
	agents.mu.Unlock()
	if startsAlice != 1 || startsBob != 1 || attachesAlice != 1 || attachesBob != 0 {
		t.Fatalf("session lifecycle: Alice start/attach=%d/%d Bob=%d/%d", startsAlice, attachesAlice, startsBob, attachesBob)
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal(fmt.Errorf("Feishu worker did not stop after cancellation"))
	}
	if err := service.Run(ctx, 1, nil); err == nil {
		t.Fatal("closed Feishu service restarted workers")
	}
}

func TestExternalStopCancelsRunWorkers(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewService(store, &testCredentialResolver{}, unusedAgentBackend{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx, 1, nil) }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		service.mu.RLock()
		running := service.running
		service.mu.RUnlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Feishu worker did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := service.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("external Stop did not cancel Run workers")
	}
}

func TestCallbackRunWorkerKeepsGroupThreadAcrossMembers(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "local-test", Target: channel.AgentTarget{RunnerID: "runner-a", AgentConfigID: "agent-a"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "test-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	agents := &localAgentBackend{starts: map[string]int{}, attaches: map[string]int{}}
	service, err := NewService(store, &testCredentialResolver{data: secret}, agents)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(context.Background())
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	type sentReply struct {
		path string
		body map[string]any
	}
	replies := make(chan sentReply, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
		case "/open-apis/bot/v3/info":
			_, _ = io.WriteString(w, `{"code":0,"bot":{"open_id":"ou_this_bot"}}`)
		default:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode group reply: %v", err)
			}
			replies <- sentReply{path: r.URL.Path, body: body}
			_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_bot_reply"}}`)
		}
	}))
	defer server.Close()
	service.bindings[binding.ID].channel.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	handler, err := service.CallbackHandler("tenant-a", binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx, 1, func(err error) { t.Errorf("worker: %v", err) }) }()
	events := []struct {
		id, message, sender, root, thread string
		mention                           bool
	}{
		{"event-root", "om_root", "ou_alice", "", "", true},
		{"event-child", "om_child", "ou_bob", "om_root", "omt_topic", false},
		{"event-new", "om_new", "ou_bob", "", "", true},
	}
	for _, event := range events {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, serviceSignedCallback(t, localGroupEvent(event.id, event.message, event.sender, event.root, event.thread, event.mention), testEncryptKey))
		if response.Code != http.StatusOK {
			t.Fatalf("callback %s: status=%d body=%q", event.id, response.Code, response.Body.String())
		}
		select {
		case reply := <-replies:
			if reply.path != "/open-apis/im/v1/messages/"+event.message+"/reply" || reply.body["reply_in_thread"] != true || reply.body["uuid"] == "" {
				t.Fatalf("reply escaped group topic: %+v", reply)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no group reply for %s", event.id)
		}
	}
	rootKey := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "oc_group", SubjectID: "om_root"}
	newKey := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "oc_group", SubjectID: "om_new"}
	for _, key := range []channel.SessionKey{rootKey, newKey} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			session, state, found, err := store.Get(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if found && state == channel.ConversationReady {
				if key == rootKey && session.ProviderThreadRef != "omt_topic" {
					t.Fatalf("thread reference was not bound to canonical root: %+v", session)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("group session %s did not become ready: found=%t state=%s", key.SubjectID, found, state)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	agents.mu.Lock()
	rootStarts, rootAttaches := agents.starts[rootKey.String()], agents.attaches[rootKey.String()]
	newStarts, newAttaches := agents.starts[newKey.String()], agents.attaches[newKey.String()]
	agents.mu.Unlock()
	if rootStarts != 1 || rootAttaches != 1 || newStarts != 1 || newAttaches != 0 {
		t.Fatalf("group topic lifecycle: root=%d/%d new=%d/%d", rootStarts, rootAttaches, newStarts, newAttaches)
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("group worker did not stop")
	}
}
