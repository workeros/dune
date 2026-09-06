package tunnel_test

import (
	"context"
	"fmt"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/transport/tunnel"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/sdk"
	"github.com/aiomni/dune/pkg/transport/ws"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/fasthttp/websocket"
	"github.com/hashicorp/yamux"
	"github.com/valyala/fasthttp"
)

func TestHostedRoutesAndCredentialRoles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var aValid atomic.Bool
	aValid.Store(true)
	grants := map[string]access.Grant{
		"machine-a": {Target: "a", Role: "daemon", Valid: aValid.Load},
		"machine-b": {Target: "b", Role: "daemon"},
		"browser-a": {Target: "a", Role: "sdk", Valid: aValid.Load},
		"browser-b": {Target: "b", Role: "sdk"},
	}
	g := gateway.New()
	handler := tunnel.NewHandler(ctx, g, func(token string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		a, ok := grants[token]
		if !ok {
			return gateway.BindingContext{}, nil, fmt.Errorf("unauthorized")
		}
		return a.Bind()
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	defer g.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/tunnel"
	register := func(target string) *yamux.Session {
		t.Helper()
		conn, err := ws.Dial(ctx, endpoint, "machine-"+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		s, err := yamux.Client(conn, wire.Config())
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = wire.Handshake(s, &pb.Message{Kind: "hello", Target: target, Incarnation: target + "-inc", ConnectionGeneration: 1,
			Payload: api.Payload(api.Hello{Version: api.Version, Role: "daemon"}), Data: api.Payload(api.Binding{Capabilities: []string{"runtime.list"}})})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				raw, err := s.AcceptStream()
				if err != nil {
					return
				}
				st := wire.Wrap(raw)
				m, err := st.Recv()
				if err == nil {
					if m.Target != target {
						t.Errorf("request escaped target binding: %s != %s", m.Target, target)
					}
					_ = st.Send(&pb.Message{Kind: "accepted"})
					_ = st.Send(&pb.Message{Kind: "result", Payload: api.Payload(map[string]string{"machine": target})})
				}
				st.Close()
			}
		}()
		return s
	}
	a := register("a")
	defer a.Close()
	b := register("b")
	defer b.Close()
	if !g.Online("a") || !g.Online("b") || g.Online("unknown") {
		t.Fatal("incorrect online routes")
	}
	dial := func(target string) *sdk.Client {
		t.Helper()
		c, err := sdk.Dial(ctx, sdk.Options{Gateway: endpoint, Token: "browser-" + target, Target: target})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ca, cb := dial("a"), dial("b")
	defer ca.Close()
	defer cb.Close()
	check := func(c *sdk.Client, target string) {
		t.Helper()
		var out struct {
			Machine string `json:"machine"`
		}
		if err := c.Call(ctx, "runtime.list", struct{}{}, &out); err != nil {
			t.Fatal(err)
		}
		if out.Machine != target {
			t.Fatalf("incorrect route: %+v", out)
		}
	}
	check(ca, "a")
	check(cb, "b")
	for _, attempt := range []struct{ token, target, role string }{
		{"browser-a", "b", "sdk"},
		{"machine-a", "b", "daemon"},
		{"machine-a", "a", "sdk"},
		{"browser-a", "a", "daemon"},
	} {
		conn, err := ws.Dial(ctx, endpoint, attempt.token, nil)
		if err != nil {
			t.Fatal(err)
		}
		s, err := yamux.Client(conn, wire.Config())
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = wire.Handshake(s, &pb.Message{Kind: "hello", Target: attempt.target, Incarnation: "forged", ConnectionGeneration: 1,
			Payload: api.Payload(api.Hello{Version: api.Version, Role: attempt.role}), Data: api.Payload(api.Binding{})})
		s.Close()
		if err == nil {
			t.Fatalf("accepted cross-machine or wrong-role credential: %+v", attempt)
		}
	}
	check(ca, "a")
	check(cb, "b")
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	wc, response, err := dialer.Dial(endpoint, http.Header{"Authorization": []string{"Bearer browser-a"}, "Origin": []string{server.URL}})
	if wc != nil {
		wc.Close()
	}
	if err == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("browser origin was allowed on machine transport")
	}
	if response != nil {
		response.Body.Close()
	}
	aValid.Store(false)
	// No traffic or explicit Disconnect: the access module must revoke idle
	// tunnels as well as streams carrying new input.
	select {
	case <-a.CloseChan():
	case <-time.After(3 * time.Second):
		t.Fatal("idle revoked tunnel remained connected")
	}
	if err := ca.Call(ctx, "runtime.list", struct{}{}, nil); err == nil {
		t.Fatal("revoked connection can still issue requests")
	}
	if g.Online("a") {
		t.Fatal("revoked route still online")
	}
	check(cb, "b")
	if c, err := sdk.Dial(ctx, sdk.Options{Gateway: endpoint, Token: "browser-a", Target: "a"}); err == nil {
		c.Close()
		t.Fatal("revoked credential can reconnect")
	}
	g.Disconnect("b")
	if g.Online("b") {
		t.Fatal("explicit disconnect left route online")
	}
}

func TestFastHTTPGatewayCloseUnblocksHijackedConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g := gateway.New()
	defer g.Close()
	handler := tunnel.NewHandler(ctx, g, func(token string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		return (access.Grant{Target: "machine", Role: gateway.RoleDaemon}).Bind()
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// KeepHijackedConns intentionally retains fasthttp's default false value.
	server := &fasthttp.Server{Handler: handler.ServeFastHTTP}
	go func() { _ = server.Serve(listener) }()
	conn, err := ws.Dial(ctx, "ws://"+listener.Addr().String()+"/tunnel", "token", nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := yamux.Client(conn, wire.Config())
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	defer session.Close()
	_, _, err = wire.Handshake(session, &pb.Message{Kind: "hello", Target: "machine", Incarnation: "boot", ConnectionGeneration: 1,
		Payload: api.Payload(api.Hello{Version: api.Version, Role: gateway.RoleDaemon}), Data: api.Payload(api.Binding{})})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { g.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway shutdown waited for hijack handler to return")
	}
	select {
	case <-session.CloseChan():
	case <-time.After(time.Second):
		t.Fatal("gateway shutdown left the tunnel connected")
	}
}
