package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiomni/dune/im/channel"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestOversizedFinalRepliesAreKnownLocalRejections(t *testing.T) {
	for _, item := range []struct {
		mode string
		text string
	}{
		{mode: ReplyFinalCard, text: strings.Repeat("好", 12*1024)},
		{mode: ReplyFinalText, text: strings.Repeat("好", 55*1024)},
	} {
		c := testFinalChannel(t, item.mode)
		message := testStreamMessage(item.text)
		if _, err := c.Send(context.Background(), testGroupAddress(), message); !errors.Is(err, channel.ErrOutboundRejected) {
			t.Fatalf("%s oversized reply was not classified as no external effect: %v", item.mode, err)
		}
	}
}

func TestFinalReplyMeasuresDoubleEscapedRequestBody(t *testing.T) {
	for _, item := range []struct {
		mode  string
		text  string
		limit int
	}{
		{mode: ReplyFinalCard, text: strings.Repeat(`"`, 10*1024), limit: 30 * 1024},
		{mode: ReplyFinalText, text: strings.Repeat(`"`, 70*1024), limit: 150 * 1024},
	} {
		t.Run(item.mode, func(t *testing.T) {
			c := testFinalChannel(t, item.mode)
			_, content, err := c.finalContent(item.text)
			if err != nil || len(content) >= item.limit {
				t.Fatalf("inner content should fit, demonstrating outer escaping: bytes=%d err=%v", len(content), err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			c.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
			if _, err := c.Send(context.Background(), testGroupAddress(), testStreamMessage(item.text)); !errors.Is(err, channel.ErrOutboundRejected) {
				t.Fatalf("oversized group wire request was not rejected locally: %v", err)
			}
			directData, _ := json.Marshal(replyAddress{ChatKind: channel.ChatDirect, ChatID: "oc_dm", SenderOpenID: "ou_alice", ReplyMessageID: "om_input"})
			directAddress := channel.ReplyAddress{Provider: Kind, Version: 1, Data: directData}
			directMessage := testStreamMessage(item.text)
			directMessage.Session.ChatID, directMessage.Session.SubjectID = "oc_dm", "ou_alice"
			if _, err := c.Send(context.Background(), directAddress, directMessage); !errors.Is(err, channel.ErrOutboundRejected) {
				t.Fatalf("oversized direct wire request was not rejected locally: %v", err)
			}
			if calls != 0 {
				t.Fatalf("%s made %d external requests before rejecting the oversized body", item.mode, calls)
			}
		})
	}
}

func testFinalChannel(t *testing.T, mode string) *Channel {
	t.Helper()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveCallback, ReplyMode: mode})
	secret, _ := json.Marshal(Credentials{AppSecret: "test-secret", EncryptKey: testEncryptKey, VerificationToken: testToken})
	opened, err := (Provider{Deliveries: &memoryStreamStore{}}).Open(context.Background(), channel.BotBinding{
		ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1, Config: config,
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

func TestGroupReplyRejectsWrongSessionRootBeforeSending(t *testing.T) {
	message := testStreamMessage("hello")
	message.Session.SubjectID = "om_another_topic"
	for _, mode := range []string{ReplyFinalText, ReplyFinalCard} {
		c := testFinalChannel(t, mode)
		if _, err := c.Send(context.Background(), testGroupAddress(), message); err == nil {
			t.Fatalf("%s accepted a different topic's session", mode)
		}
	}
	store := &memoryStreamStore{}
	c := testStreamingChannel(t, store)
	if _, err := c.OpenStream(context.Background(), testGroupAddress(), message); err == nil {
		t.Fatal("streaming_card accepted a different topic's session")
	}
	if store.states != nil {
		t.Fatal("wrong-topic stream reserved a delivery")
	}
	threadData, _ := json.Marshal(replyAddress{ChatKind: channel.ChatGroup, ChatID: "oc_group", ReplyMessageID: "om_reply", RootMessageID: "om_root", ThreadID: "omt_thread"})
	threadAddress := channel.ReplyAddress{Provider: Kind, Version: 1, Data: threadData}
	if _, err := testFinalChannel(t, ReplyFinalText).Send(context.Background(), threadAddress, message); err == nil {
		t.Fatal("confirmed thread accepted a different root session")
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
