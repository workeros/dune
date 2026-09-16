package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aiomni/dune/im/channel"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type memoryStreamStore struct {
	mu              sync.Mutex
	states          map[string]channel.Delivery
	failActiveAfter int
	activeCommits   int
}

func (s *memoryStreamStore) Reserve(_ context.Context, initial channel.Delivery) (channel.Delivery, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = map[string]channel.Delivery{}
	}
	key := initial.Session.BindingID + "\x00" + initial.ID
	if state, ok := s.states[key]; ok {
		return state, false, nil
	}
	state := initial
	state.Phase, state.Revision = "reserved", 1
	s.states[key] = state
	return state, true, nil
}

func (s *memoryStreamStore) Commit(_ context.Context, state channel.Delivery) (channel.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state.Phase == "active" {
		s.activeCommits++
		if s.failActiveAfter > 0 && s.activeCommits == s.failActiveAfter {
			return channel.Delivery{}, errors.New("simulated durable write failure")
		}
	}
	key := state.Session.BindingID + "\x00" + state.ID
	current, ok := s.states[key]
	if !ok || current.Revision != state.Revision {
		return channel.Delivery{}, errors.New("stream revision conflict")
	}
	state.Revision++
	s.states[key] = state
	return state, nil
}

func (s *memoryStreamStore) state() struct {
	StreamState
	Phase string
} {
	s.mu.Lock()
	defer s.mu.Unlock()
	delivery := s.states["bot-a\x00turn-1"]
	var state StreamState
	_ = json.Unmarshal(delivery.ProviderState, &state)
	return struct {
		StreamState
		Phase string
	}{state, delivery.Phase}
}

func testStreamingChannel(t *testing.T, store channel.DeliveryStore) *Channel {
	t.Helper()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyStreaming})
	credentials, _ := json.Marshal(Credentials{AppSecret: "test-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	opened, err := (Provider{Deliveries: store}).Open(context.Background(), channel.BotBinding{
		ID: "bot-a", TenantID: "tenant-a", Provider: Kind, Config: config,
	}, credentials, &recordingSink{})
	if err != nil {
		t.Fatal(err)
	}
	return opened.(*Channel)
}

func testGroupAddress() channel.ReplyAddress {
	data, _ := json.Marshal(replyAddress{ChatKind: channel.ChatGroup, ChatID: "oc_group", ReplyMessageID: "om_input"})
	return channel.ReplyAddress{Provider: Kind, Version: 1, Data: data}
}

func testStreamMessage(text string) channel.OutboundMessage {
	return channel.OutboundMessage{Text: text, DeliveryID: "turn-1", Session: channel.SessionKey{
		TenantID: "tenant-a", BindingID: "bot-a", ChatID: "oc_group", SubjectID: "om_input",
	}}
}

func TestCardKitStreamCreatesRepliesUpdatesAndCloses(t *testing.T) {
	store := &memoryStreamStore{}
	c := testStreamingChannel(t, store)
	var mu sync.Mutex
	var operations []string
	var updateTexts []string
	var sequences []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		operations = append(operations, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		switch r.URL.Path {
		case "/open-apis/cardkit/v1/cards":
			var request struct {
				Data string `json:"data"`
			}
			if json.Unmarshal(body, &request) != nil || !strings.Contains(request.Data, `"streaming_mode":true`) {
				t.Errorf("create did not enable native streaming: %s", body)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"card_id":"card-123"}}`)
		case "/open-apis/im/v1/messages/om_input/reply":
			var request struct {
				ReplyInThread bool   `json:"reply_in_thread"`
				Content       string `json:"content"`
				UUID          string `json:"uuid"`
			}
			if json.Unmarshal(body, &request) != nil || !request.ReplyInThread || !strings.Contains(request.Content, `"card_id":"card-123"`) || request.UUID != outboundUUID("bot-a", "turn-1", "card-message") {
				t.Errorf("card was not sent inside thread: %s", body)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_card"}}`)
		case "/open-apis/cardkit/v1/cards/card-123/elements/answer/content":
			var request struct {
				Content  string `json:"content"`
				Sequence int    `json:"sequence"`
				UUID     string `json:"uuid"`
			}
			if json.Unmarshal(body, &request) != nil || request.UUID == "" {
				t.Errorf("invalid CardKit content request: %s", body)
			}
			mu.Lock()
			updateTexts = append(updateTexts, request.Content)
			sequences = append(sequences, request.Sequence)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0}`)
		case "/open-apis/cardkit/v1/cards/card-123/settings":
			var request struct {
				Settings string `json:"settings"`
				Sequence int    `json:"sequence"`
				UUID     string `json:"uuid"`
			}
			if json.Unmarshal(body, &request) != nil || request.UUID == "" || !strings.Contains(request.Settings, `"streaming_mode":false`) {
				t.Errorf("streaming mode was not closed: %s", body)
			}
			mu.Lock()
			sequences = append(sequences, request.Sequence)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0}`)
		default:
			t.Errorf("unexpected Feishu API path: %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	ctx := context.Background()
	stream, err := c.OpenStream(ctx, testGroupAddress(), testStreamMessage("处理中"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := testStreamMessage("wrong session")
	wrong.Session.SubjectID = "om_elsewhere"
	if err := stream.Update(ctx, wrong); err == nil {
		t.Fatal("stream accepted an update for another session")
	}
	if err := stream.Update(ctx, testStreamMessage("hello")); err != nil {
		t.Fatal(err)
	}
	if err := stream.Update(ctx, testStreamMessage("hello world")); err != nil {
		t.Fatal(err)
	}
	if err := stream.Complete(ctx, testStreamMessage("HELLO revised")); err != nil {
		t.Fatal(err)
	}
	if state := store.state(); state.Phase != "complete" || state.CardID != "card-123" || state.MessageID != "om_card" || state.Sequence != 4 || state.ConfirmedText != "HELLO revised" {
		t.Fatalf("incorrect persisted stream state: %+v", state)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(operations) != 6 {
		t.Fatalf("expected create, reply, two deltas, final revision, close; got %d operations: %v", len(operations), operations)
	}
	if fmt.Sprint(updateTexts) != "[hello hello world HELLO revised]" || fmt.Sprint(sequences) != "[1 2 3 4]" {
		t.Fatalf("CardKit updates must contain cumulative text and increasing sequence: texts=%v sequence=%v", updateTexts, sequences)
	}
	for index, want := range []string{"/cardkit/v1/cards", "/messages/om_input/reply", "/elements/answer/content", "/elements/answer/content", "/elements/answer/content", "/card-123/settings"} {
		if !strings.Contains(operations[index], want) && !(index == 5 && strings.Contains(operations[index], "/cards/card-123/settings")) {
			t.Fatalf("operation %d order mismatch: %s", index, operations[index])
		}
	}
}

func TestCardKitStreamUsesDirectMessageForPrivateChat(t *testing.T) {
	store := &memoryStreamStore{}
	c := testStreamingChannel(t, store)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/open-apis/cardkit/v1/cards":
			_, _ = io.WriteString(w, `{"code":0,"data":{"card_id":"card-private"}}`)
		case "/open-apis/im/v1/messages":
			var request struct {
				ReceiveID string `json:"receive_id"`
				MsgType   string `json:"msg_type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.ReceiveID != "ou_alice" || request.MsgType != "interactive" {
				t.Errorf("wrong private card target: %+v %v", request, err)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_private_card"}}`)
		case "/open-apis/cardkit/v1/cards/card-private/elements/answer/content", "/open-apis/cardkit/v1/cards/card-private/settings":
			_, _ = io.WriteString(w, `{"code":0}`)
		default:
			t.Errorf("unexpected Feishu API path: %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	data, _ := json.Marshal(replyAddress{ChatKind: channel.ChatDirect, ChatID: "oc_dm", SenderOpenID: "ou_alice", ReplyMessageID: "om_input"})
	address := channel.ReplyAddress{Provider: Kind, Version: 1, Data: data}
	message := channel.OutboundMessage{Text: "处理中", DeliveryID: "turn-1", Session: channel.SessionKey{
		TenantID: "tenant-a", BindingID: "bot-a", ChatID: "oc_dm", SubjectID: "ou_alice",
	}}
	stream, err := c.OpenStream(context.Background(), address, message)
	if err != nil {
		t.Fatal(err)
	}
	message.Text = "hello"
	if err := stream.Complete(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	want := []string{"/open-apis/cardkit/v1/cards", "/open-apis/im/v1/messages", "/open-apis/cardkit/v1/cards/card-private/elements/answer/content", "/open-apis/cardkit/v1/cards/card-private/settings"}
	if !slices.Equal(paths, want) {
		t.Fatalf("private CardKit call sequence: got %v, want %v", paths, want)
	}
}

func TestStreamUnknownSendNeverFallsBackToGroupTopLevel(t *testing.T) {
	store := &memoryStreamStore{}
	c := testStreamingChannel(t, store)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/open-apis/cardkit/v1/cards" {
			_, _ = io.WriteString(w, `{"code":0,"data":{"card_id":"card-123"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":230001,"msg":"reply failed"}`)
	}))
	defer server.Close()
	c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	if _, err := c.OpenStream(context.Background(), testGroupAddress(), testStreamMessage("")); err == nil {
		t.Fatal("failed thread reply unexpectedly succeeded")
	}
	if state := store.state(); state.Phase != "unknown" || state.CardID == "" || state.MessageID != "" {
		t.Fatalf("unknown send state was not preserved: %+v", state)
	}
	if _, err := c.OpenStream(context.Background(), testGroupAddress(), testStreamMessage("")); err == nil {
		t.Fatal("unknown send was automatically retried")
	}
	if len(paths) != 2 || !strings.Contains(paths[1], "/reply") {
		t.Fatal(fmt.Sprintf("unexpected top-level fallback or retry: %v", paths))
	}
}

func TestStreamUnknownUpdateIsNotReplayed(t *testing.T) {
	store := &memoryStreamStore{}
	c := testStreamingChannel(t, store)
	updates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
		case "/open-apis/cardkit/v1/cards":
			_, _ = io.WriteString(w, `{"code":0,"data":{"card_id":"card-123"}}`)
		case "/open-apis/im/v1/messages/om_input/reply":
			_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_card"}}`)
		case "/open-apis/cardkit/v1/cards/card-123/elements/answer/content":
			updates++
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":999,"msg":"unknown"}`)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	stream, err := c.OpenStream(context.Background(), testGroupAddress(), testStreamMessage(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Update(context.Background(), testStreamMessage("hello")); err == nil {
		t.Fatal("failed update unexpectedly succeeded")
	}
	if state := store.state(); state.Phase != "unknown" || state.PendingText != "hello" || state.Sequence != 1 {
		t.Fatalf("pending update was not preserved: %+v", state)
	}
	if err := stream.Update(context.Background(), testStreamMessage("hello world")); err == nil {
		t.Fatal("unknown update was silently retried")
	}
	if updates != 1 {
		t.Fatalf("unknown update was replayed %d times", updates)
	}
}

func TestStreamFreezesWhenRemoteUpdateSucceedsButCommitFails(t *testing.T) {
	store := &memoryStreamStore{failActiveAfter: 3} // after card creation, send, then update
	c := testStreamingChannel(t, store)
	updates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
		case "/open-apis/cardkit/v1/cards":
			_, _ = io.WriteString(w, `{"code":0,"data":{"card_id":"card-123"}}`)
		case "/open-apis/im/v1/messages/om_input/reply":
			_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_card"}}`)
		case "/open-apis/cardkit/v1/cards/card-123/elements/answer/content":
			updates++
			_, _ = io.WriteString(w, `{"code":0}`)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	stream, err := c.OpenStream(context.Background(), testGroupAddress(), testStreamMessage(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Update(context.Background(), testStreamMessage("hello")); err == nil {
		t.Fatal("post-update durable write failure was not reported")
	}
	if state := store.state(); state.Phase != "pending" || state.PendingText != "hello" {
		t.Fatalf("pending update should remain for reconciliation: %+v", state)
	}
	if err := stream.Complete(context.Background(), testStreamMessage("hello world")); err == nil {
		t.Fatal("uncertain stream was allowed to complete")
	}
	if updates != 1 {
		t.Fatalf("uncertain stream issued %d updates", updates)
	}
}
