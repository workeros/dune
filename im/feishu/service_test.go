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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/im/sqlite"
	websocket "github.com/gorilla/websocket"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

type testCredentialResolver struct {
	data []byte
	err  error
}

func (r *testCredentialResolver) ResolveCredentials(context.Context, channel.BotBinding) ([]byte, error) {
	return r.data, r.err
}

type credentialResolverFunc func(context.Context, channel.BotBinding) ([]byte, error)

func (f credentialResolverFunc) ResolveCredentials(ctx context.Context, binding channel.BotBinding) ([]byte, error) {
	return f(ctx, binding)
}

type unusedAgentBackend struct{}

type capabilityBackend struct {
	unusedAgentBackend
	caps channel.AgentCapabilities
}

type capabilityBackendFunc struct {
	unusedAgentBackend
	check func(context.Context, channel.ConversationSession) (channel.AgentCapabilities, error)
}

func (b capabilityBackendFunc) Capabilities(ctx context.Context, session channel.ConversationSession) (channel.AgentCapabilities, error) {
	return b.check(ctx, session)
}

func (b *capabilityBackend) Capabilities(context.Context, channel.ConversationSession) (channel.AgentCapabilities, error) {
	return b.caps, nil
}

func (unusedAgentBackend) Capabilities(context.Context, channel.ConversationSession) (channel.AgentCapabilities, error) {
	return channel.AgentCapabilities{Adapter: "acp", AssistantDeltas: true, ReliableFinal: true}, nil
}
func (unusedAgentBackend) Start(context.Context, channel.ConversationSession) (channel.AgentSession, error) {
	panic("unused")
}
func (unusedAgentBackend) Attach(context.Context, channel.ConversationSession, channel.AgentSession) (channel.AgentSession, error) {
	panic("unused")
}
func (unusedAgentBackend) Prompt(context.Context, channel.ConversationSession, channel.AgentSession, string, func(channel.AgentEvent) error) (string, error) {
	panic("unused")
}
func (unusedAgentBackend) Stop(context.Context, channel.ConversationSession, channel.AgentSession) error {
	panic("unused")
}

func serviceSignedCallback(t *testing.T, plain []byte, key string) *http.Request {
	t.Helper()
	encrypted, err := larkcore.EncryptedEventMsg(context.Background(), plain, key)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"encrypt": encrypted})
	if err != nil {
		t.Fatal(err)
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	r := httptest.NewRequest(http.MethodPost, "/im/feishu/tenant-a/bot-a", bytes.NewReader(body))
	r.Header.Set(larkevent.EventRequestTimestamp, stamp)
	r.Header.Set(larkevent.EventRequestNonce, "nonce")
	r.Header.Set(larkevent.EventSignature, larkevent.Signature(stamp, "nonce", key, string(body)))
	return r
}

type delayedRequestBody struct {
	reader  io.Reader
	entered chan struct{}
	release chan struct{}
	started bool
}

func (b *delayedRequestBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		close(b.entered)
	}
	<-b.release
	return b.reader.Read(p)
}

func (*delayedRequestBody) Close() error { return nil }

func TestServiceUsesStoredBindingAndMountedCallbackFollowsRotation(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "secret-a", Target: channel.AgentTarget{RunnerID: "runner-a", AgentConfigID: "agent-a"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	resolver := &testCredentialResolver{data: secret}
	service, err := NewService(store, resolver, unusedAgentBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	forged := binding
	forged.Target.AgentConfigID = "agent-forged"
	forged.Config, _ = json.Marshal(Config{AppID: "cli_forged", ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalCard})
	if err := service.Activate(ctx, forged); err != nil {
		t.Fatal(err)
	}
	active, err := service.LookupBinding(ctx, binding.ID)
	if err != nil || active.Binding.Target.AgentConfigID != "agent-a" || active.ReplyMode != ReplyFinalText {
		t.Fatalf("unpersisted configuration was activated: %+v %v", active, err)
	}
	handler, err := service.CallbackHandler("tenant-a", binding.ID)
	if err != nil {
		t.Fatal(err)
	}

	binding, err = store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	newKey, newToken := "rotated-encrypt-key", "rotated-verification-token"
	resolver.data, _ = json.Marshal(Credentials{AppSecret: "new-app-secret", EncryptKey: newKey, VerificationToken: newToken})
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	oldResponse := httptest.NewRecorder()
	handler.ServeHTTP(oldResponse, serviceSignedCallback(t, testEventJSON(), testEncryptKey))
	if oldResponse.Code >= 200 && oldResponse.Code < 300 {
		t.Fatalf("old callback credentials accepted: %d", oldResponse.Code)
	}
	unsignedOld := serviceSignedCallback(t, testEventJSON(), testEncryptKey)
	unsignedOld.Header.Del(larkevent.EventSignature)
	unsignedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unsignedResponse, unsignedOld)
	if unsignedResponse.Code >= 200 && unsignedResponse.Code < 300 {
		t.Fatalf("unsigned old ciphertext accepted: %d", unsignedResponse.Code)
	}
	newEvent := []byte(strings.ReplaceAll(string(testEventJSON()), testToken, newToken))
	newResponse := httptest.NewRecorder()
	handler.ServeHTTP(newResponse, serviceSignedCallback(t, newEvent, newKey))
	if newResponse.Code != http.StatusOK {
		body, _ := io.ReadAll(newResponse.Body)
		t.Fatalf("rotated callback status=%d body=%q", newResponse.Code, body)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || item.Message.BindingID != binding.ID {
		t.Fatalf("rotated callback not queued: %+v %t %v", item, found, err)
	}
}

func TestLoadTenantSynchronizesMultipleBotsAndDisablesOne(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	var bindings []channel.BotBinding
	for _, id := range []string{"bot-a", "bot-b"} {
		binding, err := store.Put(ctx, channel.BotBinding{ID: id, TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
			Config: config, CredentialRef: "shared-ref", Target: channel.AgentTarget{RunnerID: "runner-a", AgentConfigID: id}, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, binding)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	service, err := NewService(store, &testCredentialResolver{data: secret}, unusedAgentBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	if err := service.LoadTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	firstA, firstB := service.bindings["bot-a"], service.bindings["bot-b"]
	if err := service.LoadTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if service.bindings["bot-a"] != firstA || service.bindings["bot-b"] != firstB {
		t.Fatal("unchanged Tenant sync restarted healthy Feishu channels")
	}
	service.mu.Lock()
	firstB.err = errors.New("WebSocket transport failed")
	service.mu.Unlock()
	if err := service.Activate(ctx, bindings[1]); err != nil {
		t.Fatal(err)
	}
	if service.bindings["bot-b"] == firstB {
		t.Fatal("failed channel was not replaced on same-revision activation")
	}
	status, err := service.Status(ctx, "tenant-a", "bot-a")
	if err != nil || status.TransportState != "callback_ready" || status.Stats.Queued != 0 {
		t.Fatalf("initial callback status: %+v %v", status, err)
	}
	if _, err := service.Issues(ctx, "another-tenant", "bot-a", 10); err == nil {
		t.Fatal("another Tenant inspected this bot's recovery state")
	}
	if err := service.ReconcileConfirmedDelivery(ctx, "another-tenant", channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "chat", SubjectID: "user"}, "event"); err == nil {
		t.Fatal("another Tenant reconciled this bot's recovery state")
	}
	if err := service.ReconcileRejectedDelivery(ctx, "another-tenant", channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "chat", SubjectID: "user"}, "event"); err == nil {
		t.Fatal("another Tenant reconciled this bot's rejected delivery")
	}
	if issues, err := service.Issues(ctx, "tenant-a", "bot-a", 10); err != nil || len(issues.Events)+len(issues.Conversations)+len(issues.Deliveries) != 0 {
		t.Fatalf("initial recovery issues: %+v %v", issues, err)
	}
	for _, binding := range bindings {
		if _, err := service.LookupBinding(ctx, binding.ID); err != nil {
			t.Fatalf("%s inactive: %v", binding.ID, err)
		}
	}
	mounted, err := service.CallbackHandler("tenant-a", "bot-a")
	if err != nil {
		t.Fatal(err)
	}
	bindings[0].Enabled = false
	if _, err := store.Put(ctx, bindings[0]); err != nil {
		t.Fatal(err)
	}
	preSync := httptest.NewRecorder()
	mounted.ServeHTTP(preSync, httptest.NewRequest(http.MethodPost, "/im/feishu/tenant-a/bot-a", nil))
	if preSync.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled Binding accepted callback before transport sync: %d", preSync.Code)
	}
	if active, err := service.LookupBinding(ctx, "bot-a"); err != nil || active.Binding.Enabled {
		t.Fatalf("disabled Binding still eligible before transport sync: %+v %v", active, err)
	}
	if err := service.LoadTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if disabled, err := service.LookupBinding(ctx, "bot-a"); err != nil || disabled.Binding.Enabled || disabled.Channel != nil {
		t.Fatalf("disabled bot remained active: %+v %v", disabled, err)
	}
	if _, err := service.LookupBinding(ctx, "bot-b"); err != nil {
		t.Fatalf("other bot stopped: %v", err)
	}
	response := httptest.NewRecorder()
	mounted.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/im/feishu/tenant-a/bot-a", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale mounted handler status=%d", response.Code)
	}
	if err := store.Insert(ctx, channel.InboundMessage{BindingID: "bot-a", EventID: "after-disable", MessageID: "om_after_disable"}); err != nil {
		t.Fatal(err)
	}
	status, err = service.Status(ctx, "tenant-a", "bot-a")
	if err != nil || status.TransportState != "disabled" || status.Stats.Queued != 1 {
		t.Fatalf("disabled Binding queue status: %+v %v", status, err)
	}
	processor := channel.Processor{Work: store, Conversations: store, Deliveries: store, Bindings: service, Agents: unusedAgentBackend{}}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("disabled Binding event was not drained: %t %v", found, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("disabled Binding event remained queued: %t %v", found, err)
	}
	status, err = service.Status(ctx, "tenant-a", "bot-a")
	if err != nil || status.Stats.Queued != 0 {
		t.Fatalf("disabled queue did not drain: %+v %v", status, err)
	}
}

func TestSlowCallbackCannotBlockServiceStop(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	service, err := NewService(store, &testCredentialResolver{data: secret}, unusedAgentBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	handler, err := service.CallbackHandler(binding.TenantID, binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := serviceSignedCallback(t, testEventJSON(), testEncryptKey)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = request.Body.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	request.Body = &delayedRequestBody{reader: bytes.NewReader(body), entered: entered, release: release}
	response := httptest.NewRecorder()
	callbackDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(callbackDone)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not begin reading its request body")
	}
	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	stopDone := make(chan error, 1)
	go func() { stopDone <- service.Stop(stopCtx) }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("slow pre-admission callback blocked service stop: %v", err)
		}
	case <-stopCtx.Done():
		t.Fatal("service stop waited for untrusted callback body under its registry lock")
	}
	close(release)
	select {
	case <-callbackDone:
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("retired callback was ACKed after Stop: %d", response.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retired callback did not finish after request body was released")
	}
}

func TestTenantCanMixWebSocketAndCallbackBindings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wsConfig, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveWebSocket, ReplyMode: ReplyFinalText})
	callbackAppID, callbackToken, callbackKey := "cli_callback_bot", "callback-token", "callback-encrypt-key"
	callbackConfig, _ := json.Marshal(Config{AppID: callbackAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalCard})
	for _, item := range []struct {
		id     string
		config json.RawMessage
	}{
		{id: "bot-ws", config: wsConfig},
		{id: "bot-callback", config: callbackConfig},
	} {
		if _, err := store.Put(ctx, channel.BotBinding{ID: item.id, TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
			Config: item.config, CredentialRef: item.id, Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: item.id}, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	wsSecret, _ := json.Marshal(Credentials{AppSecret: "ws-secret"})
	callbackSecret, _ := json.Marshal(Credentials{AppSecret: "callback-secret", EncryptKey: callbackKey, VerificationToken: callbackToken})
	secrets := map[string][]byte{"bot-ws": wsSecret, "bot-callback": callbackSecret}
	connected := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var gateway *httptest.Server
	gateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case larkws.GenEndpointUri:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(larkws.EndpointResp{Code: 0, Data: &larkws.Endpoint{
				Url:          "ws" + strings.TrimPrefix(gateway.URL, "http") + "/ws",
				ClientConfig: &larkws.ClientConfig{ReconnectCount: 0, PingInterval: 3600},
			}})
		case "/ws":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("local gateway upgrade: %v", err)
				return
			}
			defer conn.Close()
			connected <- struct{}{}
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer gateway.Close()
	resolver := credentialResolverFunc(func(_ context.Context, binding channel.BotBinding) ([]byte, error) {
		secret, ok := secrets[binding.CredentialRef]
		if !ok {
			return nil, errors.New("unexpected Feishu credential reference")
		}
		return secret, nil
	})
	service, err := NewService(store, resolver, unusedAgentBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	service.provider.wsDomain = gateway.URL
	if err := service.LoadTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatalf("mixed Tenant WebSocket bot did not connect: %v", ctx.Err())
	}
	if _, err := service.CallbackHandler("tenant-a", "bot-ws"); err == nil {
		t.Fatal("WebSocket bot exposed an HTTP callback handler")
	}
	handler, err := service.CallbackHandler("tenant-a", "bot-callback")
	if err != nil {
		t.Fatal(err)
	}
	for _, eventID := range []string{"callback-one", "callback-two"} {
		if eventID == "callback-two" {
			if err := service.Deactivate(ctx, "tenant-a", "bot-ws"); err != nil {
				t.Fatal(err)
			}
		}
		plain := localDirectEvent(eventID, "om_"+eventID, "ou_alice", "hello")
		plain = []byte(strings.ReplaceAll(strings.ReplaceAll(string(plain), testAppID, callbackAppID), testToken, callbackToken))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, serviceSignedCallback(t, plain, callbackKey))
		if response.Code != http.StatusOK {
			t.Fatalf("callback bot stopped with WebSocket bot: status=%d body=%q", response.Code, response.Body.String())
		}
	}
	if callbackStats, err := store.BindingStats(ctx, "bot-callback"); err != nil || callbackStats.Queued != 2 {
		t.Fatalf("callback ingress was not independent: %+v err=%v", callbackStats, err)
	}
	if websocketStats, err := store.BindingStats(ctx, "bot-ws"); err != nil || websocketStats.Queued != 0 {
		t.Fatalf("callback messages reached WebSocket bot: %+v err=%v", websocketStats, err)
	}
	if err := service.LoadTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatalf("WebSocket bot did not reconnect before Service.Stop: %v", ctx.Err())
	}
	service.mu.RLock()
	oldWebSocket := service.bindings["bot-ws"].channel
	service.mu.RUnlock()
	rotated, found, err := store.GetBinding(ctx, "tenant-a", "bot-ws")
	if err != nil || !found {
		t.Fatalf("load WebSocket Binding for rotation: found=%t err=%v", found, err)
	}
	rotated.CredentialRef = "bot-ws-rotated"
	if _, err := store.Put(ctx, rotated); err != nil {
		t.Fatal(err)
	}
	secrets[rotated.CredentialRef], _ = json.Marshal(Credentials{AppSecret: "rotated-ws-secret"})
	if err := service.LoadTenant(ctx, "tenant-a"); err != nil {
		t.Fatalf("rotate live WebSocket bot while callback bot stays active: %v", err)
	}
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatalf("rotated WebSocket bot did not connect: %v", ctx.Err())
	}
	select {
	case <-oldWebSocket.stopped:
	default:
		t.Fatal("old WebSocket receiver remained active after credential rotation")
	}
	service.mu.RLock()
	newWebSocket := service.bindings["bot-ws"].channel
	service.mu.RUnlock()
	if newWebSocket == oldWebSocket || newWebSocket.secret.AppSecret != "rotated-ws-secret" {
		t.Fatal("WebSocket credential rotation did not install a new channel")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, serviceSignedCallback(t,
		[]byte(strings.ReplaceAll(strings.ReplaceAll(string(localDirectEvent("callback-three", "om_callback-three", "ou_alice", "hello")), testAppID, callbackAppID), testToken, callbackToken)), callbackKey))
	if response.Code != http.StatusOK {
		t.Fatalf("callback bot stopped during another bot's credential rotation: %d", response.Code)
	}
	if err := service.Stop(ctx); err != nil {
		t.Fatalf("stopping a connected WebSocket bot and callback bot: %v", err)
	}
}

func TestActivateDoesNotExposeStoppedBindingAfterReplacementStopFailure(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	service, err := NewService(store, &testCredentialResolver{data: secret}, unusedAgentBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	first := service.bindings[binding.ID]
	failed := false
	first.stop = func(ctx context.Context) error {
		_ = first.channel.Stop(ctx)
		if !failed {
			failed = true
			return errors.New("simulated teardown failure")
		}
		return nil
	}
	binding, err = store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Activate(ctx, binding); err == nil {
		t.Fatal("replacement ignored old Channel stop failure")
	}
	if service.bindings[binding.ID] != first || !first.retiring {
		t.Fatal("stopped old Channel was not retained as an unrouteable teardown")
	}
	if _, err := service.LookupBinding(ctx, binding.ID); err == nil {
		t.Fatal("worker resolved stopped old Channel as active")
	}
	if _, err := service.CallbackHandler("tenant-a", binding.ID); err == nil {
		t.Fatal("stopped old Channel retained callback route")
	}
	if err := service.Activate(ctx, binding); err != nil || service.bindings[binding.ID] == nil {
		t.Fatalf("current revision could not be activated after teardown failure: %v", err)
	}
}

func TestTimedOutBindingTeardownCanBeRetried(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	service, err := NewService(store, &testCredentialResolver{data: secret}, unusedAgentBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	var event larkim.P2MessageReceiveV1
	if err := json.Unmarshal(testEventJSON(), &event); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"binding", "service"} {
		entry := service.bindings[binding.ID]
		sink := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
		defer func() {
			select {
			case <-sink.release:
			default:
				close(sink.release)
			}
		}()
		entry.channel.sink = sink
		accepted := make(chan error, 1)
		go func() { accepted <- entry.channel.onMessage(ctx, &event) }()
		select {
		case <-sink.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s event did not enter acceptance", scope)
		}
		stopCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		if scope == "binding" {
			err = service.Deactivate(stopCtx, binding.TenantID, binding.ID)
		} else {
			err = service.Stop(stopCtx)
		}
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s teardown did not report undrained callback: %v", scope, err)
		}
		if service.bindings[binding.ID] != entry || !entry.retiring {
			t.Fatalf("%s lost the pending teardown", scope)
		}
		if _, err := service.LookupBinding(ctx, binding.ID); err == nil {
			t.Fatalf("%s retiring Binding remained routeable", scope)
		}
		if _, err := service.CallbackHandler(binding.TenantID, binding.ID); err == nil {
			t.Fatalf("%s retiring Binding retained callback route", scope)
		}
		close(sink.release)
		select {
		case err := <-accepted:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s in-flight callback did not finish", scope)
		}
		if scope == "binding" {
			err = service.Deactivate(ctx, binding.TenantID, binding.ID)
		} else {
			err = service.Stop(ctx)
		}
		if err != nil || service.bindings[binding.ID] != nil {
			t.Fatalf("%s teardown could not be confirmed on retry: %v", scope, err)
		}
		if scope == "binding" {
			if err := service.Activate(ctx, binding); err != nil {
				t.Fatalf("Binding could not reactivate after confirmed teardown: %v", err)
			}
		}
	}
}

func TestActivateFailureDoesNotKeepOldReceiverConnected(t *testing.T) {
	for _, failure := range []string{"resolve_credentials", "parse_config", "open_provider"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
			binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
				Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
			resolver := &testCredentialResolver{data: secret}
			service, err := NewService(store, resolver, unusedAgentBackend{})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Stop(ctx)
			if err := service.Activate(ctx, binding); err != nil {
				t.Fatal(err)
			}
			old := service.bindings[binding.ID]
			mounted, err := service.CallbackHandler("tenant-a", binding.ID)
			if err != nil {
				t.Fatal(err)
			}
			binding, err = store.Put(ctx, binding)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "resolve_credentials":
				resolver.err = errors.New("secret store unavailable")
			case "parse_config":
				resolver.data = nil
			case "open_provider":
				service.provider.Deliveries = nil
			}
			if err := service.Activate(ctx, binding); err == nil {
				t.Fatal("failed replacement was reported as active")
			}
			if service.bindings[binding.ID] != nil || old.channel.TransportState() != "stopped" {
				t.Fatalf("old receiver survived failed replacement: entry=%v transport=%s", service.bindings[binding.ID], old.channel.TransportState())
			}
			response := httptest.NewRecorder()
			mounted.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/im/feishu/tenant-a/bot-a", nil))
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("mounted callback still resolved old receiver: %d", response.Code)
			}
		})
	}
}

func TestLoadTenantDeactivatesRemovedBotDespiteAnotherActivationFailure(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	valid, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	bindings := make(map[string]channel.BotBinding)
	for _, id := range []string{"bot-a", "bot-b"} {
		binding, err := store.Put(ctx, channel.BotBinding{ID: id, TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
			Config: valid, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		bindings[id] = binding
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	service, err := NewService(store, &testCredentialResolver{data: secret}, unusedAgentBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	if err := service.LoadTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	oldA, err := service.CallbackHandler("tenant-a", "bot-a")
	if err != nil {
		t.Fatal(err)
	}
	oldB, err := service.CallbackHandler("tenant-a", "bot-b")
	if err != nil {
		t.Fatal(err)
	}
	a := bindings["bot-a"]
	a.Enabled = false
	if _, err := store.Put(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := bindings["bot-b"]
	b.Config = json.RawMessage(`{"app_id":"invalid","receive_mode":"callback"}`)
	if _, err := store.Put(ctx, b); err != nil {
		t.Fatal(err)
	}
	var staleEvent larkim.P2MessageReceiveV1
	if err := json.Unmarshal(testEventJSON(), &staleEvent); err != nil {
		t.Fatal(err)
	}
	if err := service.bindings["bot-b"].channel.onMessage(ctx, &staleEvent); err == nil {
		t.Fatal("stale WebSocket-style event was durably accepted after revision change")
	}
	if stats, err := store.BindingStats(ctx, "bot-b"); err != nil || stats.Queued != 0 {
		t.Fatalf("stale event reached the inbox: %+v %v", stats, err)
	}
	if _, err := store.Put(ctx, channel.BotBinding{ID: "bot-c", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: valid, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := service.LoadTenant(ctx, "tenant-a"); err == nil {
		t.Fatal("invalid bot-b configuration was not reported")
	}
	if service.bindings["bot-a"] != nil || service.bindings["bot-b"] != nil || service.bindings["bot-c"] == nil {
		t.Fatalf("partial failure skipped independent deactivation or activation: active=%v", service.bindings)
	}
	for _, handler := range []http.Handler{oldA, oldB} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/im/feishu/tenant-a/bot", nil))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("removed or stale Binding still accepted callback: %d", response.Code)
		}
	}
}

func TestLoadTenantFailureDoesNotStopConcurrentlyActivatedNewerRevision(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	resolver := credentialResolverFunc(func(_ context.Context, current channel.BotBinding) ([]byte, error) {
		if current.Revision == 2 {
			close(entered)
			<-release
			return nil, errors.New("revision 2 credentials unavailable")
		}
		return secret, nil
	})
	service, err := NewService(store, resolver, unusedAgentBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	binding, err = store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	loadDone := make(chan error, 1)
	go func() { loadDone <- service.LoadTenant(ctx, "tenant-a") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("LoadTenant did not begin activating revision 2")
	}
	binding, err = store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	newer := service.bindings[binding.ID]
	close(release)
	select {
	case err := <-loadDone:
		if err == nil {
			t.Fatal("failed revision 2 activation was not reported")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadTenant did not finish after failed activation")
	}
	if service.bindings[binding.ID] != newer || newer.channel.TransportState() != "callback_ready" {
		t.Fatal("failed stale sync stopped a concurrently activated newer receiver")
	}
}

func TestActivateRejectsBindingChangedDuringCredentialResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	resolver := credentialResolverFunc(func(_ context.Context, current channel.BotBinding) ([]byte, error) {
		if current.Revision == 2 {
			close(entered)
			<-release
		}
		return secret, nil
	})
	service, err := NewService(store, resolver, unusedAgentBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	binding, err = store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	activation := make(chan error, 1)
	go func() { activation <- service.Activate(ctx, binding) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatalf("credential resolution did not start: %v", ctx.Err())
	}
	binding, err = store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-activation:
		if err == nil {
			t.Fatal("obsolete Binding revision was activated")
		}
	case <-ctx.Done():
		t.Fatalf("stale activation did not finish: %v", ctx.Err())
	}
	if service.bindings[binding.ID] != nil {
		t.Fatal("old or obsolete receiver remained active after stale activation")
	}
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatalf("latest Binding revision cannot activate: %v", err)
	}
}

func TestActivateHealthyBindingRechecksRevisionAfterAgentValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	checks := 0
	agents := capabilityBackendFunc{check: func(_ context.Context, _ channel.ConversationSession) (channel.AgentCapabilities, error) {
		checks++
		if checks == 2 {
			close(entered)
			<-release
		}
		return channel.AgentCapabilities{Adapter: "acp", ReliableFinal: true, AssistantDeltas: true}, nil
	}}
	service, err := NewService(store, &testCredentialResolver{data: secret}, agents)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	activation := make(chan error, 1)
	go func() { activation <- service.Activate(ctx, binding) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatalf("same-revision Agent validation did not start: %v", ctx.Err())
	}
	binding, err = store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-activation:
		if err == nil {
			t.Fatal("stale healthy Binding was accepted after rotation")
		}
	case <-ctx.Done():
		t.Fatalf("same-revision activation did not finish: %v", ctx.Err())
	}
	if service.bindings[binding.ID] != nil {
		t.Fatal("obsolete healthy receiver remained active")
	}
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatalf("latest Binding revision cannot activate: %v", err)
	}
}

func TestStreamingCardRejectsNonACPBeforeBindingActivation(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: ReplyStreaming})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	agents := &capabilityBackend{caps: channel.AgentCapabilities{Adapter: "pty"}}
	resolver := &testCredentialResolver{data: secret}
	service, err := NewService(store, resolver, agents)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	if err := service.Activate(ctx, binding); err == nil || service.bindings[binding.ID] != nil {
		t.Fatalf("PTY streaming card bot was activated: %v", err)
	}
	agents.caps = channel.AgentCapabilities{Adapter: "acp", ReliableFinal: true}
	if err := service.Activate(ctx, binding); err == nil || service.bindings[binding.ID] != nil {
		t.Fatalf("ACP without assistant deltas was activated: %v", err)
	}
	agents.caps = channel.AgentCapabilities{Adapter: "acp", AssistantDeltas: true, ReliableFinal: true}
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if service.bindings[binding.ID] == nil {
		t.Fatal("valid ACP streaming bot did not activate")
	}
	healthy := service.bindings[binding.ID]
	resolver.data = nil // an unchanged active Binding does not need secret resolution
	if err := service.LoadTenant(ctx, "tenant-a"); err != nil || service.bindings[binding.ID] != healthy {
		t.Fatalf("unchanged streaming bot was restarted or required credentials: %v", err)
	}
	agents.caps = channel.AgentCapabilities{Adapter: "pty"}
	if err := service.Activate(ctx, binding); err == nil || service.bindings[binding.ID] != nil {
		t.Fatalf("changed AgentConfig left an incompatible streaming bot active: %v", err)
	}
	if _, err := service.CallbackHandler("tenant-a", binding.ID); err == nil {
		t.Fatal("incompatible streaming bot retained a callback receiver")
	}
}

func TestFinalModesValidateReliableACPBeforeBindingActivation(t *testing.T) {
	for _, mode := range []string{ReplyFinalText, ReplyFinalCard} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: mode})
			binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
				Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			secret, _ := json.Marshal(Credentials{AppSecret: "app-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
			agents := &capabilityBackend{caps: channel.AgentCapabilities{Adapter: "pty"}}
			service, err := NewService(store, &testCredentialResolver{data: secret}, agents)
			if err != nil {
				t.Fatal(err)
			}
			defer service.Stop(ctx)
			if err := service.Activate(ctx, binding); err == nil || service.bindings[binding.ID] != nil {
				t.Fatalf("PTY final reply bot was activated: %v", err)
			}
			agents.caps = channel.AgentCapabilities{Adapter: "acp"}
			if err := service.Activate(ctx, binding); err == nil || service.bindings[binding.ID] != nil {
				t.Fatalf("ACP without reliable final state was activated: %v", err)
			}
			agents.caps = channel.AgentCapabilities{Adapter: "acp", ReliableFinal: true}
			if err := service.Activate(ctx, binding); err != nil || service.bindings[binding.ID] == nil {
				t.Fatalf("valid final reply bot without deltas did not activate: %v", err)
			}
			agents.caps = channel.AgentCapabilities{Adapter: "pty"}
			if err := service.Activate(ctx, binding); err == nil || service.bindings[binding.ID] != nil {
				t.Fatalf("changed AgentConfig left incompatible final bot active: %v", err)
			}
		})
	}
}
