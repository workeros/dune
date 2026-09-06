// Package host assembles the Dune workbench, local accounts, Attached machines,
// Gateway and default WebSocket transport. Applications own configuration and
// TLS termination. Protocol-only hosts can use pkg/gateway and pkg/fabricd directly.
package host

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/webapp"
	"github.com/aiomni/dune/pkg/deployment"
	externalidentity "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/transport/ws"
)

type Options struct {
	// DataDir selects SQLite in an absolute, private directory owned by the current user.
	// The application holds it exclusively until Close has completed. The on-disk
	// format is managed by Dune and is not a public storage extension interface.
	DataDir string
	// Database selects a SQL backend instead of the default SQLite DataDir.
	// It cannot be combined with DataDir. The application owns the connection pool.
	Database *storage.Config
	// Assets and Binaries contain the built workbench and installation archives.
	Assets, Binaries string
	// PublicURL is the browser HTTP(S) deployment directory. GatewayURL optionally
	// overrides the full machine WS(S) entry point without changing browser URLs.
	PublicURL, GatewayURL string
	DisableRegistration   bool
	// Identity selects a trusted external browser login provider instead of
	// local password login. Nil preserves the default local account behavior.
	Identity *externalidentity.Options
	// DialGateway optionally connects the workbench to this application's tunnel
	// using the supplied short-lived credential. The returned connection belongs
	// to Dune. It must honor context cancellation and must not replay requests.
	// By default, Dune dials PublicURL + "tunnel" using verified WS(S). A host can
	// provide a local network route or its TLS trust configuration here.
	DialGateway func(context.Context, string) (net.Conn, error)
}

// App is an http.Handler mounted with the complete deployment prefix preserved.
// It owns its metadata store, Gateway, accepted Dune requests and subscriptions.
// It never closes an HTTP server supplied by the caller; Serve is the optional
// convenience entry point for listeners whose ownership is transferred to App.
// HTTP middleware must preserve Hijacker and ResponseController support (directly
// or through Unwrap) for upgrades and cancellation of blocked request I/O.
type App struct {
	ctx     context.Context
	cancel  context.CancelFunc
	web     *webapp.Server
	store   *metadata.Store
	mu      sync.Mutex
	closed  bool
	servers map[*http.Server]struct{}
	active  sync.WaitGroup
	once    sync.Once
	done    chan struct{}
	err     error
}

// Open assembles an application without opening a listener. Cancelling parent
// closes the application; callers can wait for resource release through Done.
func Open(parent context.Context, options Options) (*App, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	addresses, err := deployment.NewURLs(options.PublicURL, options.GatewayURL)
	if err != nil {
		return nil, err
	}
	dial := options.DialGateway
	if dial == nil {
		endpoint := strings.Replace(addresses.PublicURL, "http", "ws", 1) + "tunnel"
		dial = func(ctx context.Context, token string) (net.Conn, error) {
			return ws.Dial(ctx, endpoint, token, &tls.Config{MinVersion: tls.VersionTLS12})
		}
	}
	database := storage.Config{SQLiteDir: options.DataDir}
	if options.Database != nil {
		if options.DataDir != "" {
			return nil, fmt.Errorf("DataDir and Database cannot be combined")
		}
		database = *options.Database
	}
	store, err := metadata.Open(parent, database)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	local := identity.NewLocal(store, !options.DisableRegistration && options.Identity == nil)
	var external *identity.External
	if options.Identity != nil {
		external, err = identity.NewExternal(store, *options.Identity)
		if err != nil {
			cancel()
			store.Close()
			return nil, err
		}
	}
	var service identity.Service = local
	if external != nil {
		service = external
	}
	owner := authorization.NewLocal(ctx, service, store)
	web, err := webapp.NewServer(ctx, webapp.Options{
		Assets: options.Assets, Binaries: options.Binaries,
		PublicURL: addresses.PublicURL, GatewayURL: addresses.GatewayURL,
		DialGateway: dial,
		External:    external,
	}, store, service, owner)
	if err != nil {
		cancel()
		store.Close()
		return nil, err
	}
	app := &App{ctx: ctx, cancel: cancel, web: web, store: store, servers: make(map[*http.Server]struct{}), done: make(chan struct{})}
	context.AfterFunc(ctx, func() { app.Close() })
	return app, nil
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if a.closed || a.ctx.Err() != nil {
		a.mu.Unlock()
		http.Error(w, "Dune is closed", http.StatusServiceUnavailable)
		return
	}
	a.active.Add(1)
	a.mu.Unlock()
	defer a.active.Done()
	ctx, cancel := context.WithCancel(r.Context())
	interrupted := make(chan struct{})
	stop := context.AfterFunc(a.ctx, func() {
		cancel()
		// Context cancellation alone does not unblock a partial HTTP body or
		// a stalled response on a caller-owned server. Limit this request's I/O
		// without shutting down unrelated routes on that server.
		controller := http.NewResponseController(w)
		controller.SetReadDeadline(time.Now())
		controller.SetWriteDeadline(time.Now())
		close(interrupted)
	})
	defer func() {
		if !stop() {
			<-interrupted
		}
	}()
	defer cancel()
	a.web.ServeHTTP(w, r.WithContext(ctx))
}

// Serve takes ownership of listener, including on failure. Multiple listeners
// may be served concurrently. Close stops all of them; Serve then returns
// http.ErrServerClosed. TLS listeners should be configured by the caller.
func (a *App) Serve(listener net.Listener) error {
	srv := &http.Server{Handler: a, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	a.mu.Lock()
	if a.closed || a.ctx.Err() != nil {
		a.mu.Unlock()
		listener.Close()
		return http.ErrServerClosed
	}
	a.servers[srv] = struct{}{}
	a.mu.Unlock()
	defer func() {
		srv.Close()
		listener.Close()
		a.mu.Lock()
		delete(a.servers, srv)
		a.mu.Unlock()
	}()
	return srv.Serve(listener)
}

// Close is idempotent. It stops admission, cancels connections and requests,
// closes owned HTTP servers, waits for accepted handlers, then releases storage.
// It preserves remote tmux sessions and never deletes machine work contents.
// Do not call Close from within an active Dune request or DialGateway callback.
func (a *App) Close() error {
	a.once.Do(func() {
		a.mu.Lock()
		a.closed = true
		servers := make([]*http.Server, 0, len(a.servers))
		for srv := range a.servers {
			servers = append(servers, srv)
		}
		a.mu.Unlock()
		a.cancel()
		a.web.Close()
		for _, srv := range servers {
			srv.Close()
		}
		a.active.Wait()
		a.err = a.store.Close()
		close(a.done)
	})
	return a.err
}

// Done is closed after all application-owned resources have been released.
func (a *App) Done() <-chan struct{} { return a.done }

// SetPrincipalEnabled lets a trusted host administrator suspend or re-enable a
// Dune principal. The caller must authorize its administrator; this is not a
// user HTTP endpoint. Suspension revokes sessions, pending enrollments and user access,
// while preserving machines and their running work. Re-enabling requires a new
// login. Close cancels and waits for this operation as for accepted HTTP work.
func (a *App) SetPrincipalEnabled(ctx context.Context, principalID string, enabled bool) error {
	a.mu.Lock()
	if a.closed || a.ctx.Err() != nil {
		a.mu.Unlock()
		return fmt.Errorf("Dune is closed")
	}
	a.active.Add(1)
	a.mu.Unlock()
	defer a.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(a.ctx, cancel)
	defer stop()
	return a.store.SetPrincipalEnabled(ctx, principalID, enabled)
}
