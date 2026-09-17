package webapp

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/fasthttp/websocket"
	"github.com/hashicorp/yamux"
)

func TestBrowserEventsStreamCompletion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		adapter     string
		exitCode    *int
		streamError bool
		keepOpen    bool
		wantError   bool
	}{
		{name: "pty exit then EOF", adapter: "pty", exitCode: new(0)},
		{name: "pty nonzero exit then EOF", adapter: "pty", exitCode: new(7)},
		{name: "pty EOF before exit", adapter: "pty", wantError: true},
		{name: "pty error after exit", adapter: "pty", exitCode: new(0), streamError: true, wantError: true},
		{name: "acp EOF before exit", adapter: "acp", wantError: true},
		{name: "acp exit without EOF", adapter: "acp", exitCode: new(0), keepOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// More frames than the browser forwarding queue can hold, followed
			// immediately by exit/EOF, exercise buffered delivery at shutdown.
			var messages []*pb.Message
			for i := range 32 {
				messages = append(messages, &pb.Message{Kind: "data", Data: []byte(fmt.Sprintf("output %d\r\n", i))})
			}
			if tc.exitCode != nil {
				messages = append(messages, &pb.Message{Kind: "exit", Payload: api.Payload(*tc.exitCode)})
				if tc.adapter == "pty" {
					// Dead panes can still emit retained history after exit.
					messages = append(messages, &pb.Message{Kind: "data", Data: []byte("retained history\r\n")})
				}
			}
			outbound := append([]*pb.Message(nil), messages...)
			if tc.streamError {
				outbound = append(outbound, &pb.Message{Kind: "error", Code: "STREAM_INTERRUPTED", Detail: "connection reset"})
			}
			connection := browserEventConnection(t, tc.adapter, outbound, tc.keepOpen)
			for i, message := range messages {
				var event browserEvent
				if err := connection.ReadJSON(&event); err != nil {
					t.Fatalf("read event %d (%s): %v", i, message.Kind, err)
				}
				wantData := string(message.Data)
				wantBinary := tc.adapter == "pty" && message.Kind == "data"
				if wantBinary {
					wantData = base64.StdEncoding.EncodeToString(message.Data)
				}
				if event.Type != message.Kind || event.Data != wantData || event.Binary != wantBinary || string(event.Payload) != string(message.Payload) {
					t.Fatalf("event %d = %+v, want %s with data %q, binary %v and payload %s", i, event, message.Kind, wantData, wantBinary, message.Payload)
				}
			}
			if tc.wantError {
				var event browserEvent
				if err := connection.ReadJSON(&event); err != nil {
					t.Fatalf("read interruption: %v", err)
				}
				wantDetail := "EOF"
				if tc.streamError {
					wantDetail = "connection reset"
				}
				if event.Type != "error" || event.Code != "STREAM_INTERRUPTED" || !strings.Contains(event.Error, wantDetail) {
					t.Fatalf("interruption = %+v", event)
				}
			}
			var extra browserEvent
			err := connection.ReadJSON(&extra)
			if err == nil {
				t.Fatalf("unexpected event after completion: %+v", extra)
			}
			if tc.wantError {
				if !websocket.IsCloseError(err, websocket.CloseAbnormalClosure) {
					t.Fatalf("interrupted stream did not close: %v", err)
				}
			} else if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				t.Fatalf("confirmed exit did not close normally: %v", err)
			}
		})
	}
}

// Exercise the HTTP event route, SDK framing, and browser WebSocket against a
// scripted Gateway peer so exit and EOF can arrive without process timing gaps.
func browserEventConnection(t *testing.T, adapter string, messages []*pb.Message, keepOpen bool) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, err := OpenStore(filepath.Join(t.TempDir(), "metadata"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	localIdentity := identity.NewLocal(store, true)
	user, token, err := localIdentity.Register(ctx, "terminal@example.test", "terminal-test-password")
	if err != nil {
		t.Fatal(err)
	}
	enrollment, _, err := store.IssueEnrollment(ctx, user.ID, "terminal-machine")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := store.Enroll(ctx, enrollment, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	runtime := api.Runtime{ID: "terminal-runtime", Incarnation: "test-boot", Generation: 1, Adapter: adapter}
	local, remote := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveBrowserEventGateway(ctx, remote, machine.ID, runtime, messages, keepOpen) }()
	t.Cleanup(func() {
		cancel()
		local.Close()
		remote.Close()
		select {
		case err := <-done:
			if err != nil && !t.Failed() {
				t.Errorf("scripted Gateway: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("scripted Gateway did not stop")
		}
	})
	const origin = "http://dune.example.test"
	app, err := newTestServer(ctx, Options{PublicURL: origin, DialGateway: func(context.Context, string) (net.Conn, error) { return local, nil }}, store, localIdentity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/ws/machines/" + machine.ID + "/sessions/" + runtime.ID + "/events?incarnation=" + runtime.Incarnation + "&generation=1"
	header := http.Header{"Origin": {origin}, "Cookie": {(&http.Cookie{Name: cookieName, Value: token}).String()}}
	connection, response, err := websocket.DefaultDialer.DialContext(ctx, endpoint, header)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("connect browser events: %v", err)
	}
	t.Cleanup(func() { connection.Close() })
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return connection
}

func serveBrowserEventGateway(ctx context.Context, conn net.Conn, target string, runtime api.Runtime, messages []*pb.Message, keepOpen bool) error {
	session, err := yamux.Server(conn, wire.Config())
	if err != nil {
		return err
	}
	defer session.Close()
	stop := context.AfterFunc(ctx, func() { session.Close() })
	defer stop()
	raw, err := session.AcceptStream()
	if err != nil {
		return err
	}
	control := wire.Wrap(raw)
	if _, err := control.Recv(); err != nil {
		return err
	}
	if err := control.Send(&pb.Message{Kind: "welcome", Payload: api.Payload(api.Binding{Target: target, Incarnation: "test-boot", Generation: 1})}); err != nil {
		return err
	}
	for _, operation := range []string{"runtime.get", "runtime.attach"} {
		raw, err := session.AcceptStream()
		if err != nil {
			return err
		}
		stream := wire.Wrap(raw)
		request, err := stream.Recv()
		if err != nil {
			return err
		}
		if request.Operation != operation || request.RuntimeId != runtime.ID || request.RuntimeIncarnation != runtime.Incarnation || request.RuntimeGeneration != runtime.Generation {
			return fmt.Errorf("unexpected request: %v", request)
		}
		if err := stream.Send(&pb.Message{Kind: "accepted", RequestId: request.RequestId}); err != nil {
			return err
		}
		if operation == "runtime.get" {
			if err := stream.Send(&pb.Message{Kind: "result", Payload: api.Payload(runtime)}); err != nil {
				return err
			}
			stream.Close()
			continue
		}
		for _, message := range messages {
			if err := stream.Send(message); err != nil {
				return err
			}
		}
		if !keepOpen {
			stream.Close()
		}
	}
	// Keep the session alive until the browser handler closes its client, so
	// the test delivers a business stream EOF rather than a connection reset.
	_, _ = control.Recv()
	return nil
}
