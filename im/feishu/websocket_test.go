package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/im/sqlite"
	websocket "github.com/gorilla/websocket"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// This exercises the actual SDK websocket bootstrap, frame dispatcher and
// response path against a local gateway. It does not replace live Feishu QA.
func TestWebSocketACKFollowsDurableAcceptance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := sqlite.OpenWithOptions(ctx, filepath.Join(t.TempDir(), "im.db"), sqlite.Options{MaxPendingPerBinding: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveWebSocket, ReplyMode: ReplyFinalText})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	credentials, _ := json.Marshal(Credentials{AppSecret: "test-secret"})
	responses := make(chan int, 4)
	serverErrors := make(chan error, 8)
	releaseGateway := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case larkws.GenEndpointUri:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(larkws.EndpointResp{Code: 0, Data: &larkws.Endpoint{
				Url:          "ws" + strings.TrimPrefix(server.URL, "http") + "/callback",
				ClientConfig: &larkws.ClientConfig{ReconnectCount: 0, PingInterval: 3600},
			}})
		case "/callback":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				serverErrors <- err
				return
			}
			defer conn.Close()
			events := [][]byte{
				localDirectEvent("event-1", "om_1", "ou_alice", "first"),
				localDirectEvent("event-2", "om_2", "ou_alice", "second"),
				localDirectEvent("event-1", "om_1", "ou_alice", "first"),
				localDirectEvent("event-1", "om_1", "ou_alice", "collision"),
			}
			for index, event := range events {
				headers := larkws.Headers{}
				headers.Add(larkws.HeaderType, string(larkws.MessageTypeEvent))
				headers.Add(larkws.HeaderMessageID, fmt.Sprintf("frame-%d", index))
				headers.Add(larkws.HeaderTraceID, "local-test")
				frame, err := (&larkws.Frame{Method: int32(larkws.FrameTypeData), Headers: headers, Payload: event}).Marshal()
				if err != nil {
					serverErrors <- err
					return
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
					serverErrors <- err
					return
				}
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				for {
					_, payload, err := conn.ReadMessage()
					if err != nil {
						serverErrors <- err
						return
					}
					var replyFrame larkws.Frame
					if err := replyFrame.Unmarshal(payload); err != nil {
						serverErrors <- err
						return
					}
					if replyFrame.Method != int32(larkws.FrameTypeData) || larkws.Headers(replyFrame.Headers).GetString(larkws.HeaderMessageID) != fmt.Sprintf("frame-%d", index) {
						continue // SDK control frames are not event ACKs.
					}
					var reply larkws.Response
					if err := json.Unmarshal(replyFrame.Payload, &reply); err != nil {
						serverErrors <- err
						return
					}
					responses <- reply.StatusCode
					break
				}
			}
			<-releaseGateway
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	defer close(releaseGateway)
	opened, err := (Provider{Deliveries: store, wsDomain: server.URL}).Open(ctx, binding, credentials, bindingIngress{store: store, binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	c := opened.(*Channel)
	startDone := make(chan error, 1)
	go func() { startDone <- c.Start(ctx) }()
	for index, want := range []int{http.StatusOK, http.StatusInternalServerError, http.StatusOK, http.StatusInternalServerError} {
		select {
		case got := <-responses:
			if got != want {
				t.Fatalf("websocket frame %d ACK=%d, want %d", index, got, want)
			}
		case err := <-serverErrors:
			t.Fatalf("local Feishu websocket gateway: %v", err)
		case <-ctx.Done():
			t.Fatalf("websocket frame %d was not acknowledged: %v", index, ctx.Err())
		}
	}
	if stats, err := store.BindingStats(ctx, binding.ID); err != nil || stats.Queued != 1 {
		t.Fatalf("websocket ACK did not match durable inbox: %+v err=%v", stats, err)
	}
	if c.TransportState() != "connected" {
		t.Fatalf("SDK never reported connected transport: %s", c.TransportState())
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("websocket Start after Stop: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("websocket Start did not stop: %v", ctx.Err())
	}
}

func TestWebSocketGroupEventReachesAgentAndThreadReply(t *testing.T) {
	for _, mode := range []string{ReplyFinalText, ReplyFinalCard, ReplyStreaming} {
		t.Run(mode, func(t *testing.T) {
			testWebSocketGroupEventReachesAgentAndThreadReply(t, mode)
		})
	}
}

func testWebSocketGroupEventReachesAgentAndThreadReply(t *testing.T, mode string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, _ := json.Marshal(Config{AppID: testAppID, ReceiveMode: ReceiveWebSocket, ReplyMode: mode})
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot-a", TenantID: "tenant-a", Provider: Kind, ConfigVersion: 1,
		Config: config, CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	credentials, _ := json.Marshal(Credentials{AppSecret: "test-secret"})
	connected := make(chan struct{}, 1)
	events := make(chan []byte, 1)
	acks := make(chan int, 1)
	replies := make(chan string, 1)
	cardCalls := make(chan string, 4)
	serverErrors := make(chan error, 8)
	releaseGateway := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case larkws.GenEndpointUri:
			_ = json.NewEncoder(w).Encode(larkws.EndpointResp{Code: 0, Data: &larkws.Endpoint{
				Url:          "ws" + strings.TrimPrefix(server.URL, "http") + "/callback",
				ClientConfig: &larkws.ClientConfig{ReconnectCount: 0, PingInterval: 3600},
			}})
		case "/callback":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				serverErrors <- err
				return
			}
			defer conn.Close()
			connected <- struct{}{}
			var event []byte
			select {
			case event = <-events:
			case <-ctx.Done():
				return
			}
			headers := larkws.Headers{}
			headers.Add(larkws.HeaderType, string(larkws.MessageTypeEvent))
			headers.Add(larkws.HeaderMessageID, "frame-1")
			headers.Add(larkws.HeaderTraceID, "local-group-test")
			frame, err := (&larkws.Frame{Method: int32(larkws.FrameTypeData), Headers: headers, Payload: event}).Marshal()
			if err != nil {
				serverErrors <- err
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				serverErrors <- err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					serverErrors <- err
					return
				}
				var responseFrame larkws.Frame
				if err := responseFrame.Unmarshal(payload); err != nil {
					serverErrors <- err
					return
				}
				if larkws.Headers(responseFrame.Headers).GetString(larkws.HeaderMessageID) != "frame-1" {
					continue
				}
				var response larkws.Response
				if err := json.Unmarshal(responseFrame.Payload, &response); err != nil {
					serverErrors <- err
					return
				}
				acks <- response.StatusCode
				break
			}
			select {
			case <-releaseGateway:
			case <-ctx.Done():
			}
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
		case "/open-apis/bot/v3/info":
			_, _ = io.WriteString(w, `{"code":0,"bot":{"open_id":"ou_this_bot"}}`)
		case "/open-apis/cardkit/v1/cards":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || mode != ReplyStreaming || !strings.Contains(fmt.Sprint(request["data"]), `"streaming_mode":true`) {
				serverErrors <- fmt.Errorf("invalid CardKit create request: %+v: %v", request, err)
			}
			cardCalls <- "create"
			_, _ = io.WriteString(w, `{"code":0,"data":{"card_id":"card-1"}}`)
		case "/open-apis/im/v1/messages/om_root/reply":
			var request struct {
				MsgType       string `json:"msg_type"`
				ReplyInThread bool   `json:"reply_in_thread"`
				UUID          string `json:"uuid"`
			}
			wantType := "text"
			if mode != ReplyFinalText {
				wantType = "interactive"
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.MsgType != wantType || !request.ReplyInThread || request.UUID == "" {
				serverErrors <- fmt.Errorf("WebSocket turn escaped group topic: %+v: %v", request, err)
			}
			replies <- r.URL.Path
			_, _ = io.WriteString(w, `{"code":0,"data":{"message_id":"om_answer"}}`)
		case "/open-apis/cardkit/v1/cards/card-1/elements/answer/content":
			var request struct {
				Content  string `json:"content"`
				Sequence int    `json:"sequence"`
				UUID     string `json:"uuid"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || mode != ReplyStreaming || request.Content == "" || request.Sequence < 1 || request.UUID == "" {
				serverErrors <- fmt.Errorf("invalid CardKit update request: %+v: %v", request, err)
			}
			cardCalls <- "update"
			_, _ = io.WriteString(w, `{"code":0}`)
		case "/open-apis/cardkit/v1/cards/card-1/settings":
			var request struct {
				Settings string `json:"settings"`
				Sequence int    `json:"sequence"`
				UUID     string `json:"uuid"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || mode != ReplyStreaming || !strings.Contains(request.Settings, `"streaming_mode":false`) || request.Sequence < 1 || request.UUID == "" {
				serverErrors <- fmt.Errorf("invalid CardKit close request: %+v: %v", request, err)
			}
			cardCalls <- "close"
			_, _ = io.WriteString(w, `{"code":0}`)
		default:
			serverErrors <- fmt.Errorf("unexpected Feishu API path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	defer close(releaseGateway)
	agents := &localAgentBackend{starts: map[string]int{}, attaches: map[string]int{}}
	service, err := NewService(store, &testCredentialResolver{data: credentials}, agents)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop(ctx)
	service.provider.wsDomain = server.URL
	if err := service.Activate(ctx, binding); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case err := <-serverErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatalf("local WebSocket did not connect: %v", ctx.Err())
	}
	service.bindings[binding.ID].channel.client = lark.NewClient(testAppID, "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithOAuthBaseUrl(server.URL))
	events <- localGroupEvent("event-1", "om_root", "ou_alice", "", "", true)
	select {
	case status := <-acks:
		if status != http.StatusOK {
			t.Fatalf("durable WebSocket event ACK=%d", status)
		}
	case err := <-serverErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatalf("local WebSocket event was not acknowledged: %v", ctx.Err())
	}
	processor := channel.Processor{Work: store, Conversations: store, Deliveries: store, Bindings: service, Agents: agents}
	if found, err := processor.ProcessOne(ctx); err != nil || !found {
		t.Fatalf("WebSocket event was not processed: found=%t err=%v", found, err)
	}
	select {
	case <-replies:
	case err := <-serverErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatalf("group thread reply was not sent: %v", ctx.Err())
	}
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
	cardCount := map[string]int{}
	for len(cardCalls) > 0 {
		cardCount[<-cardCalls]++
	}
	if mode == ReplyStreaming {
		if cardCount["create"] != 1 || cardCount["update"] < 1 || cardCount["close"] != 1 {
			t.Fatalf("WebSocket streaming reply did not create, update and close one CardKit entity: %+v", cardCount)
		}
	} else if len(cardCount) != 0 {
		t.Fatalf("final reply unexpectedly called CardKit: %+v", cardCount)
	}
	key := channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "oc_group", SubjectID: "om_root"}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationReady || agents.starts[key.String()] != 1 {
		t.Fatalf("WebSocket group session was not completed: state=%s found=%t starts=%d err=%v", state, found, agents.starts[key.String()], err)
	}
	if delivery, found, err := store.GetDelivery(ctx, binding.ID, channel.TurnDeliveryID(binding.ID, "event-1")); err != nil || !found || delivery.Phase != "complete" || !delivery.AgentTurnCompleted || delivery.Mode != mode {
		t.Fatalf("WebSocket delivery was not confirmed: %+v found=%t err=%v", delivery, found, err)
	}
}
