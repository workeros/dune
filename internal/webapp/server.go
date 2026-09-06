package webapp

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/sdk"
	"github.com/aiomni/dune/pkg/transport/tunnel"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/fasthttp/websocket"
)

const cookieName = "dune_session"

type Options struct {
	DataDir    string
	Binaries   string
	Assets     string
	PublicURL  string
	GatewayURL string
	// Optional extra loopback-only HTTP listener for local browser validation
	// while remote machines connect through the primary verified TLS endpoint.
	WebListen string
}

type authRate struct {
	Count int
	Until time.Time
}

type Server struct {
	store     *Store
	urls      deployment.URLs
	gateway   *gateway.Gateway
	config    config.Config
	options   Options
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	tickets   map[string]access.Grant
	rates     map[string]authRate
	hashSlots chan struct{}
	mux       *http.ServeMux
}

func NewServer(parent context.Context, c config.Config, options Options, store *Store) (*Server, error) {
	addresses, err := deployment.NewURLs(options.PublicURL, options.GatewayURL)
	if err != nil {
		return nil, err
	}
	options.PublicURL = addresses.PublicURL
	ctx, cancel := context.WithCancel(parent)
	s := &Server{urls: addresses, store: store, config: c, options: options, ctx: ctx, cancel: cancel, tickets: map[string]access.Grant{}, rates: map[string]authRate{}, hashSlots: make(chan struct{}, 4), mux: http.NewServeMux()}
	s.gateway = gateway.New()
	s.mux.Handle("GET /tunnel", tunnel.NewHandler(ctx, s.gateway, func(token string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		grant, ok := s.authorize(token)
		if !ok {
			return gateway.BindingContext{}, nil, fmt.Errorf("unauthorized")
		}
		return grant.Bind()
	}))
	s.mux.HandleFunc("POST /api/auth/register", s.register)
	s.mux.HandleFunc("POST /api/auth/login", s.login)
	s.mux.HandleFunc("POST /api/auth/logout", s.logout)
	s.mux.HandleFunc("GET /api/me", s.me)
	s.mux.HandleFunc("GET /downloads/{binary}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("binary")
		switch name {
		case "dune-linux-amd64.tar.gz", "dune-linux-arm64.tar.gz", "dune-darwin-amd64.tar.gz", "dune-darwin-arm64.tar.gz":
		default:
			http.NotFound(w, r)
			return
		}
		if options.Binaries == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, filepath.Join(options.Binaries, name))
	})
	s.mux.HandleFunc("GET /api/machines", s.machines)
	s.mux.HandleFunc("POST /api/enrollments", s.enrollment)
	s.mux.HandleFunc("POST /api/enroll", s.enroll)
	s.mux.HandleFunc("DELETE /api/machines/{machine}", s.revoke)
	s.mux.HandleFunc("POST /api/machines/{machine}/call", s.call)
	s.mux.HandleFunc("POST /api/machines/{machine}/sessions", s.start)
	s.mux.HandleFunc("GET /api/machines/{machine}/sessions/{runtime}/events", s.events)
	if options.Assets != "" {
		s.mux.Handle("GET /", http.FileServer(http.Dir(options.Assets)))
	}
	return s, nil
}

func (s *Server) Close() { s.cancel(); s.gateway.Close() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// This entry point expects the deployment prefix to be preserved by the
	// proxy. Map it exactly once, before handing fixed relative routes to mux.
	if s.urls.Path != "/" && r.URL.Path == strings.TrimSuffix(s.urls.Path, "/") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		target := &url.URL{Path: s.urls.Path, RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, target.String(), http.StatusPermanentRedirect)
		return
	}
	clean := path.Clean(r.URL.Path)
	if strings.HasSuffix(r.URL.Path, "/") && clean != "/" {
		clean += "/"
	}
	if !strings.HasPrefix(r.URL.Path, s.urls.Path) || clean != r.URL.Path {
		http.NotFound(w, r)
		return
	}
	r = r.Clone(r.Context())
	r.URL.Path = "/" + strings.TrimPrefix(r.URL.Path, s.urls.Path)
	r.URL.RawPath = ""
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		origin := r.Header.Get("Origin")
		_, cookieErr := r.Cookie(cookieName)
		if r.Header.Get("X-Dune-Request") != "1" || (origin != "" && origin != s.urls.Origin) || (origin == "" && cookieErr == nil) {
			writeError(w, http.StatusForbidden, "ORIGIN", "same-origin Dune request required")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "error": message})
}
func readJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, 415, "CONTENT_TYPE", "application/json required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		writeError(w, 400, "INVALID_ARGUMENT", err.Error())
		return false
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		writeError(w, 400, "INVALID_ARGUMENT", "one JSON object required")
		return false
	}
	return true
}

func (s *Server) user(w http.ResponseWriter, r *http.Request) (User, string, bool) {
	cookie, err := r.Cookie(cookieName)
	if err == nil {
		if user, ok := s.store.Session(cookie.Value); ok {
			return user, cookie.Value, true
		}
	}
	writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "please sign in")
	return User{}, "", false
}

func (s *Server) authAllowed(w http.ResponseWriter, r *http.Request) bool {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	s.mu.Lock()
	rate := s.rates[ip]
	if !now.Before(rate.Until) {
		rate = authRate{Until: now.Add(time.Minute)}
	}
	if len(s.rates) >= 10000 {
		for key, old := range s.rates {
			if now.After(old.Until) {
				delete(s.rates, key)
			}
		}
	}
	allow := rate.Count < 20 && len(s.rates) < 10000
	rate.Count++
	if allow {
		s.rates[ip] = rate
	}
	s.mu.Unlock()
	if !allow {
		writeError(w, 429, "RATE_LIMIT", "too many authentication attempts; retry in a minute")
		return false
	}
	select {
	case s.hashSlots <- struct{}{}:
		return true
	default:
		writeError(w, 429, "RATE_LIMIT", "authentication busy; retry shortly")
		return false
	}
}

func (s *Server) auth(w http.ResponseWriter, r *http.Request, register bool) {
	if !s.authAllowed(w, r) {
		return
	}
	defer func() { <-s.hashSlots }()
	var request struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	var user User
	var token string
	var err error
	if register {
		user, token, err = s.store.Register(request.Email, request.Password)
	} else {
		user, token, err = s.store.Login(request.Email, request.Password)
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrUnauthorized) {
			status = http.StatusUnauthorized
		}
		writeError(w, status, "AUTH_FAILED", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, HttpOnly: true, Secure: strings.HasPrefix(s.options.PublicURL, "https://"), SameSite: http.SameSiteStrictMode, Path: s.urls.CookiePath, MaxAge: 7 * 24 * 3600})
	writeJSON(w, http.StatusOK, user)
}
func (s *Server) register(w http.ResponseWriter, r *http.Request) { s.auth(w, r, true) }
func (s *Server) login(w http.ResponseWriter, r *http.Request)    { s.auth(w, r, false) }
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if user, _, ok := s.user(w, r); ok {
		writeJSON(w, 200, user)
	}
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	_, token, ok := s.user(w, r)
	if !ok {
		return
	}
	if err := s.store.Logout(token); err != nil {
		writeError(w, 500, "STORE_FAILED", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: s.urls.CookiePath, HttpOnly: true, Secure: strings.HasPrefix(s.options.PublicURL, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) machines(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	type machineView struct {
		Machine
		Online bool `json:"online"`
	}
	out := []machineView{}
	for _, machine := range s.store.Machines(user.ID) {
		out = append(out, machineView{machine, s.gateway.Online(machine.ID)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	writeJSON(w, 200, out)
}

func (s *Server) enrollment(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	token, expires, err := s.store.IssueEnrollment(user.ID, request.Name)
	if err != nil {
		writeError(w, 400, "BINDING_FAILED", err.Error())
		return
	}
	endpoint := strings.TrimSuffix(s.urls.PublicURL, "/")
	writeJSON(w, 200, map[string]any{"token": token, "expires_at": expires, "endpoint": endpoint,
		"command": fmt.Sprintf("curl --fail --show-error --proto '=http,https' %s -o dune-install.sh && sh dune-install.sh %s %s", shellQuote(endpoint+"/install.sh"), shellQuote(endpoint), shellQuote(token))})
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	if !s.authAllowed(w, r) {
		return
	}
	defer func() { <-s.hashSlots }()
	var request struct {
		Token string `json:"token"`
		OS    string `json:"os"`
		Arch  string `json:"arch"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	machine, credential, err := s.store.Enroll(request.Token, request.OS, request.Arch)
	if err != nil {
		writeError(w, 400, "BINDING_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"machine": machine, "credential": credential, "gateway": s.urls.GatewayURL})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	machineID := r.PathValue("machine")
	if err := s.store.Revoke(user.ID, machineID); err != nil {
		writeError(w, 404, "NOT_FOUND", "machine not found")
		return
	}
	s.gateway.Disconnect(machineID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) authorize(token string) (access.Grant, bool) {
	s.mu.Lock()
	grant, ok := s.tickets[token]
	s.mu.Unlock()
	if ok {
		return grant, true
	}
	machineID, ok := s.store.MachineCredential(token)
	if !ok {
		return access.Grant{}, false
	}
	return access.Grant{Target: machineID, Role: "daemon", Valid: func() bool { id, ok := s.store.MachineCredential(token); return ok && id == machineID }}, true
}

func (s *Server) machineClient(w http.ResponseWriter, r *http.Request) (*sdk.Client, func() bool, bool) {
	user, cookie, ok := s.user(w, r)
	if !ok {
		return nil, nil, false
	}
	machineID := r.PathValue("machine")
	if !s.store.Owns(user.ID, machineID) {
		writeError(w, 404, "NOT_FOUND", "machine not found")
		return nil, nil, false
	}
	valid := func() bool {
		u, ok := s.store.Session(cookie)
		return ok && u.ID == user.ID && s.store.Owns(user.ID, machineID)
	}
	ticket := randomToken()
	s.mu.Lock()
	s.tickets[ticket] = access.Grant{Target: machineID, Role: "sdk", Valid: valid}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.tickets, ticket); s.mu.Unlock() }()
	tc, err := s.config.TLS()
	if err != nil {
		writeError(w, 500, "TLS_FAILED", err.Error())
		return nil, nil, false
	}
	// Browser adapter and Gateway are one server. Use its local listener
	// instead of hairpinning through a changing VPN/public route. TLS still
	// verifies the advertised Gateway identity against the pinned certificate.
	localURL, _ := url.Parse(s.config.Gateway)
	localURL.Path = s.urls.Path + "tunnel"
	localURL.RawPath = ""
	endpoint := localURL.String()
	if host, port, e := net.SplitHostPort(s.config.Listen); e == nil {
		u, _ := url.Parse(endpoint)
		if tc != nil {
			tc.ServerName = u.Hostname()
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			if ip.To4() != nil {
				host = "127.0.0.1"
			} else {
				host = "::1"
			}
		}
		u.Host = net.JoinHostPort(host, port)
		endpoint = u.String()
	}
	client, err := sdk.Dial(r.Context(), sdk.Options{Gateway: endpoint, Token: ticket, Target: machineID, TLSConfig: tc})
	if err != nil {
		writeError(w, 503, "OFFLINE", err.Error())
		return nil, nil, false
	}
	return client, valid, true
}

func (s *Server) call(w http.ResponseWriter, r *http.Request) {
	client, _, ok := s.machineClient(w, r)
	if !ok {
		return
	}
	defer client.Close()
	var request struct {
		Operation string          `json:"operation"`
		Payload   json.RawMessage `json:"payload"`
		Runtime   *api.Runtime    `json:"runtime,omitempty"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	switch request.Operation {
	case "acp.action", "acp.state", "machine.info", "agent.config", "runtime.list", "runtime.get", "runtime.stop", "runtime.forget", "runtime.capture", "runtime.history":
	case "files":
		var f api.File
		if json.Unmarshal(request.Payload, &f) != nil || (f.Action != "list" && f.Action != "stat") {
			writeError(w, 400, "UNSUPPORTED", "workbench file API is directory listing/stat only")
			return
		}
	case "git":
		var g api.Git
		if json.Unmarshal(request.Payload, &g) != nil || (g.Action != "status" && g.Action != "diff") {
			writeError(w, 400, "UNSUPPORTED", "workbench Git API is read-only status/diff")
			return
		}
	default:
		writeError(w, 400, "UNSUPPORTED", "unsupported workbench operation")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var out json.RawMessage
	if err := client.CallID(ctx, request.Operation, wire.ID(), request.Payload, &out, request.Runtime); err != nil {
		operationError(w, err)
		return
	}
	if len(out) == 0 {
		out = json.RawMessage("null")
	}
	writeJSON(w, 200, out)
}

func operationError(w http.ResponseWriter, err error) {
	code := "OPERATION_FAILED"
	var ae *api.Error
	if errors.As(err, &ae) {
		code = ae.Code
	}
	writeError(w, 422, code, err.Error())
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	client, _, ok := s.machineClient(w, r)
	if !ok {
		return
	}
	defer client.Close()
	var profile api.Profile
	if !readJSON(w, r, &profile) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	profile.ManagedACP = profile.Adapter == "acp"
	runtime, stream, err := client.Start(ctx, profile)
	if err != nil {
		operationError(w, err)
		return
	}
	stream.Close()
	writeJSON(w, 201, runtime)
}

type browserEvent struct {
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Data      string          `json:"data,omitempty"`
	Binary    bool            `json:"binary,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Code      string          `json:"code,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.urls.Origin {
		writeError(w, 403, "ORIGIN", "same-origin browser required")
		return
	}
	client, valid, ok := s.machineClient(w, r)
	if !ok {
		return
	}
	defer client.Close()
	list, err := client.List(r.Context())
	if err != nil {
		operationError(w, err)
		return
	}
	var runtime api.Runtime
	for _, rt := range list {
		if rt.ID == r.PathValue("runtime") {
			runtime = rt
			break
		}
	}
	if runtime.ID == "" {
		writeError(w, 404, "NOT_FOUND", "session not found")
		return
	}
	var stream *sdk.Stream
	if runtime.Adapter == "pty" {
		stream, err = client.Attach(r.Context(), runtime, false)
	} else {
		stream, err = client.Attach(r.Context(), runtime, true)
	}
	if err != nil {
		operationError(w, err)
		return
	}
	defer stream.Close()
	u := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == s.urls.Origin }}
	wc, err := u.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer wc.Close()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	end := context.AfterFunc(ctx, func() { wc.Close(); stream.Close() })
	defer end()
	wc.SetReadLimit(64 * 1024)
	_ = wc.SetReadDeadline(time.Now().Add(45 * time.Second))
	wc.SetPongHandler(func(string) error { return wc.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	done := make(chan error, 2)
	go func() {
		for {
			var input struct {
				Type   string `json:"type"`
				Data   string `json:"data"`
				Binary bool   `json:"binary"`
				Rows   uint16 `json:"rows"`
				Cols   uint16 `json:"cols"`
			}
			if err := wc.ReadJSON(&input); err != nil {
				done <- err
				return
			}
			if !valid() {
				done <- ErrUnauthorized
				return
			}
			var err error
			switch input.Type {
			case "input":
				data := []byte(input.Data)
				if input.Binary {
					data, err = base64.StdEncoding.DecodeString(input.Data)
				}
				if err == nil {
					_, err = stream.Input(data)
				}
			case "resize":
				err = stream.Resize(input.Rows, input.Cols)
			case "signal":
				err = stream.Signal(input.Data)
			default:
				err = fmt.Errorf("invalid browser input")
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	frames := make(chan *pb.Message, 8)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			select {
			case frames <- m:
			case <-ctx.Done():
				return
			}
			if m.Kind == "exit" && runtime.Adapter != "pty" {
				return
			}
		}
	}()
	write := func(event browserEvent) error {
		_ = wc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return wc.WriteJSON(event)
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case m := <-frames:
			if !valid() {
				_ = write(browserEvent{Type: "error", Code: "UNAUTHORIZED", Error: "access revoked"})
				return
			}
			event := browserEvent{Type: m.Kind, Payload: m.Payload, Data: string(m.Data), RequestID: m.RequestId, Code: m.Code, Error: m.Detail}
			if runtime.Adapter == "pty" && m.Kind == "data" {
				event.Binary = true
				event.Data = base64.StdEncoding.EncodeToString(m.Data)
			}
			if err := write(event); err != nil {
				return
			}
			if m.Kind == "exit" && runtime.Adapter != "pty" {
				return
			}
		case err := <-done:
			_ = write(browserEvent{Type: "error", Code: "STREAM_INTERRUPTED", Error: err.Error()})
			return
		case <-ping.C:
			if !valid() {
				return
			}
			if err := wc.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func Run(ctx context.Context, c config.Config, options Options) error {
	if err := c.ValidateServer(); err != nil {
		return err
	}
	if options.PublicURL == "" {
		if !strings.HasSuffix(c.Gateway, "/tunnel") {
			return fmt.Errorf("public browser URL required when configured gateway uses a nonstandard connection path")
		}
		options.PublicURL = strings.Replace(strings.TrimSuffix(c.Gateway, "/tunnel"), "ws", "http", 1)
	}
	dataDir, err := filepath.Abs(options.DataDir)
	if err != nil {
		return err
	}
	store, err := OpenStore(dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	app, err := NewServer(ctx, c, options, store)
	if err != nil {
		return err
	}
	defer app.Close()
	primary, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer primary.Close()
	if strings.HasPrefix(c.Gateway, "wss://") {
		cert, err := tls.LoadX509KeyPair(c.Certificate, c.Key)
		if err != nil {
			return err
		}
		primary = tls.NewListener(primary, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	listeners := []net.Listener{primary}
	if options.WebListen != "" {
		host, _, err := net.SplitHostPort(options.WebListen)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return fmt.Errorf("extra web listener must bind a loopback IP")
		}
		ln, err := net.Listen("tcp", options.WebListen)
		if err != nil {
			return err
		}
		defer ln.Close()
		listeners = append(listeners, ln)
	}
	errors := make(chan error, len(listeners))
	servers := []*http.Server{}
	for _, ln := range listeners {
		srv := &http.Server{Handler: app, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
		servers = append(servers, srv)
		go func() { errors <- srv.Serve(ln) }()
	}
	fmt.Printf("Dune Web: %s\n", options.PublicURL)
	select {
	case <-ctx.Done():
	case err = <-errors:
	}
	app.Close()
	for _, srv := range servers {
		_ = srv.Close()
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}
