package feishu

import (
	"bytes"
	"context"
	"encoding/json"
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
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
)

type testCredentialResolver struct{ data []byte }

func (r *testCredentialResolver) ResolveCredentials(context.Context, channel.BotBinding) ([]byte, error) {
	return r.data, nil
}

type unusedAgentBackend struct{}

func (unusedAgentBackend) Capabilities(context.Context, channel.ConversationSession) (channel.AgentCapabilities, error) {
	panic("unused")
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
