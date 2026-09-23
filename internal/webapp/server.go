package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	duneclient "github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/gateway"
	publicidentity "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/managed"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/transport/tunnel"
	"github.com/aiomni/dune/pkg/upgrade"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/fasthttp/websocket"
)

const cookieName = "dune_session"

func (s *Server) setSession(w http.ResponseWriter, token string, lifetime time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, HttpOnly: true, Secure: strings.HasPrefix(s.urls.PublicURL, "https://"), SameSite: http.SameSiteStrictMode, Path: s.urls.CookiePath, MaxAge: int(lifetime.Seconds())})
}

type Options struct {
	RunnerUpgrades         upgrade.Service
	AgentNativeSessions    agents.NativeSessions
	AgentLauncher          agents.Launcher
	AgentDirectory         agents.Directory
	AgentDirectoryObserver agents.DirectoryObserver
	AgentMessenger         agents.Messenger
	// TrustedProxies contains CIDRs allowed to supply X-Forwarded-For.
	TrustedProxies []string
	Binaries       string
	Assets         string
	PublicURL      string
	GatewayURL     string
	// DialGateway establishes the authenticated byte connection; the server
	// owns the returned connection and performs the execution protocol handshake.
	DialGateway func(context.Context, string) (net.Conn, error)
	// Online optionally reads shared directory facts for already authorized IDs.
	Online          func(context.Context, []string) (map[string]bool, error)
	Managed         managed.Service
	DisableAttached bool
	TenantScoped    bool
	// ConsumeWebSocketTicket exchanges a one-time browser subprotocol ticket
	// for the session token used by the normal Dune identity/access path.
	ConsumeWebSocketTicket func(context.Context, string, string, runner.Binding, api.Runtime) (string, error)
}

type authRate struct {
	Count int
	Until time.Time
}

type Server struct {
	trustedProxies []netip.Prefix
	store          *metadata.Store
	identity       publicidentity.Service
	access         *authorization.Service
	urls           deployment.URLs
	gateway        *gateway.Gateway
	options        Options
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	rates          map[string]authRate
	hashSlots      chan struct{}
	mux            *http.ServeMux
}

func NewServer(parent context.Context, options Options, store *metadata.Store, service publicidentity.Service, authorizer *authorization.Service, core *gateway.Gateway) (*Server, error) {
	if options.DialGateway == nil || service == nil || authorizer == nil || core == nil {
		return nil, fmt.Errorf("Gateway, dialer, identity and access modules required")
	}
	addresses, err := deployment.NewURLs(options.PublicURL, options.GatewayURL)
	if err != nil {
		return nil, err
	}
	options.PublicURL = addresses.PublicURL
	ctx, cancel := context.WithCancel(parent)
	s := &Server{urls: addresses, store: store, identity: service, access: authorizer, options: options, ctx: ctx, cancel: cancel, rates: map[string]authRate{}, hashSlots: make(chan struct{}, 4), mux: http.NewServeMux()}
	for _, cidr := range options.TrustedProxies {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", cidr, err)
		}
		s.trustedProxies = append(s.trustedProxies, prefix)
	}
	s.gateway = core
	s.mux.Handle("GET /api/v1/ws/tunnel", tunnel.NewHandler(ctx, s.gateway, func(credential string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		binding, handler, err := authorizer.Authorize(credential)
		return binding, handler, err
	}))
	s.mux.HandleFunc("GET /api/v1/bootstrap", s.bootstrap)
	if _, ok := service.(publicidentity.PasswordService); ok {
		s.mux.HandleFunc("POST /api/v1/auth/register", s.register)
		s.mux.HandleFunc("POST /api/v1/auth/login", s.login)
	}
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.logout)
	s.mux.HandleFunc("GET /api/v1/me", s.me)
	if options.TenantScoped {
		s.projectRoutes("/api/v1/tenants/{tenant}")
		s.viewRoutes("/api/v1/tenants/{tenant}")
		s.agentDirectoryRoutes("/api/v1/tenants/{tenant}")
	} else {
		s.projectRoutes("/api/v1")
		s.viewRoutes("/api/v1")
		s.agentDirectoryRoutes("/api/v1")
	}
	if !options.TenantScoped {
		s.mux.HandleFunc("GET /api/v1/profiles", s.listProfiles)
		s.mux.HandleFunc("GET /api/v1/profiles/{profile}", s.getProfile)
		s.mux.HandleFunc("POST /api/v1/profiles", s.saveProfile)
		s.mux.HandleFunc("PUT /api/v1/profiles/{profile}", s.saveProfile)
		s.mux.HandleFunc("DELETE /api/v1/profiles/{profile}", s.deleteProfile)
	}
	s.mux.HandleFunc("GET /api/v1/runners", s.runners)
	s.mux.HandleFunc("GET /api/v1/runners/{runner}", s.runner)
	if options.Managed != nil {
		s.mux.HandleFunc("GET /api/v1/managed/tenants/{tenant}/templates", s.managedTemplates)
		s.mux.HandleFunc("GET /api/v1/managed/tenants/{tenant}/templates/{fabric}/{template}/{version}", s.managedTemplate)
		s.mux.HandleFunc("POST /api/v1/managed/tenants/{tenant}/runners", s.createManagedRunner)
		s.mux.HandleFunc("DELETE /api/v1/managed/runners/{runner}", s.destroyManagedRunner)
		s.mux.HandleFunc("POST /api/v1/managed/runners/{runner}/pause", s.pauseManagedRunner)
		s.mux.HandleFunc("POST /api/v1/managed/runners/{runner}/resume", s.resumeManagedRunner)
		s.mux.HandleFunc("GET /api/v1/managed/runners/{runner}", s.managedRunnerStatus)
		s.mux.HandleFunc("GET /api/v1/managed/operations/{operation}", s.managedOperation)
	}
	s.mux.HandleFunc("GET /api/v1/downloads/{binary}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("binary")
		switch name {
		case "dune-linux-amd64.tar.gz", "dune-linux-arm64.tar.gz", "dune-darwin-amd64.tar.gz", "dune-darwin-arm64.tar.gz", "dune-linux-amd64.tar.gz.sha256", "dune-linux-arm64.tar.gz.sha256", "dune-darwin-amd64.tar.gz.sha256", "dune-darwin-arm64.tar.gz.sha256":
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
	s.mux.HandleFunc("GET /api/v1/install.sh", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, installScript)
	})
	if !options.DisableAttached && !options.TenantScoped {
		s.mux.HandleFunc("POST /api/v1/enrollments", s.enrollment)
		s.mux.HandleFunc("DELETE /api/v1/enrollments/{runner}", s.cancelEnrollment)
	}
	if !options.DisableAttached && options.TenantScoped {
		s.mux.HandleFunc("POST /api/v1/tenants/{tenant}/enrollments", s.tenantEnrollment)
		s.mux.HandleFunc("DELETE /api/v1/tenants/{tenant}/runners/{runner}/binding", s.detachTenantRunner)
	}
	if options.TenantScoped {
		s.mux.HandleFunc("GET /api/v1/tenants/{tenant}/runners", s.tenantRunners)
	}
	s.mux.HandleFunc("POST /api/v1/enroll", s.enroll)
	s.mux.HandleFunc("DELETE /api/v1/runners/{runner}/binding", s.revokeRunner)
	s.mux.HandleFunc("POST /api/v1/runners/{runner}/call", s.call)
	s.mux.HandleFunc("POST /api/v1/runners/{runner}/sessions", s.start)
	s.mux.HandleFunc("POST /api/v1/runners/{runner}/upgrade/{action}", s.runnerUpgrade)
	s.mux.HandleFunc("GET /api/v1/ws/runners/{runner}/sessions/{runtime}/events", s.events)
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
	if strings.HasPrefix(r.URL.Path, "/api/v1/") {
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

const defaultJSONBodyBytes = 256 * 1024

func readJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	return readJSONWithLimit(w, r, value, defaultJSONBodyBytes)
}

func readJSONWithLimit(w http.ResponseWriter, r *http.Request, value any, maxBytes int64) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, 415, "CONTENT_TYPE", "application/json required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
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

func (s *Server) user(w http.ResponseWriter, r *http.Request) (publicidentity.User, string, bool) {
	token := ""
	if value := strings.TrimSpace(r.Header.Get("X-Jwt-Token")); value != "" {
		token = value
	} else if value := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(value, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	} else if cookie, err := r.Cookie(cookieName); err == nil {
		token = cookie.Value
	}
	if token != "" {
		if authenticated, err := s.identity.Authenticate(r.Context(), token); err == nil && authenticated.Valid() && r.Context().Err() == nil {
			return authenticated.User, token, true
		} else if err != nil && !errors.Is(err, publicidentity.ErrUnauthorized) {
			writeError(w, http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", "identity service unavailable")
			return publicidentity.User{}, "", false
		}
	}
	writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "please sign in")
	return publicidentity.User{}, "", false
}

func (s *Server) authAllowed(w http.ResponseWriter, r *http.Request, email string) bool {
	account := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email)))))
	return s.authAllowedKeys(w, []string{"login:source:" + s.clientIP(r), "login:account:" + account}, 20)
}
func (s *Server) authAllowedKeys(w http.ResponseWriter, keys []string, limit int) bool {
	now := time.Now()
	s.mu.Lock()
	if len(s.rates) >= 10000 {
		for key, old := range s.rates {
			if !now.Before(old.Until) {
				delete(s.rates, key)
			}
		}
	}
	allow := true
	for _, key := range keys {
		rate, exists := s.rates[key]
		if !now.Before(rate.Until) {
			rate = authRate{Until: now.Add(time.Minute)}
		}
		if rate.Count >= limit || (!exists && len(s.rates) >= 10000) {
			allow = false
			break
		}
	}
	if allow {
		for _, key := range keys {
			rate := s.rates[key]
			if !now.Before(rate.Until) {
				rate = authRate{Until: now.Add(time.Minute)}
			}
			rate.Count++
			s.rates[key] = rate
		}
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
	password, ok := s.identity.(publicidentity.PasswordService)
	if !ok {
		writeError(w, 403, "LOCAL_LOGIN_DISABLED", "此站点使用企业登录。")
		return
	}
	var request struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if !s.authAllowed(w, r, request.Email) {
		return
	}
	defer func() { <-s.hashSlots }()
	var user publicidentity.User
	var token string
	var err error
	if register {
		user, token, err = password.Register(r.Context(), request.Email, request.Password)
	} else {
		user, token, err = password.Login(r.Context(), request.Email, request.Password)
	}
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	s.setSession(w, token, identity.SessionLifetime)
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
	if err := s.identity.Logout(r.Context(), token); err != nil {
		writeMetadataError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: s.urls.CookiePath, HttpOnly: true, Secure: strings.HasPrefix(s.options.PublicURL, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) enrollment(w http.ResponseWriter, r *http.Request) {
	_, cookie, ok := s.user(w, r)
	if !ok {
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	logical, token, expires, err := s.access.IssueEnrollment(r.Context(), cookie, request.Name)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	endpoint := strings.TrimSuffix(s.urls.PublicURL, "/")
	writeJSON(w, 200, map[string]any{"runner": logical, "token": token, "expires_at": expires, "endpoint": endpoint,
		"command": installCommand(endpoint, token, logical.ID)})
}

func (s *Server) tenantEnrollment(w http.ResponseWriter, r *http.Request) {
	_, cookie, ok := s.user(w, r)
	if !ok {
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	logical, token, expires, err := s.access.IssueTenantEnrollment(r.Context(), cookie, r.PathValue("tenant"), request.Name)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	endpoint := strings.TrimSuffix(s.urls.PublicURL, "/")
	writeJSON(w, http.StatusCreated, map[string]any{"runner": logical, "token": token, "expires_at": expires, "endpoint": endpoint,
		"command": installCommand(endpoint, token, logical.ID)})
}

func (s *Server) tenantRunners(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	query, ok := pageQuery(w, r)
	if !ok {
		return
	}
	page, err := s.access.DiscoverTenant(r.Context(), user, r.PathValue("tenant"), query)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	online, err := s.online(r.Context(), page.Items)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	out := struct {
		Items      []runnerView `json:"items"`
		NextCursor string       `json:"next_cursor,omitempty"`
	}{Items: []runnerView{}, NextCursor: page.NextCursor}
	for _, resource := range page.Items {
		out.Items = append(out.Items, runnerResponse(resource, online))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) detachTenantRunner(w http.ResponseWriter, r *http.Request) {
	_, cookie, ok := s.user(w, r)
	if !ok {
		return
	}
	binding, ok := selectedBinding(w, r)
	if !ok {
		return
	}
	if err := s.access.RevokeTenantRunner(r.Context(), cookie, r.PathValue("tenant"), binding); err != nil {
		writeMetadataError(w, err)
		return
	}
	s.gateway.Disconnect(binding.MachineID)
	w.WriteHeader(http.StatusNoContent)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token string `json:"token"`
		OS    string `json:"os"`
		Arch  string `json:"arch"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	decision, err := s.access.EnrollmentDecision(r.Context(), request.Token)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), decision.ValidUntil)
	defer cancel()
	machine, credential, err := s.store.Enroll(ctx, request.Token, request.OS, request.Arch)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"machine": machine, "credential": credential, "gateway": s.urls.GatewayURL})
}

func (s *Server) executionClient(w http.ResponseWriter, r *http.Request) (*duneclient.Client, bool) {
	_, cookie, ok := s.user(w, r)
	if !ok {
		return nil, false
	}
	binding, ok := selectedBinding(w, r)
	if !ok {
		return nil, false
	}
	grant, err := s.access.ClientRunner(r.Context(), cookie, binding)
	if err != nil {
		writeMetadataError(w, err)
		return nil, false
	}
	defer grant.Close()
	conn, err := s.options.DialGateway(r.Context(), grant.Token())
	if err != nil {
		writeError(w, 503, "OFFLINE", err.Error())
		return nil, false
	}
	client, err := duneclient.Connect(r.Context(), conn, binding.MachineID)
	if err != nil {
		writeError(w, 503, "OFFLINE", err.Error())
		return nil, false
	}
	return client, true
}

func (s *Server) call(w http.ResponseWriter, r *http.Request) {
	client, ok := s.executionClient(w, r)
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
	status := http.StatusUnprocessableEntity
	var ae *api.Error
	if errors.As(err, &ae) {
		code = ae.Code
		if ae.Code == "FILE_CHANGED" || ae.Code == "OFFSET_CONFLICT" {
			status = http.StatusConflict
		}
	}
	writeError(w, status, code, err.Error())
}

type browserEvent struct {
	ControlEpoch uint64          `json:"control_epoch"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Data         string          `json:"data,omitempty"`
	Binary       bool            `json:"binary,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	Code         string          `json:"code,omitempty"`
	Error        string          `json:"error,omitempty"`
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.urls.Origin {
		writeError(w, 403, "ORIGIN", "same-origin browser required")
		return
	}
	selected, ok := selectedRuntime(w, r)
	if !ok {
		return
	}
	webSocketProtocol := ""
	purpose := ""
	if s.options.ConsumeWebSocketTicket != nil {
		binding, bindingOK := selectedBinding(w, r)
		if !bindingOK {
			return
		}
		purpose = r.URL.Query().Get("purpose")
		if (purpose != "terminal" && purpose != "acp") || len(r.URL.Query()["purpose"]) != 1 {
			writeError(w, http.StatusBadRequest, "INVALID_PURPOSE", "terminal or acp WebSocket purpose is required")
			return
		}
		var ticket string
		webSocketProtocol, ticket, ok = browserTicketProtocol(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "WEBSOCKET_TICKET_REQUIRED", "a one-time WebSocket ticket is required")
			return
		}
		token, err := s.options.ConsumeWebSocketTicket(r.Context(), ticket, purpose, binding, selected)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "INVALID_WEBSOCKET_TICKET", "the WebSocket ticket is invalid, expired, used, or outside its scope")
			return
		}
		request := r.Clone(r.Context())
		request.Header = r.Header.Clone()
		request.Header.Set("X-Jwt-Token", token)
		r = request
	}
	client, ok := s.executionClient(w, r)
	if !ok {
		return
	}
	defer client.Close()
	lookupCtx, cancelLookup := context.WithTimeout(r.Context(), 5*time.Second)
	runtime, err := client.Get(lookupCtx, selected)
	cancelLookup()
	if err != nil {
		operationError(w, err)
		return
	}
	if !sameRuntime(runtime, selected) || (runtime.Adapter != "pty" && runtime.Adapter != "acp") {
		writeError(w, 409, "STALE_RUNTIME", "Runtime identity changed; select the current session")
		return
	}
	if (purpose == "terminal" && runtime.Adapter != "pty") || (purpose == "acp" && runtime.Adapter != "acp") {
		writeError(w, http.StatusConflict, "RUNTIME_PURPOSE_MISMATCH", "Runtime adapter does not match the ticket purpose")
		return
	}
	var stream *duneclient.Stream
	if runtime.Adapter == "pty" {
		stream, err = client.Attach(r.Context(), runtime, false)
	} else if r.URL.Query().Get("acp_events") == "conversation" {
		stream, err = client.SubscribeACPConversation(r.Context(), runtime)
	} else {
		stream, err = client.Attach(r.Context(), runtime, true)
	}
	if err != nil {
		operationError(w, err)
		return
	}
	defer stream.Close()
	u := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == s.urls.Origin }}
	if webSocketProtocol != "" {
		u.Subprotocols = []string{webSocketProtocol}
	}
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
	inputDone := make(chan error, 1)
	go func() {
		for {
			var input struct {
				Type         string `json:"type"`
				Data         string `json:"data"`
				Binary       bool   `json:"binary"`
				Rows         uint16 `json:"rows"`
				Cols         uint16 `json:"cols"`
				ControlEpoch uint64 `json:"control_epoch"`
				Action       string `json:"action"`
			}
			if err := wc.ReadJSON(&input); err != nil {
				inputDone <- err
				return
			}
			var err error
			message := &pb.Message{Kind: input.Type, RequestId: wire.ID(), ControlEpoch: input.ControlEpoch}
			switch input.Type {
			case "input":
				data := []byte(input.Data)
				if input.Binary {
					data, err = base64.StdEncoding.DecodeString(input.Data)
				}
				message.Data = data
			case "resize":
				message.Payload = api.Payload(api.Resize{Rows: input.Rows, Cols: input.Cols})
			case "signal":
				message.Data = []byte(input.Data)
			case "control", "history":
				message.Payload = api.Payload(api.TerminalControl{Action: input.Action})
			default:
				err = fmt.Errorf("invalid browser input")
			}
			// Preserve the browser's epoch. Replacing it with the SDK's latest
			// epoch would grant delayed input authority after a takeover.
			if err == nil {
				err = stream.Send(message)
			}
			if err != nil {
				inputDone <- err
				return
			}
		}
	}()
	type streamRead struct {
		message *pb.Message
		err     error
	}
	// Keep read errors behind all preceding messages, including exit and any
	// retained PTY history, so EOF cannot close the browser before they arrive.
	frames := make(chan streamRead, 8)
	go func() {
		for {
			m, err := stream.Recv()
			select {
			case frames <- streamRead{message: m, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil || (m.Kind == "exit" && runtime.Adapter != "pty") {
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
	exited := false
	for {
		select {
		case frame := <-frames:
			if frame.err != nil {
				if exited && errors.Is(frame.err, io.EOF) {
					_ = wc.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(5*time.Second))
				} else {
					_ = write(browserEvent{Type: "error", Code: "STREAM_INTERRUPTED", Error: frame.err.Error()})
				}
				return
			}
			m := frame.message
			event := browserEvent{Type: m.Kind, Payload: m.Payload, Data: string(m.Data), RequestID: m.RequestId, Code: m.Code, Error: m.Detail, ControlEpoch: m.ControlEpoch}
			if runtime.Adapter == "pty" && m.Kind == "data" {
				event.Binary = true
				event.Data = base64.StdEncoding.EncodeToString(m.Data)
			}
			if err := write(event); err != nil {
				return
			}
			if m.Kind == "exit" {
				exited = true
				if runtime.Adapter != "pty" {
					_ = wc.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(5*time.Second))
					return
				}
			}
		case err := <-inputDone:
			_ = write(browserEvent{Type: "error", Code: "STREAM_INTERRUPTED", Error: err.Error()})
			return
		case <-ping.C:
			if err := wc.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

const browserTicketProtocolPrefix = "sanddance.ticket."

func browserTicketProtocol(r *http.Request) (string, string, bool) {
	parts := strings.Split(strings.Join(r.Header.Values("Sec-WebSocket-Protocol"), ","), ",")
	if len(parts) != 1 {
		return "", "", false
	}
	protocol := strings.TrimSpace(parts[0])
	if !strings.HasPrefix(protocol, browserTicketProtocolPrefix) {
		return "", "", false
	}
	ticket := strings.TrimPrefix(protocol, browserTicketProtocolPrefix)
	if len(ticket) != 43 || strings.ContainsFunc(ticket, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_')
	}) {
		return "", "", false
	}
	return protocol, ticket, true
}
