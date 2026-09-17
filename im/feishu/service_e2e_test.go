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
	"strings"
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

func TestTenantBotsKeepCredentialsTargetsSessionsAndReplyModesSeparate(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	type botCase struct {
		id, appID, secret, encryptKey, token, mode, agentID string
	}
	bots := []botCase{
		{id: "bot-a", appID: testAppID, secret: "secret-a", encryptKey: testEncryptKey, token: testToken, mode: ReplyFinalText, agentID: "agent-a"},
		{id: "bot-b", appID: "cli_other_app", secret: "secret-b", encryptKey: "other-encrypt-key", token: "other-token", mode: ReplyFinalCard, agentID: "agent-b"},
	}
	secrets := map[string][]byte{}
	for _, bot := range bots {
		config, _ := json.Marshal(Config{AppID: bot.appID, ReceiveMode: ReceiveCallback, ReplyMode: bot.mode})
		secret, _ := json.Marshal(Credentials{AppSecret: bot.secret, EncryptKey: bot.encryptKey, VerificationToken: bot.token})
		secrets[bot.id] = secret
		if _, err := store.Put(ctx, channel.BotBinding{ID: bot.id, TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
			Config: config, CredentialRef: bot.id, Target: channel.AgentTarget{RunnerID: "runner", ProfileID: bot.agentID, ProfileRevision: 1}, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	agents := &localAgentBackend{starts: map[string]int{}, attaches: map[string]int{}}
	resolver := credentialResolverFunc(func(_ context.Context, binding channel.BotBinding) ([]byte, error) {
		secret, ok := secrets[binding.CredentialRef]
		if !ok {
			return nil, fmt.Errorf("unexpected credential reference %q", binding.CredentialRef)
		}
		return secret, nil
	})
	service, err := NewService(store, resolver, agents)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	if err := service.LoadTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	apiServers := make([]*httptest.Server, 0, len(bots))
	defer func() {
		for _, server := range apiServers {
			server.Close()
		}
	}()
	apiCalls := map[string][]string{}
	var apiMu sync.Mutex
	for _, bot := range bots {
		bot := bot
		entry := service.bindings[bot.id]
		if entry == nil || entry.channel.config.AppID != bot.appID || entry.channel.secret.AppSecret != bot.secret || entry.channel.secret.EncryptKey != bot.encryptKey || entry.channel.secret.VerificationToken != bot.token {
			t.Fatalf("%s did not resolve its own Feishu credentials", bot.id)
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
				_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
				return
			}
			var request struct {
				MsgType   string `json:"msg_type"`
				ReceiveID string `json:"receive_id"`
			}
			if r.URL.Path != "/open-apis/im/v1/messages" || json.NewDecoder(r.Body).Decode(&request) != nil || request.ReceiveID != "ou_alice" {
				t.Errorf("%s sent to an unexpected address: path=%s request=%+v", bot.id, r.URL.Path, request)
			}
			apiMu.Lock()
			apiCalls[bot.id] = append(apiCalls[bot.id], request.MsgType)
			apiMu.Unlock()
			_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_answer"}}`)
		}))
		apiServers = append(apiServers, server)
		service.bindings[bot.id].channel.client = lark.NewClient(bot.appID, bot.secret, lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
		plain := localDirectEvent("shared-event", "om_shared", "ou_alice", "hello")
		plain = []byte(strings.ReplaceAll(strings.ReplaceAll(string(plain), testAppID, bot.appID), testToken, bot.token))
		handler, err := service.CallbackHandler("tenant-a", bot.id)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, serviceSignedCallback(t, plain, bot.encryptKey))
		if response.Code != http.StatusOK {
			t.Fatalf("%s callback status=%d body=%q", bot.id, response.Code, response.Body.String())
		}
	}
	processor := channel.Processor{Work: store, Conversations: store, Deliveries: store, Bindings: service, Agents: agents}
	for range bots {
		if found, err := processor.ProcessOne(ctx); err != nil || !found {
			t.Fatalf("multi-bot event was not processed: found=%t err=%v", found, err)
		}
	}
	apiMu.Lock()
	defer apiMu.Unlock()
	for _, bot := range bots {
		key := channel.SessionKey{TenantID: "tenant-a", BindingID: bot.id, ChatID: "oc_direct", SubjectID: "ou_alice"}
		session, state, found, err := store.Get(ctx, key)
		if err != nil || !found || state != channel.ConversationReady || session.Target.ProfileID != bot.agentID || agents.starts[key.String()] != 1 {
			t.Fatalf("%s session crossed a Binding: session=%+v state=%s found=%t starts=%d err=%v", bot.id, session, state, found, agents.starts[key.String()], err)
		}
		delivery, found, err := store.GetDelivery(ctx, bot.id, channel.TurnDeliveryID(bot.id, "shared-event"))
		if err != nil || !found || delivery.Phase != "complete" || delivery.Mode != bot.mode || delivery.Session != key {
			t.Fatalf("%s delivery crossed a Binding: %+v found=%t err=%v", bot.id, delivery, found, err)
		}
		wantType := "text"
		if bot.mode == ReplyFinalCard {
			wantType = "interactive"
		}
		if len(apiCalls[bot.id]) != 1 || apiCalls[bot.id][0] != wantType {
			t.Fatalf("%s used the wrong reply API mode: %v", bot.id, apiCalls[bot.id])
		}
	}
}

func TestCallbackReplyModesInDirectAndGroupSessions(t *testing.T) {
	for _, mode := range []string{ReplyFinalText, ReplyFinalCard, ReplyStreaming} {
		for _, chat := range []string{"direct", "group"} {
			t.Run(string(mode)+"/"+chat, func(t *testing.T) {
				agents, target := gatewayAgentFixture(t)
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: mode})
				binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
					Config: config, CredentialRef: "local-test", Target: target, Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				secret, _ := json.Marshal(Credentials{AppSecret: "test-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
				service, err := NewService(store, &testCredentialResolver{data: secret}, agents)
				if err != nil {
					t.Fatal(err)
				}
				defer service.Stop(ctx)
				if err := service.Activate(ctx, binding); err != nil {
					t.Fatal(err)
				}
				type outboundCall struct {
					path string
					body map[string]any
				}
				var calls []outboundCall
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/open-apis/auth/v3/tenant_access_token/internal":
						_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
						return
					case "/open-apis/bot/v3/info":
						_, _ = io.WriteString(w, `{"code":0,"bot":{"open_id":"ou_this_bot"}}`)
						return
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode outbound API request: %v", err)
					}
					calls = append(calls, outboundCall{path: r.URL.Path, body: body})
					switch {
					case r.URL.Path == "/open-apis/cardkit/v1/cards":
						_, _ = io.WriteString(w, `{"code":0,"data":{"card_id":"card-1"}}`)
					case r.URL.Path == "/open-apis/im/v1/messages" || strings.HasSuffix(r.URL.Path, "/reply"):
						_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_answer"}}`)
					case strings.HasPrefix(r.URL.Path, "/open-apis/cardkit/v1/cards/card-1/"):
						_, _ = io.WriteString(w, `{"code":0}`)
					default:
						t.Errorf("unexpected outbound API path: %s", r.URL.Path)
						http.Error(w, "unexpected", http.StatusNotFound)
					}
				}))
				defer server.Close()
				service.bindings[binding.ID].channel.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
				handler, err := service.CallbackHandler("tenant-a", binding.ID)
				if err != nil {
					t.Fatal(err)
				}
				var event []byte
				subject, chatID, inputID := "ou_alice", "oc_direct", "om_direct"
				if chat == "group" {
					subject, chatID, inputID = "om_root", "oc_group", "om_root"
					event = localGroupEvent("event-1", inputID, "ou_alice", "", "", true)
				} else {
					event = localDirectEvent("event-1", inputID, "ou_alice", "hello")
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, serviceSignedCallback(t, event, testEncryptKey))
				if response.Code != http.StatusOK {
					t.Fatalf("callback was not accepted: status=%d body=%q", response.Code, response.Body.String())
				}
				processor := channel.Processor{Work: store, Conversations: store, Deliveries: store, Bindings: service, Agents: agents}
				if found, err := processor.ProcessOne(ctx); err != nil || !found {
					t.Fatalf("callback was not processed: found=%t err=%v", found, err)
				}
				key := channel.SessionKey{TenantID: "tenant-a", BindingID: binding.ID, ChatID: chatID, SubjectID: subject}
				session, state, found, err := store.Get(ctx, key)
				if err != nil || !found || state != channel.ConversationReady {
					t.Fatalf("session was not completed: state=%s found=%t err=%v", state, found, err)
				}
				defer agents.Stop(context.Background(), session, channel.AgentSession{Runtime: session.Runtime, ACPSessionID: session.ACPSessionID})
				delivery, found, err := store.GetDelivery(ctx, binding.ID, channel.TurnDeliveryID(binding.ID, "event-1"))
				if err != nil || !found || delivery.Phase != "complete" || !delivery.AgentTurnCompleted || delivery.Mode != string(mode) {
					t.Fatalf("delivery was not durably completed: %+v found=%t err=%v", delivery, found, err)
				}
				messageCount, cardCreates, cardUpdates, cardCloses := 0, 0, 0, 0
				var visible string
				for _, call := range calls {
					switch {
					case call.path == "/open-apis/cardkit/v1/cards":
						cardCreates++
						if mode != ReplyStreaming || !strings.Contains(fmt.Sprint(call.body["data"]), `"streaming_mode":true`) {
							t.Fatalf("unexpected CardKit create: %+v", call)
						}
					case call.path == "/open-apis/im/v1/messages" || strings.HasSuffix(call.path, "/reply"):
						messageCount++
						if call.body["uuid"] == "" {
							t.Fatalf("message lacks idempotency UUID: %+v", call)
						}
						if chat == "group" {
							if call.path != "/open-apis/im/v1/messages/"+inputID+"/reply" || call.body["reply_in_thread"] != true {
								t.Fatalf("reply escaped group topic: %+v", call)
							}
						} else if call.path != "/open-apis/im/v1/messages" || call.body["receive_id"] != "ou_alice" {
							t.Fatalf("reply escaped direct chat: %+v", call)
						}
						wantType := "text"
						if mode != ReplyFinalText {
							wantType = "interactive"
						}
						if call.body["msg_type"] != wantType {
							t.Fatalf("wrong reply message type: %+v", call)
						}
						if mode != ReplyStreaming {
							visible = fmt.Sprint(call.body["content"])
						}
					case strings.HasSuffix(call.path, "/elements/answer/content"):
						cardUpdates++
						visible = fmt.Sprint(call.body["content"])
					case strings.HasSuffix(call.path, "/settings"):
						cardCloses++
						if !strings.Contains(fmt.Sprint(call.body["settings"]), `"streaming_mode":false`) {
							t.Fatalf("streaming mode was not closed: %+v", call)
						}
					default:
						t.Fatalf("unexpected outbound API request: %+v", call)
					}
				}
				wantAnswer := "turn 1: hello"
				if chat == "group" {
					wantAnswer = "turn 1: question om_root"
				}
				if !strings.Contains(visible, wantAnswer) || strings.Contains(visible, "private thought") {
					t.Fatalf("reply does not contain the ACP assistant answer: %q", visible)
				}
				if messageCount != 1 || (mode == ReplyStreaming && (cardCreates != 1 || cardUpdates == 0 || cardCloses != 1)) ||
					(mode != ReplyStreaming && (cardCreates != 0 || cardUpdates != 0 || cardCloses != 0)) {
					t.Fatalf("wrong API sequence for %s/%s: %+v", mode, chat, calls)
				}
			})
		}
	}
}

func TestCallbackRedeliveryAcrossBindingRevisionDoesNotRunOldAgentTurn(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "local-test", Target: channel.AgentTarget{RunnerID: "runner-a", ProfileID: "old-agent", ProfileRevision: 1}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "test-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	agents := &localAgentBackend{starts: map[string]int{}, attaches: map[string]int{}}
	service, err := NewService(store, &testCredentialResolver{data: secret}, agents)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	handler, err := service.CallbackHandler("tenant-a", "bot-a")
	if err != nil {
		t.Fatal(err)
	}
	event := localDirectEvent("event-old", "om_old", "ou_alice", "old question")
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, serviceSignedCallback(t, event, testEncryptKey))
	if first.Code != http.StatusOK {
		t.Fatalf("old revision was not durably accepted: %d", first.Code)
	}
	binding.Target.ProfileID = "new-agent"
	binding, err = store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	redelivery := httptest.NewRecorder()
	handler.ServeHTTP(redelivery, serviceSignedCallback(t, event, testEncryptKey))
	if redelivery.Code != http.StatusOK {
		t.Fatalf("current revision did not ACK an identical redelivery: %d", redelivery.Code)
	}
	processor := channel.Processor{Work: store, Conversations: store, Deliveries: store, Bindings: service, Agents: agents}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("old queued event was not consumed: found=%t err=%v", found, err)
	}
	if len(agents.starts) != 0 {
		t.Fatalf("old queued event started an Agent under new Binding: %v", agents.starts)
	}
	if found, err := processor.ProcessOne(ctx); err != nil || found {
		t.Fatalf("redelivery created a second work item: found=%t err=%v", found, err)
	}
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
		Config: config, CredentialRef: "local-test", Target: channel.AgentTarget{RunnerID: "runner-a", ProfileID: "agent-a", ProfileRevision: 1}, Enabled: true})
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
		Config: config, CredentialRef: "local-test", Target: channel.AgentTarget{RunnerID: "runner-a", ProfileID: "agent-a", ProfileRevision: 1}, Enabled: true})
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
