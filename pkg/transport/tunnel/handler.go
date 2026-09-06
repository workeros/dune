// Package tunnel authenticates and upgrades the default HTTP/WebSocket tunnel.
// Applications own routing, listeners, and the lifetime context.
package tunnel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/transport/ws"
	"github.com/fasthttp/websocket"
	"github.com/valyala/fasthttp"
)

type Authorize func(token string) (gateway.BindingContext, gateway.ConnectionHandler, error)

type Handler struct {
	ctx       context.Context
	gateway   *gateway.Gateway
	authorize Authorize
	slots     chan struct{}
}

// NewHandler uses the application lifetime, not a framework request context:
// returning from a hijacked HTTP handler must not cancel an established tunnel.
func NewHandler(ctx context.Context, g *gateway.Gateway, authorize Authorize) *Handler {
	return &Handler{ctx: ctx, gateway: g, authorize: authorize, slots: make(chan struct{}, 256)}
}

func (h *Handler) authenticate(header string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" || h.authorize == nil {
		return gateway.BindingContext{}, nil, fmt.Errorf("unauthorized")
	}
	binding, handler, err := h.authorize(token)
	if err == nil && (handler == nil || binding.Target == "" || (binding.Role != gateway.RoleSDK && binding.Role != gateway.RoleDaemon && binding.Role != gateway.RoleEither)) {
		err = fmt.Errorf("invalid authenticated connection")
	}
	return binding, handler, err
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.ctx.Err() != nil {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	binding, handler, err := h.authenticate(r.Header.Get("Authorization"))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	select {
	case h.slots <- struct{}{}:
	default:
		http.Error(w, "session limit", http.StatusServiceUnavailable)
		return
	}
	defer func() { <-h.slots }()
	upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	_ = h.gateway.ServeConn(h.ctx, ws.NetConn(conn), binding, handler)
}

// ServeFastHTTP is the same tunnel admission for an application using fasthttp.
// The caller mounts this handler at the deployment's complete tunnel path.
func (h *Handler) ServeFastHTTP(ctx *fasthttp.RequestCtx) {
	if h.ctx.Err() != nil {
		ctx.Error("shutting down", 503)
		return
	}
	binding, handler, err := h.authenticate(string(ctx.Request.Header.Peek("Authorization")))
	if err != nil {
		ctx.Error("unauthorized", 401)
		return
	}
	select {
	case h.slots <- struct{}{}:
	default:
		ctx.Error("session limit", 503)
		return
	}
	upgrader := websocket.FastHTTPUpgrader{CheckOrigin: func(c *fasthttp.RequestCtx) bool { return len(c.Request.Header.Peek("Origin")) == 0 }}
	err = upgrader.Upgrade(ctx, func(conn *websocket.Conn) {
		defer func() { <-h.slots }()
		_ = h.gateway.ServeConn(h.ctx, ws.NetConn(conn), binding, handler)
	})
	if err != nil {
		<-h.slots
	}
}
