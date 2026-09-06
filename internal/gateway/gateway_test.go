package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/sdk"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/fasthttp/websocket"
	"github.com/hashicorp/yamux"
)

func TestHostedRoutesAndCredentialRoles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var aValid atomic.Bool
	aValid.Store(true)
	grants := map[string]Grant{
		"machine-a": {Target: "a", Role: "daemon", Valid: aValid.Load},
		"machine-b": {Target: "b", Role: "daemon"},
		"browser-a": {Target: "a", Role: "sdk", Valid: aValid.Load},
		"browser-b": {Target: "b", Role: "sdk"},
	}
	g := New(func(token string) (Grant, bool) { a, ok := grants[token]; return a, ok })
	server := httptest.NewServer(g)
	defer server.Close()
	defer g.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/tunnel"
	register := func(target string) *yamux.Session {
		t.Helper()
		s, err := wire.Dial(ctx, endpoint, "machine-"+target, nil)
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
		s, err := wire.Dial(ctx, endpoint, attempt.token, nil)
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
	g.Disconnect("a")
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
}
