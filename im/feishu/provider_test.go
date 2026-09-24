package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/im/sqlite"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const testAppID = "cli_test_app"
const testEncryptKey = "test-encrypt-key"
const testToken = "test-verification-token"

type recordingSink struct {
	mu       sync.Mutex
	messages []channel.InboundMessage
	err      error
}

type blockingSink struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingSink) Accept(context.Context, channel.InboundMessage) error {
	close(s.entered)
	<-s.release
	return nil
}

type flakyInbox struct {
	store    *sqlite.Store
	failOnce bool
}

func (f *flakyInbox) Insert(ctx context.Context, message channel.InboundMessage) error {
	if f.failOnce {
		f.failOnce = false
		return errors.New("temporary storage outage")
	}
	return f.store.Insert(ctx, message)
}

func (s *recordingSink) Accept(_ context.Context, message channel.InboundMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.messages = append(s.messages, message)
	return nil
}

func testChannel(t *testing.T, sink channel.EventSink) *Channel {
	t.Helper()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	credentials, _ := json.Marshal(Credentials{AppSecret: "test-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	opened, err := (Provider{Deliveries: &memoryStreamStore{}}).Open(context.Background(), channel.BotBinding{
		ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1, Config: config,
	}, credentials, sink)
	if err != nil {
		t.Fatal(err)
	}
	return opened.(*Channel)
}

func testEventJSON() []byte {
	return []byte(`{"schema":"2.0","header":{"event_id":"event-1","event_type":"im.message.receive_v1","app_id":"cli_test_app","token":"test-verification-token"},"event":{"sender":{"sender_id":{"open_id":"ou_alice"},"sender_type":"user"},"message":{"message_id":"om_input","chat_id":"oc_group","chat_type":"group","thread_id":"omt_topic","root_id":"om_root","message_type":"text","content":"{\"text\":\"hello\"}"}}}`)
}

func signedCallback(t *testing.T, plain []byte, timestamp string) *http.Request {
	t.Helper()
	encrypted, err := larkcore.EncryptedEventMsg(context.Background(), plain, testEncryptKey)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"encrypt": encrypted})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/feishu/bot-a", bytes.NewReader(body))
	request.Header.Set(larkevent.EventRequestTimestamp, timestamp)
	request.Header.Set(larkevent.EventRequestNonce, "nonce-1")
	request.Header.Set(larkevent.EventSignature, larkevent.Signature(timestamp, "nonce-1", testEncryptKey, string(body)))
	return request
}

func TestCallbackAndWebSocketShareNormalizedEvent(t *testing.T) {
	sink := &recordingSink{}
	c := testChannel(t, sink)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	recorder := httptest.NewRecorder()
	c.CallbackHandler().ServeHTTP(recorder, signedCallback(t, testEventJSON(), stamp))
	if body := assertCallbackJSON(t, recorder, http.StatusOK); len(body) != 0 {
		t.Fatalf("event acknowledgement = %#v", body)
	}
	if recorder.Code != http.StatusOK || len(sink.messages) != 1 {
		t.Fatalf("callback status=%d, messages=%d", recorder.Code, len(sink.messages))
	}
	var event larkim.P2MessageReceiveV1
	if err := json.Unmarshal(testEventJSON(), &event); err != nil {
		t.Fatal(err)
	}
	if err := c.onMessage(context.Background(), &event); err != nil {
		t.Fatal(err)
	}
	if len(sink.messages) != 2 || !reflect.DeepEqual(sink.messages[0], sink.messages[1]) {
		t.Fatalf("transport event mismatch: %#v", sink.messages)
	}
	if got := sink.messages[0]; got.SenderID != "ou_alice" || got.Address.Provider != Kind || string(got.Address.Data) == "" {
		t.Fatalf("lost routing fields: %#v", got)
	}
}

func TestGroupAdmissionMatchesBotOpenIDWhenMentionTypeIsOmitted(t *testing.T) {
	c := testChannel(t, &recordingSink{})
	c.botOpenID, c.botIdentityFetchedAt = "ou_this_bot", time.Now()
	var raw map[string]any
	if err := json.Unmarshal(localGroupEvent("event", "om_root", "ou_sender", "", "", true), &raw); err != nil {
		t.Fatal(err)
	}
	mention := raw["event"].(map[string]any)["message"].(map[string]any)["mentions"].([]any)[0].(map[string]any)
	delete(mention, "mentioned_type")
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var event larkim.P2MessageReceiveV1
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	message, accepted, err := c.normalize(&event)
	if err != nil || !accepted || len(message.MentionedIDs) != 1 || message.MentionedIDs[0] != "ou_this_bot" {
		t.Fatalf("structured mention lost bot Open ID: %+v accepted=%t err=%v", message, accepted, err)
	}
	if addressed, err := c.AddressedToBot(context.Background(), message); err != nil || !addressed {
		t.Fatalf("bot mention without type was not admitted: addressed=%t err=%v", addressed, err)
	}
	message.MentionedIDs = []string{"ou_other_bot"}
	if addressed, err := c.AddressedToBot(context.Background(), message); err != nil || addressed {
		t.Fatalf("different Open ID triggered bot: addressed=%t err=%v", addressed, err)
	}
}

func TestStoppedChannelRejectsLateWebSocketEvent(t *testing.T) {
	sink := &recordingSink{}
	c := testChannel(t, sink)
	var event larkim.P2MessageReceiveV1
	if err := json.Unmarshal(testEventJSON(), &event); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.onMessage(context.Background(), &event); err == nil || len(sink.messages) != 0 {
		t.Fatalf("stopped Channel accepted a late event: %v messages=%d", err, len(sink.messages))
	}
}

func TestChannelStopWaitsForInflightIngress(t *testing.T) {
	sink := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	c := testChannel(t, sink)
	var event larkim.P2MessageReceiveV1
	if err := json.Unmarshal(testEventJSON(), &event); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() { accepted <- c.onMessage(context.Background(), &event) }()
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("event did not enter durable acceptance")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Stop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop did not report undrained in-flight acceptance: %v", err)
	}
	close(sink.release)
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight acceptance did not finish")
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop did not finish draining after event completed: %v", err)
	}
	if err := c.onMessage(context.Background(), &event); err == nil {
		t.Fatal("stopped Channel accepted another event")
	}
}

func TestCallbackRejectsInvalidSignatureAndStaleTimestamp(t *testing.T) {
	sink := &recordingSink{}
	c := testChannel(t, sink)
	goodTime := strconv.FormatInt(time.Now().Unix(), 10)
	request := signedCallback(t, testEventJSON(), goodTime)
	request.Header.Set(larkevent.EventSignature, "invalid")
	recorder := httptest.NewRecorder()
	c.CallbackHandler().ServeHTTP(recorder, request)
	assertCallbackJSON(t, recorder, http.StatusUnauthorized)
	recorder = httptest.NewRecorder()
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	c.CallbackHandler().ServeHTTP(recorder, signedCallback(t, testEventJSON(), stale))
	assertCallbackJSON(t, recorder, http.StatusUnauthorized)
	if recorder.Code != http.StatusUnauthorized || len(sink.messages) != 0 {
		t.Fatalf("stale callback accepted: status=%d, messages=%d", recorder.Code, len(sink.messages))
	}
}

func TestCallbackURLVerificationRequiresEncryptedMatchingToken(t *testing.T) {
	c := testChannel(t, &recordingSink{})
	for _, test := range []struct {
		token  string
		signed bool
		want   int
	}{{testToken, true, http.StatusOK}, {testToken, false, http.StatusOK}, {"wrong-token", false, http.StatusUnauthorized}} {
		plain, _ := json.Marshal(map[string]string{"type": "url_verification", "challenge": "challenge-123", "token": test.token})
		request := signedCallback(t, plain, strconv.FormatInt(time.Now().Unix(), 10))
		if !test.signed {
			request.Header.Del(larkevent.EventSignature)
		}
		response := httptest.NewRecorder()
		c.CallbackHandler().ServeHTTP(response, request)
		body := assertCallbackJSON(t, response, test.want)
		if test.want == http.StatusOK && (len(body) != 1 || body["challenge"] != "challenge-123") {
			t.Fatalf("challenge response = %#v", body)
		}
	}
}

func TestCallbackDoesNotAckFailedDurableAcceptance(t *testing.T) {
	sink := &recordingSink{err: errors.New("storage unavailable")}
	c := testChannel(t, sink)
	recorder := httptest.NewRecorder()
	c.CallbackHandler().ServeHTTP(recorder, signedCallback(t, testEventJSON(), strconv.FormatInt(time.Now().Unix(), 10)))
	assertCallbackJSON(t, recorder, http.StatusServiceUnavailable)
}

func TestCallbackRedeliveryAfterStorageFailureIsDurablyDeduplicated(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	channel := testChannel(t, channel.Ingress{Inbox: &flakyInbox{store: store, failOnce: true}})
	for _, want := range []int{http.StatusServiceUnavailable, http.StatusOK, http.StatusOK} {
		response := httptest.NewRecorder()
		channel.CallbackHandler().ServeHTTP(response, signedCallback(t, testEventJSON(), strconv.FormatInt(time.Now().Unix(), 10)))
		assertCallbackJSON(t, response, want)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || item.Message.EventID != "event-1" {
		t.Fatalf("redelivered event missing: %+v %t %v", item, found, err)
	}
	if err := store.Ignore(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("duplicate event queued: %t %v", found, err)
	}
}

func TestConfigRequiresCallbackSecrets(t *testing.T) {
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback})
	credentials, _ := json.Marshal(Credentials{AppSecret: "secret"})
	if err := (Provider{}).Validate(config, credentials); err == nil {
		t.Fatal("callback accepted without verification credentials")
	}
}

func TestProviderRejectsUnknownConfigVersion(t *testing.T) {
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	credentials, _ := json.Marshal(Credentials{AppSecret: "test-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	for _, version := range []int{0, 2} {
		binding := channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: version, Config: config}
		if _, err := (Provider{Deliveries: &memoryStreamStore{}}).Open(context.Background(), binding, credentials, &recordingSink{}); err == nil {
			t.Fatalf("Feishu config version %d was interpreted as v1", version)
		}
	}
}

func TestGroupAdmissionMatchesThisBotAndCachesIdentity(t *testing.T) {
	c := testChannel(t, &recordingSink{})
	infoCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
		case "/open-apis/bot/v3/info":
			infoCalls++
			_, _ = io.WriteString(w, `{"code":0,"bot":{"open_id":"ou_this_bot"}}`)
		default:
			t.Errorf("unexpected Feishu request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	message := channel.InboundMessage{BindingID: "bot-a", ChatKind: channel.ChatGroup, MentionedIDs: []string{"ou_other_bot"}}
	if addressed, err := c.AddressedToBot(context.Background(), message); err != nil || addressed {
		t.Fatalf("other bot mention admitted: %t %v", addressed, err)
	}
	message.MentionedIDs = []string{"ou_this_bot"}
	if addressed, err := c.AddressedToBot(context.Background(), message); err != nil || !addressed {
		t.Fatalf("own bot mention rejected: %t %v", addressed, err)
	}
	if infoCalls != 1 {
		t.Fatalf("bot identity fetched %d times", infoCalls)
	}
}
