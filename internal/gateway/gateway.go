// Package gateway terminates two independent Yamux sessions and relays bounded
// application messages. It never executes commands or retries requests.
package gateway

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/transport/ws"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/fasthttp/websocket"
	"github.com/hashicorp/yamux"
	"github.com/valyala/fasthttp"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type route struct {
	s *yamux.Session
	b api.Binding
}

// Grant is issued by the account layer, not by request payloads. A machine
// credential grants only the daemon role; browser service calls use SDK grants.
type Grant struct {
	Target string
	Role   string
	Valid  func() bool
}
type Authorize func(token string) (Grant, bool)

func (a Grant) valid() bool { return a.Valid == nil || a.Valid() }

type Gateway struct {
	c         config.Config
	mu        sync.Mutex
	routes    map[string]*route
	sessions  map[*yamux.Session]Grant
	slots     chan struct{}
	streams   chan struct{}
	authorize Authorize
}

func New(authorize Authorize) *Gateway {
	return &Gateway{authorize: authorize, routes: map[string]*route{}, sessions: map[*yamux.Session]Grant{}, slots: make(chan struct{}, 256), streams: make(chan struct{}, 512)}
}

func (g *Gateway) Online(target string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.routes[target]
	return r != nil && !r.s.IsClosed()
}

func (g *Gateway) Disconnect(target string) {
	g.mu.Lock()
	var closeSessions []*yamux.Session
	for sess, grant := range g.sessions {
		if grant.Target == target {
			closeSessions = append(closeSessions, sess)
		}
	}
	g.mu.Unlock()
	for _, sess := range closeSessions {
		sess.Close()
	}
}

func (g *Gateway) Close() {
	g.mu.Lock()
	var closeSessions []*yamux.Session
	for sess := range g.sessions {
		closeSessions = append(closeSessions, sess)
	}
	g.mu.Unlock()
	for _, sess := range closeSessions {
		sess.Close()
	}
}

// ServeHTTP is the hosted account layer's tunnel endpoint. Browser WebSockets
// use a separate authenticated same-origin adapter, never machine credentials.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/tunnel" {
		http.NotFound(w, r)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || g.authorize == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	grant, ok := g.authorize(token)
	if !ok || grant.Target == "" || (grant.Role != "sdk" && grant.Role != "daemon") || !grant.valid() {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	select {
	case g.slots <- struct{}{}:
	default:
		http.Error(w, "session limit", http.StatusServiceUnavailable)
		return
	}
	defer func() { <-g.slots }()
	u := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	wc, err := u.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	g.session(wc, grant)
}

func Run(ctx context.Context, c config.Config) error {
	if e := c.ValidateServer(); e != nil {
		return e
	}
	ln, e := net.Listen("tcp", c.Listen)
	if e != nil {
		return e
	}
	if strings.HasPrefix(c.Gateway, "wss://") {
		cert, e := tls.LoadX509KeyPair(c.Certificate, c.Key)
		if e != nil {
			ln.Close()
			return e
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	g := New(nil)
	g.c = c
	g.slots = make(chan struct{}, 16)
	g.streams = make(chan struct{}, 128)
	srv := &fasthttp.Server{Handler: g.handle, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxRequestBodySize: 1024, Concurrency: 64}
	go func() {
		<-ctx.Done()
		ln.Close()
		g.Close()
	}()
	log.Printf("gateway listening %s", c.Listen)
	e = srv.Serve(ln)
	if ctx.Err() != nil {
		return nil
	}
	return e
}
func (g *Gateway) handle(ctx *fasthttp.RequestCtx) {
	if string(ctx.Path()) != "/tunnel" {
		ctx.Error("not found", 404)
		return
	}
	want := "Bearer " + g.c.Token
	if subtle.ConstantTimeCompare(ctx.Request.Header.Peek("Authorization"), []byte(want)) != 1 {
		ctx.Error("unauthorized", 401)
		return
	}
	select {
	case g.slots <- struct{}{}:
	default:
		ctx.Error("session limit", 503)
		return
	}
	u := websocket.FastHTTPUpgrader{CheckOrigin: func(c *fasthttp.RequestCtx) bool { return len(c.Request.Header.Peek("Origin")) == 0 }}
	e := u.Upgrade(ctx, func(w *websocket.Conn) { defer func() { <-g.slots }(); g.session(w, Grant{Target: g.c.Target}) })
	if e != nil {
		<-g.slots
	}
}
func (g *Gateway) session(w *websocket.Conn, grant Grant) {
	defer w.Close()
	s, e := yamux.Server(ws.NetConn(w), wire.Config())
	if e != nil {
		return
	}
	defer s.Close()
	g.mu.Lock()
	g.sessions[s] = grant
	g.mu.Unlock()
	defer func() { g.mu.Lock(); delete(g.sessions, s); g.mu.Unlock() }()
	timer := time.AfterFunc(5*time.Second, func() { s.Close() })
	raw, e := s.AcceptStream()
	if e != nil {
		return
	}
	st := wire.Wrap(raw)
	m, e := st.Recv()
	if e != nil {
		return
	}
	var h api.Hello
	if wire.Decode(m, &h) != nil || m.Kind != "hello" || h.Version != api.Version || m.Target != grant.Target || (grant.Role != "" && h.Role != grant.Role) || !grant.valid() {
		st.Fail("HANDSHAKE", fmt.Errorf("invalid version or target"))
		return
	}
	timer.Stop()
	defer st.Close()
	if h.Role == "daemon" {
		if m.Incarnation == "" || m.ConnectionGeneration == 0 {
			st.Fail("HANDSHAKE", fmt.Errorf("missing incarnation/generation"))
			return
		}
		var b api.Binding
		if len(m.Data) == 0 {
			return
		}
		if e := jsonBinding(m.Data, &b); e != nil {
			return
		}
		b.Target = grant.Target
		b.Incarnation = m.Incarnation
		b.Generation = m.ConnectionGeneration
		b.Version = api.Version
		r := &route{s: s, b: b}
		g.mu.Lock()
		old := g.routes[grant.Target]
		g.routes[grant.Target] = r
		g.mu.Unlock()
		if old != nil {
			old.s.Close()
		}
		defer func() {
			g.mu.Lock()
			if g.routes[grant.Target] == r {
				delete(g.routes, grant.Target)
			}
			g.mu.Unlock()
		}()
		if st.Send(&pb.Message{Kind: "welcome", Payload: api.Payload(b)}) != nil {
			return
		}
		log.Printf("fabricd registered incarnation=%s generation=%d", b.Incarnation, b.Generation)
		go func() {
			for {
				extra, e := s.AcceptStream()
				if e != nil {
					return
				}
				extra.Close()
			}
		}()
		_, _ = st.Recv()
		return
	}
	if h.Role != "sdk" {
		st.Fail("HANDSHAKE", fmt.Errorf("invalid role"))
		return
	}
	g.mu.Lock()
	r := g.routes[grant.Target]
	g.mu.Unlock()
	if r == nil || r.s.IsClosed() {
		st.Fail("OFFLINE", fmt.Errorf("fabricd offline"))
		return
	}
	if st.Send(&pb.Message{Kind: "welcome", Payload: api.Payload(r.b)}) != nil {
		return
	}
	go func() { _, _ = st.Recv(); s.Close() }()
	sem := make(chan struct{}, wire.MaxStreams)
	for {
		raw, e := s.AcceptStream()
		if e != nil {
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			raw.Close()
			continue
		}
		select {
		case g.streams <- struct{}{}:
		default:
			<-sem
			raw.Close()
			continue
		}
		go func() { defer func() { <-sem; <-g.streams }(); g.forward(wire.Wrap(raw), r, grant) }()
	}
}
func (g *Gateway) forward(c *wire.Stream, r *route, grant Grant) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, e := c.Recv()
	_ = c.SetReadDeadline(time.Time{})
	if e != nil {
		return
	}
	if !grant.valid() || m.Kind != "request" || m.RequestId == "" || m.Target != r.b.Target || m.Incarnation != r.b.Incarnation || m.ConnectionGeneration != r.b.Generation {
		c.Fail("STALE_BINDING", fmt.Errorf("invalid target or binding"))
		return
	}
	g.mu.Lock()
	current := g.routes[grant.Target]
	g.mu.Unlock()
	if current != r {
		c.Fail("STALE_BINDING", fmt.Errorf("reconnect SDK for current binding"))
		return
	}
	raw, e := r.s.OpenStream()
	if e != nil {
		c.Fail("OFFLINE", e)
		return
	}
	d := wire.Wrap(raw)
	defer d.Close()
	if e = d.Send(m); e != nil {
		c.Fail("RESULT_UNKNOWN", e)
		return
	}
	done := make(chan error, 2)
	relay := func(dst, src *wire.Stream) {
		for {
			m, e := src.Recv()
			if e == nil && !grant.valid() {
				e = fmt.Errorf("machine access revoked")
			}
			if e == nil {
				e = dst.Send(m)
			}
			if e != nil {
				done <- e
				return
			}
		}
	}
	go relay(d, c)
	go relay(c, d)
	e = <-done
	if e != io.EOF {
		c.Fail("STREAM_INTERRUPTED", e)
	}
	c.Close()
	d.Close()
	<-done
}
