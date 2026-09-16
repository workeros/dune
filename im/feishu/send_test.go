package feishu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiomni/dune/im/channel"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func testFinalChannel(t *testing.T, mode string) *Channel {
	t.Helper()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: mode})
	secret, _ := json.Marshal(Credentials{AppSecret: "test-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	opened, err := (Provider{Deliveries: &memoryStreamStore{}}).Open(context.Background(), channel.BotBinding{
		ID: "bot-a", TenantID: "tenant-a", Provider: Kind, Config: config,
	}, secret, &recordingSink{})
	if err != nil {
		t.Fatal(err)
	}
	return opened.(*Channel)
}

func TestFinalModesUseDirectOrThreadReplyAPIs(t *testing.T) {
	for _, mode := range []string{ReplyFinalText, ReplyFinalCard} {
		for _, chat := range []string{"direct", "group"} {
			t.Run(mode+"/"+chat, func(t *testing.T) {
				c := testFinalChannel(t, mode)
				var path string
				var body map[string]any
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
						_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
						return
					}
					calls++
					path = r.URL.Path
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("request body: %v", err)
					}
					_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_response"}}`)
				}))
				defer server.Close()
				c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
				var address channel.ReplyAddress
				var subject, chatID string
				if chat == "group" {
					address, subject, chatID = testGroupAddress(), "om_input", "oc_group"
				} else {
					data, _ := json.Marshal(replyAddress{ChatKind: channel.ChatDirect, ChatID: "oc_dm", SenderOpenID: "ou_alice", ReplyMessageID: "om_input"})
					address, subject, chatID = channel.ReplyAddress{Provider: Kind, Version: 1, Data: data}, "ou_alice", "oc_dm"
				}
				message := channel.OutboundMessage{Text: "hello", DeliveryID: "turn-1", Session: channel.SessionKey{
					TenantID: "tenant-a", BindingID: "bot-a", ChatID: chatID, SubjectID: subject,
				}}
				receipt, err := c.Send(context.Background(), address, message)
				if err != nil || calls != 1 || !strings.Contains(string(receipt), "om_response") {
					t.Fatalf("send: receipt=%s calls=%d err=%v", receipt, calls, err)
				}
				wantType := "text"
				if mode == ReplyFinalCard {
					wantType = "interactive"
				}
				if body["msg_type"] != wantType {
					t.Fatalf("message type: %+v", body)
				}
				if body["uuid"] != outboundUUID("bot-a", "turn-1", "final-message") {
					t.Fatalf("final message has no stable deduplication UUID: %+v", body)
				}
				if chat == "group" {
					if path != "/open-apis/im/v1/messages/om_input/reply" || body["reply_in_thread"] != true {
						t.Fatalf("group reply escaped thread: path=%s body=%+v", path, body)
					}
				} else if path != "/open-apis/im/v1/messages" || body["receive_id"] != "ou_alice" {
					t.Fatalf("direct send target: path=%s body=%+v", path, body)
				}
			})
		}
	}
}

func TestFinalReplyRequiresDeliveryID(t *testing.T) {
	c := testFinalChannel(t, ReplyFinalText)
	message := testStreamMessage("hello")
	message.DeliveryID = ""
	if _, err := c.Send(context.Background(), testGroupAddress(), message); err == nil {
		t.Fatal("Feishu send accepted an untracked delivery")
	}
	if outboundUUID("bot-a", "turn-1", "final-message") == outboundUUID("bot-a", "turn-2", "final-message") ||
		outboundUUID("bot-a", "turn-1", "final-message") == outboundUUID("bot-b", "turn-1", "final-message") ||
		outboundUUID("bot-a", "turn-1", "final-message") == outboundUUID("bot-a", "turn-1", "card-message") {
		t.Fatal("distinct deliveries, bindings or operations share a UUID")
	}
}

func TestFailedGroupReplyNeverFallsBackToTopLevel(t *testing.T) {
	c := testFinalChannel(t, ReplyFinalText)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, `{"code":19001,"msg":"reply failed"}`)
	}))
	defer server.Close()
	c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	_, err := c.Send(context.Background(), testGroupAddress(), testStreamMessage("hello"))
	if err == nil || len(paths) != 1 || paths[0] != "/open-apis/im/v1/messages/om_input/reply" {
		t.Fatalf("failed thread reply fell back: paths=%v err=%v", paths, err)
	}
}
