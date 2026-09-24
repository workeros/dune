package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/im/sqlite"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcard "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
)

func TestCardKitChunksRespectEscapedUpdateRequestBudget(t *testing.T) {
	for _, r := range []rune{'a', '"', '\\', '\b', '\f', '\n', '\r', '\t', 0, 0x1f, '<', '>', '&', '\u2028', '\u2029', '你', '\ufffd'} {
		encoded, err := json.Marshal(string(r))
		if err != nil || jsonEscapedRuneBytes(r) != len(encoded)-2 {
			t.Fatalf("incorrect JSON escaped rune size for %U: got=%d encoded=%q err=%v", r, jsonEscapedRuneBytes(r), encoded, err)
		}
	}
	for _, answer := range []string{
		strings.Repeat(`"`, maxStreamUpdateRequestBytes),
		strings.Repeat("<>&\\\n", maxStreamUpdateRequestBytes/4),
		strings.Repeat("你好", maxStreamUpdateRequestBytes/4),
	} {
		parts := streamChunks(answer)
		if len(parts) < 2 || strings.Join(parts, "") != answer {
			t.Fatalf("CardKit answer was lost or not split: parts=%d length=%d", len(parts), len(answer))
		}
		for index, part := range parts {
			if !utf8.ValidString(part) {
				t.Fatalf("part %d split a UTF-8 rune", index)
			}
			body := larkcard.NewContentCardElementReqBodyBuilder().Uuid(strings.Repeat("0", 32)).Sequence(index + 1).Content(part).Build()
			encoded, err := json.Marshal(body)
			if err != nil || len(encoded) > maxStreamUpdateRequestBytes {
				t.Fatalf("part %d exceeds update request budget: bytes=%d err=%v", index, len(encoded), err)
			}
			cardJSON, err := streamCardJSON(part, false)
			if err != nil || len(cardJSON) > 30*1024 {
				t.Fatalf("final card %d exceeds CardKit's 30 KiB size limit: bytes=%d err=%v", index, len(cardJSON), err)
			}
		}
	}
}

func TestCardKitChunksAccountForInvalidUTF8Replacement(t *testing.T) {
	// A malformed byte becomes six JSON bytes (\\ufffd), not the three bytes
	// of a valid replacement rune. The public ReplyStream accepts Go strings.
	answer := strings.Repeat(string([]byte{0xff}), maxStreamUpdateRequestBytes/3)
	parts := streamChunks(answer)
	if len(parts) < 2 || strings.Join(parts, "") != answer {
		t.Fatalf("malformed answer was lost or not split: parts=%d", len(parts))
	}
	for index, part := range parts {
		body := larkcard.NewContentCardElementReqBodyBuilder().Uuid(strings.Repeat("0", 32)).Sequence(index + 1).Content(part).Build()
		encoded, err := json.Marshal(body)
		if err != nil || len(encoded) > maxStreamUpdateRequestBytes {
			t.Fatalf("part %d exceeds update request budget: bytes=%d err=%v", index, len(encoded), err)
		}
	}
}

type memoryStreamStore struct {
	mu              sync.Mutex
	states          map[string]channel.Delivery
	failActiveAfter int
	activeCommits   int
	failComplete    bool
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
	if state.Phase == "complete" && s.failComplete {
		return channel.Delivery{}, errors.New("simulated final confirmation failure")
	}
	if state.Phase == "active" {
		s.activeCommits++
		if s.failActiveAfter > 0 && s.activeCommits >= s.failActiveAfter {
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

func TestCardKitConfirmedOperationPersistsAfterRequestCancellation(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := channel.DeliveryManager{Store: store}
	initial := channel.Delivery{ID: "turn-1", Session: channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "oc_group", SubjectID: "om_root"},
		Mode: ReplyStreaming, ProviderStateVersion: 2, AgentTurnCompleted: true}
	delivery, created, err := manager.Reserve(context.Background(), initial)
	if err != nil || !created {
		t.Fatalf("reserve CardKit delivery: created=%t err=%v", created, err)
	}
	delivery, err = manager.Intent(context.Background(), delivery, "closing", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := &cardStream{owner: &Channel{deliveries: store}, delivery: delivery,
		state: StreamState{CardID: "card-1", MessageID: "om_reply", ConfirmedText: "answer", Sequence: 2}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // CardKit already returned success; only its local confirmation remains.
	if err := stream.commit(ctx, "complete"); err != nil {
		t.Fatalf("confirmed CardKit close lost after cancellation: %v", err)
	}
	stored, found, err := store.GetDelivery(context.Background(), "bot-a", "turn-1")
	if err != nil || !found || stored.Phase != "complete" || !stored.AgentTurnCompleted {
		t.Fatalf("confirmed CardKit close was not durable: %+v found=%t err=%v", stored, found, err)
	}
}

func (s *memoryStreamStore) state() struct {
	StreamState
	Phase              string
	AgentTurnCompleted bool
} {
	s.mu.Lock()
	defer s.mu.Unlock()
	delivery := s.states["bot-a\x00turn-1"]
	var state StreamState
	_ = json.Unmarshal(delivery.ProviderState, &state)
	return struct {
		StreamState
		Phase              string
		AgentTurnCompleted bool
	}{state, delivery.Phase, delivery.AgentTurnCompleted}
}

func testStreamingChannel(t *testing.T, store channel.DeliveryStore) *Channel {
	t.Helper()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyStreaming})
	credentials, _ := json.Marshal(Credentials{AppSecret: "test-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	opened, err := (Provider{Deliveries: store}).Open(context.Background(), channel.BotBinding{
		ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1, Config: config,
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

func checkFinalCardUpdate(body []byte, wantText string) error {
	var request larkcard.UpdateCardReqBody
	if err := json.Unmarshal(body, &request); err != nil {
		return err
	}
	if request.Card == nil || value(request.Card.Type) != "card_json" || value(request.Uuid) == "" || request.Sequence == nil || *request.Sequence < 1 {
		return fmt.Errorf("invalid final CardKit update: %s", body)
	}
	return checkStreamCard(value(request.Card.Data), wantText, false)
}

func checkStreamCard(data, wantText string, streaming bool) error {
	var card struct {
		Schema string `json:"schema"`
		Config struct {
			StreamingMode *bool `json:"streaming_mode"`
			UpdateMulti   bool  `json:"update_multi"`
		} `json:"config"`
		Header json.RawMessage `json:"header"`
		Body   struct {
			Elements []struct {
				Tag       string `json:"tag"`
				ElementID string `json:"element_id"`
				Content   string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(data), &card); err != nil {
		return err
	}
	if len(data) > 30*1024 || card.Schema != "2.0" || card.Config.StreamingMode == nil || *card.Config.StreamingMode != streaming || !card.Config.UpdateMulti ||
		len(card.Header) != 0 || len(card.Body.Elements) != 1 {
		return fmt.Errorf("card has a header, incorrect config or size: %s", data)
	}
	element := card.Body.Elements[0]
	if element.Tag != "markdown" || element.ElementID != streamElementID || element.Content != wantText {
		return fmt.Errorf("card did not preserve its answer: %q, want %q", element.Content, wantText)
	}
	return nil
}

func TestCardKitStreamCreatesRepliesUpdatesAndCloses(t *testing.T) {
	for _, tc := range []struct {
		name, finalText string
		completed       bool
	}{
		{name: "revised answer", finalText: "HELLO revised", completed: true},
		{name: "unchanged answer", finalText: "hello world", completed: true},
		{name: "agent failed", finalText: "hello world\n\n⚠️ 处理未完成，Agent 结果未知。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
					if err := json.Unmarshal(body, &request); err != nil {
						t.Error(err)
					}
					if err := checkStreamCard(request.Data, "", true); err != nil {
						t.Error(err)
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
				case "/open-apis/cardkit/v1/cards/card-123":
					var request struct {
						Sequence int `json:"sequence"`
					}
					if err := checkFinalCardUpdate(body, tc.finalText); err != nil {
						t.Error(err)
					}
					if r.Method != http.MethodPut || json.Unmarshal(body, &request) != nil {
						t.Errorf("invalid final card request: %s %s", r.Method, body)
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
			stream, err := c.OpenStream(ctx, testGroupAddress(), testStreamMessage(""))
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
			final := testStreamMessage(tc.finalText)
			final.AgentTurnCompleted = tc.completed
			if err := stream.Complete(ctx, final); err != nil {
				t.Fatal(err)
			}
			wantTexts := []string{"hello", "hello world"}
			wantPaths := []string{"/open-apis/cardkit/v1/cards", "/open-apis/im/v1/messages/om_input/reply", "/open-apis/cardkit/v1/cards/card-123/elements/answer/content", "/open-apis/cardkit/v1/cards/card-123/elements/answer/content"}
			if tc.finalText != "hello world" {
				wantTexts = append(wantTexts, tc.finalText)
				wantPaths = append(wantPaths, "/open-apis/cardkit/v1/cards/card-123/elements/answer/content")
			}
			wantPaths = append(wantPaths, "/open-apis/cardkit/v1/cards/card-123")
			if state := store.state(); state.Phase != "complete" || state.AgentTurnCompleted != tc.completed || state.CardID != "card-123" || state.MessageID != "om_card" || state.Sequence != len(wantTexts)+1 || state.ConfirmedText != tc.finalText {
				t.Fatalf("incorrect persisted stream state: %+v", state)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(operations) != len(wantPaths) {
				t.Fatalf("unexpected operations: %v", operations)
			}
			if !slices.Equal(updateTexts, wantTexts) || len(sequences) != len(wantTexts)+1 {
				t.Fatalf("CardKit updates must contain cumulative text and increasing sequence: texts=%v sequence=%v", updateTexts, sequences)
			}
			for index, sequence := range sequences {
				if sequence != index+1 {
					t.Fatalf("CardKit sequence did not increase: %v", sequences)
				}
			}
			for index, want := range wantPaths {
				if strings.Fields(operations[index])[1] != want {
					t.Fatalf("operation %d order mismatch: %s", index, operations[index])
				}
			}
		})
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
		case "/open-apis/cardkit/v1/cards/card-private/elements/answer/content", "/open-apis/cardkit/v1/cards/card-private":
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
	want := []string{"/open-apis/cardkit/v1/cards", "/open-apis/im/v1/messages", "/open-apis/cardkit/v1/cards/card-private/elements/answer/content", "/open-apis/cardkit/v1/cards/card-private"}
	if !slices.Equal(paths, want) {
		t.Fatalf("private CardKit call sequence: got %v, want %v", paths, want)
	}
}

func TestCardKitUnknownUpdateSurvivesStoreReopenWithoutResend(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "im.db")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	c := testStreamingChannel(t, store)
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/open-apis/cardkit/v1/cards":
			_, _ = io.WriteString(w, `{"code":0,"data":{"card_id":"card-1"}}`)
		case "/open-apis/im/v1/messages/om_input/reply":
			_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_card"}}`)
		case "/open-apis/cardkit/v1/cards/card-1/elements/answer/content":
			_, _ = io.WriteString(w, `{"code":999,"msg":"outcome unknown"}`)
		default:
			t.Errorf("unexpected CardKit API path: %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	initial := testStreamMessage("处理中")
	stream, err := c.OpenStream(ctx, testGroupAddress(), initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Update(ctx, testStreamMessage("answer")); err == nil {
		t.Fatal("failed CardKit update was reported as confirmed")
	}
	before, found, err := store.GetDelivery(ctx, "bot-a", initial.DeliveryID)
	if err != nil || !found || before.Phase != "unknown" || before.Operation != "updating" {
		t.Fatalf("unknown update was not durable: %+v found=%t err=%v", before, found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c = testStreamingChannel(t, store)
	c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	mu.Lock()
	count := len(calls)
	mu.Unlock()
	if _, err := c.OpenStream(ctx, testGroupAddress(), initial); err == nil {
		t.Fatal("reopened unknown delivery was resumed without reconciliation")
	}
	mu.Lock()
	afterCalls := len(calls)
	mu.Unlock()
	if afterCalls != count {
		t.Fatalf("reopened unknown delivery caused new Feishu API calls: before=%d after=%d", count, afterCalls)
	}
	after, found, err := store.GetDelivery(ctx, "bot-a", initial.DeliveryID)
	if err != nil || !found || after.Phase != before.Phase || after.Operation != before.Operation || string(after.ProviderState) != string(before.ProviderState) {
		t.Fatalf("unknown delivery changed across restart: before=%+v after=%+v found=%t err=%v", before, after, found, err)
	}
}

func TestCardKitLongAnswerContinuesInSameThread(t *testing.T) {
	store := &memoryStreamStore{}
	c := testStreamingChannel(t, store)
	var paths, texts, sendUUIDs []string
	created, closed := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		paths = append(paths, r.URL.Path)
		switch {
		case r.URL.Path == "/open-apis/cardkit/v1/cards":
			var request struct {
				Data string `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if err := checkStreamCard(request.Data, "", true); err != nil {
				t.Error(err)
			}
			created++
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"card_id":"card-%d"}}`, created)
		case r.URL.Path == "/open-apis/im/v1/messages/om_input/reply":
			var request struct {
				ReplyInThread bool   `json:"reply_in_thread"`
				Content       string `json:"content"`
				UUID          string `json:"uuid"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.ReplyInThread || !strings.Contains(request.Content, fmt.Sprintf(`"card_id":"card-%d"`, created)) {
				t.Errorf("continuation escaped group thread: %+v %v", request, err)
			}
			sendUUIDs = append(sendUUIDs, request.UUID)
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"message_id":"om_card_%d"}}`, created)
		case strings.HasSuffix(r.URL.Path, "/elements/answer/content"):
			wireBody, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Error(readErr)
			}
			var request struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(wireBody, &request); err != nil {
				t.Error(err)
			}
			if len(wireBody) > maxStreamUpdateRequestBytes {
				t.Errorf("CardKit update request exceeded local byte budget: %d", len(wireBody))
			}
			texts = append(texts, request.Content)
			_, _ = io.WriteString(w, `{"code":0}`)
		case r.URL.Path == fmt.Sprintf("/open-apis/cardkit/v1/cards/card-%d", created):
			body, _ := io.ReadAll(r.Body)
			if err := checkFinalCardUpdate(body, texts[created-1]); err != nil {
				t.Error(err)
			}
			closed++
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
	answer := strings.Repeat("你好", maxStreamUpdateRequestBytes/4) // multibyte UTF-8 across multiple cards
	if err := stream.Update(ctx, testStreamMessage(answer)); err != nil {
		t.Fatal(err)
	}
	final := testStreamMessage(answer)
	final.AgentTurnCompleted = true
	if err := stream.Complete(ctx, final); err != nil {
		t.Fatal(err)
	}
	state := store.state()
	if state.Phase != "complete" || !state.AgentTurnCompleted || created != 2 || closed != 2 || len(state.Parts) != 1 || state.Parts[0].CardID != "card-1" || state.CardID != "card-2" || state.NeedsContinuation {
		t.Fatalf("long answer was not durably completed across two cards: state=%+v created=%d closed=%d", state, created, closed)
	}
	if len(sendUUIDs) != 2 || sendUUIDs[0] == "" || sendUUIDs[1] == "" || sendUUIDs[0] == sendUUIDs[1] {
		t.Fatalf("continuation messages need distinct deterministic UUIDs: %v", sendUUIDs)
	}
	if state.Parts[0].Text+state.ConfirmedText != answer || len(texts) != 2 || texts[0] != state.Parts[0].Text || texts[1] != state.ConfirmedText {
		t.Fatalf("long answer was lost or duplicated: chunk sizes=%d/%d, answer=%d", len(state.Parts[0].Text), len(state.ConfirmedText), len(answer))
	}
	if len(paths) != 8 {
		t.Fatalf("expected create/send/update/close for each card, got %v", paths)
	}
}

func TestCardKitUnknownContinuationIsNotReplayed(t *testing.T) {
	store := &memoryStreamStore{}
	c := testStreamingChannel(t, store)
	created, sent := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
		case "/open-apis/cardkit/v1/cards":
			created++
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"card_id":"card-%d"}}`, created)
		case "/open-apis/im/v1/messages/om_input/reply":
			sent++
			if sent == 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"code":999,"msg":"unknown"}`)
			} else {
				_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_card_1"}}`)
			}
		case "/open-apis/cardkit/v1/cards/card-1/elements/answer/content", "/open-apis/cardkit/v1/cards/card-1":
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
	answer := strings.Repeat("长", maxStreamUpdateRequestBytes)
	if err := stream.Complete(ctx, testStreamMessage(answer)); err == nil {
		t.Fatal("uncertain continuation send unexpectedly succeeded")
	}
	state := store.state()
	if state.Phase != "unknown" || state.CardID != "card-2" || state.MessageID != "" || len(state.Parts) != 1 || created != 2 || sent != 2 {
		t.Fatalf("uncertain continuation was not preserved: %+v created=%d sent=%d", state, created, sent)
	}
	if _, err := c.OpenStream(ctx, testGroupAddress(), testStreamMessage("处理中")); err == nil || created != 2 || sent != 2 {
		t.Fatal("uncertain continuation was automatically replayed")
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

func TestStreamUncertainOperationCannotReplayOrComplete(t *testing.T) {
	for _, tc := range []struct {
		name, phase string
		failCommit  bool
		finalize    bool
	}{
		{name: "remote update failed", phase: "unknown"},
		{name: "remote succeeded but local commit failed", phase: "pending", failCommit: true},
		{name: "remote final card failed", phase: "unknown", finalize: true},
		{name: "final card succeeded but local commit failed", phase: "pending", failCommit: true, finalize: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &memoryStreamStore{}
			if tc.failCommit && tc.finalize {
				store.failComplete = true
			} else if tc.failCommit {
				store.failActiveAfter = 3 // card creation, send, then update
			}
			c := testStreamingChannel(t, store)
			platformCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				platformCalls++
				switch r.URL.Path {
				case "/open-apis/auth/v3/tenant_access_token/internal":
					_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
				case "/open-apis/cardkit/v1/cards":
					_, _ = io.WriteString(w, `{"code":0,"data":{"card_id":"card-123"}}`)
				case "/open-apis/im/v1/messages/om_input/reply":
					_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_card"}}`)
				case "/open-apis/cardkit/v1/cards/card-123/elements/answer/content":
					if tc.failCommit || tc.finalize {
						_, _ = io.WriteString(w, `{"code":0}`)
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = io.WriteString(w, `{"code":999,"msg":"unknown"}`)
					}
				case "/open-apis/cardkit/v1/cards/card-123":
					if tc.failCommit {
						_, _ = io.WriteString(w, `{"code":0}`)
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = io.WriteString(w, `{"code":999,"msg":"unknown"}`)
					}
				default:
					t.Errorf("unexpected request path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
			stream, err := c.OpenStream(t.Context(), testGroupAddress(), testStreamMessage(""))
			if err != nil {
				t.Fatal(err)
			}
			message := testStreamMessage("hello")
			wantSequence, wantPending := 1, "hello"
			if tc.finalize {
				message.AgentTurnCompleted = true
				err = stream.Complete(t.Context(), message)
				wantSequence, wantPending = 2, ""
			} else {
				err = stream.Update(t.Context(), message)
			}
			if err == nil {
				t.Fatal("uncertain operation unexpectedly succeeded")
			}
			if state := store.state(); state.Phase != tc.phase || state.PendingText != wantPending || state.Sequence != wantSequence || state.AgentTurnCompleted != tc.finalize {
				t.Fatalf("pending operation was not preserved: %+v", state)
			}
			beforeCalls := platformCalls
			if err := stream.Update(t.Context(), testStreamMessage("hello world")); err == nil {
				t.Fatal("uncertain update was silently retried")
			}
			if err := stream.Complete(t.Context(), testStreamMessage("hello world")); err == nil {
				t.Fatal("uncertain stream was allowed to complete")
			}
			reopened := testStreamingChannel(t, store)
			reopened.client = c.client
			if _, err := reopened.OpenStream(t.Context(), testGroupAddress(), testStreamMessage("")); err == nil {
				t.Fatal("uncertain operation was resumed without reconciliation")
			}
			if platformCalls != beforeCalls {
				t.Fatalf("uncertain operation was replayed: before=%d after=%d", beforeCalls, platformCalls)
			}
		})
	}
}
