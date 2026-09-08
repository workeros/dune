// Package host assembles the Dune workbench, local accounts, Attached machines,
// Gateway and default WebSocket transport. Applications own configuration and
// TLS termination. Protocol-only hosts can use pkg/gateway and pkg/fabricd directly.
package host

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	managedmodule "github.com/aiomni/dune/internal/managed"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/webapp"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/gateway"
	externalidentity "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/observe"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/transport/peer"
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
	// Cluster enables shared connection ownership and authenticated peer routing.
	// All metadata, authorization and directory records use Database's PostgreSQL.
	Cluster *ClusterOptions
	// Assets and Binaries contain the built workbench and installation archives.
	Assets, Binaries string
	// PublicURL is the browser HTTP(S) deployment directory. GatewayURL optionally
	// overrides the full machine WS(S) entry point without changing browser URLs.
	PublicURL, GatewayURL string
	DisableRegistration   bool
	// Identity selects a trusted external browser login provider instead of
	// local password login. Nil preserves the default local account behavior.
	Identity *externalidentity.Options
	// AccessChecker selects enterprise policy instead of the default owner check.
	// It must honor cancellation and must not retain credentials or work content.
	AccessChecker access.Checker
	// Observer receives best-effort structured operational and audit events on
	// a bounded asynchronous dispatcher. The caller retains Sink ownership.
	Observer observe.Sink
	// Managed enables configured templates, the browser lifecycle API and the
	// durable provider worker. Nil leaves Managed unavailable.
	Managed *ManagedOptions
	// ConfigurationVersion identifies nonsecret custom identity, policy and
	// Managed provider behavior. PostgreSQL replicas with any such module must
	// use the same value and change it for incompatible settings.
	ConfigurationVersion string
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
	ctx           context.Context
	cancel        context.CancelFunc
	web           *webapp.Server
	core          *gateway.Gateway
	publicPath    string
	store         *metadata.Store
	peer          *peer.Transport
	peerHandler   http.Handler
	admission     *gateway.AdmissionLease
	observer      *observationRecorder
	managedStatus *managedStatusConfig
	mu            sync.Mutex
	closed        bool
	draining      bool
	requests      int
	requestsDone  chan struct{}
	managedDrain  chan struct{}
	managedDone   chan struct{}
	servers       map[*http.Server]struct{}
	active        sync.WaitGroup
	once          sync.Once
	done          chan struct{}
	err           error
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
	managedConfig, err := prepareManaged(options.Managed, addresses)
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
	transport, err := clusterTransport(database, options.Cluster)
	if err != nil {
		return nil, err
	}
	store, err := metadata.Open(parent, database)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	observer := newObservationRecorder(options.Observer)
	var core *gateway.Gateway
	var admission *gateway.AdmissionLease
	var instance metadata.InstanceConfig
	appInstanceID := wire.ID()
	assembled := false
	defer func() {
		if !assembled {
			cancel()
			observer.close()
			admission.Close()
			if core != nil {
				core.Close()
			}
			store.Close()
		}
	}()
	local := identity.NewLocal(store, !options.DisableRegistration && options.Identity == nil)
	var external *identity.External
	if options.Identity != nil {
		external, err = identity.NewExternal(store, *options.Identity)
		if err != nil {
			return nil, err
		}
	}
	var service identity.Service = local
	if external != nil {
		service = external
	}
	if database.Postgres != nil {
		managedFingerprint := ""
		if managedConfig != nil {
			managedFingerprint = managedConfig.fingerprint
		}
		admission, instance, err = registerInstance(ctx, store, options, addresses, service, managedFingerprint)
		if err != nil {
			return nil, err
		}
		appInstanceID = instance.BootID
	}
	core, err = openGateway(ctx, store, database, options.Cluster, transport)
	if err != nil {
		return nil, err
	}
	if options.Observer != nil {
		core.SetObserver(observer.emit)
	}
	authorizer := authorization.NewObserved(ctx, service, store, options.AccessChecker, accessObservation(observer))
	var managedService *managedmodule.Service
	var managedWorker *managedmodule.Worker
	if managedConfig != nil {
		availability, providers := observeManagedProviders(managedConfig.availability, managedConfig.providers, observer)
		managedService, err = managedmodule.New(managedConfig.catalog, availability, service, authorizer, store, appInstanceID)
		if err != nil {
			return nil, err
		}
		workerConfig := managedConfig.worker
		workerConfig.InstanceID, workerConfig.CloseTarget = appInstanceID, core.Disconnect
		managedWorker, err = managedmodule.NewWorker(store, providers, workerConfig)
		if err != nil {
			return nil, err
		}
	}
	var online func(context.Context, []string) (map[string]bool, error)
	var peerHandler http.Handler
	if transport != nil {
		recovery := options.Cluster.RecoveryGeneration
		online = func(ctx context.Context, ids []string) (map[string]bool, error) {
			return store.OnlineConnections(ctx, recovery, ids)
		}
		authorizer, err = authorizer.WithPeers(core.BootID())
		if err != nil {
			return nil, err
		}
		peerHandler, err = transport.Handler(ctx, core, peerAuthorization(authorizer, admission))
		if err != nil {
			return nil, err
		}
	}
	web, err := webapp.NewServer(ctx, webapp.Options{
		Assets: options.Assets, Binaries: options.Binaries,
		PublicURL: addresses.PublicURL, GatewayURL: addresses.GatewayURL,
		DialGateway: dial,
		External:    external,
		Online:      online,
		Admission:   admission,
		Managed:     managedService,
		DestroyAccessCloseTimeout: func() time.Duration {
			if managedConfig == nil {
				return 0
			}
			return managedConfig.destroyCloseTimeout
		}(),
	}, store, service, authorizer, core)
	if err != nil {
		return nil, err
	}
	var managedStatus *managedStatusConfig
	if managedConfig != nil {
		managedStatus = &managedStatusConfig{policyVersion: managedConfig.worker.RenewalPolicyVersion, expiryRiskWindow: managedConfig.worker.Renewal.RenewBefore}
	}
	app := &App{core: core, publicPath: addresses.Path, requestsDone: make(chan struct{}), managedDrain: make(chan struct{}), managedDone: make(chan struct{}), ctx: ctx, cancel: cancel, web: web, store: store, peer: transport, peerHandler: peerHandler, admission: admission, observer: observer, managedStatus: managedStatus, servers: make(map[*http.Server]struct{}), done: make(chan struct{})}
	assembled = true
	if admission != nil {
		app.active.Add(1)
		go app.renewAdmission(instance)
	}
	if managedWorker != nil {
		app.active.Add(1)
		go app.runManaged(managedWorker)
	} else {
		close(app.managedDone)
	}
	context.AfterFunc(ctx, func() { app.Close() })
	return app, nil
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.health(w, r) {
		return
	}
	a.serveHTTP(a.web, w, r, r.URL.Path == a.publicPath+"tunnel")
}

func (a *App) serveHTTP(handler http.Handler, w http.ResponseWriter, r *http.Request, connection bool) {
	a.mu.Lock()
	if a.closed || a.draining || a.ctx.Err() != nil || a.admission.Remaining() <= 0 {
		a.mu.Unlock()
		http.Error(w, "Dune is closed", http.StatusServiceUnavailable)
		return
	}
	a.active.Add(1)
	if !connection {
		a.requests++
	}
	a.mu.Unlock()
	defer func() {
		if !connection {
			a.finishRequest()
		}
		a.active.Done()
	}()
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
	handler.ServeHTTP(w, r.WithContext(ctx))
}

// Serve takes ownership of listener, including on failure. Multiple listeners
// may be served concurrently. Close stops all of them; Serve then returns
// http.ErrServerClosed. TLS listeners should be configured by the caller.
func (a *App) Serve(listener net.Listener) error {
	return a.serve(listener, a)
}

func (a *App) serve(listener net.Listener, handler http.Handler) error {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	a.mu.Lock()
	if a.closed || a.draining || a.ctx.Err() != nil || a.admission.Remaining() <= 0 {
		a.mu.Unlock()
		listener.Close()
		return http.ErrServerClosed
	}
	a.servers[srv] = struct{}{}
	a.mu.Unlock()
	defer func() {
		listener.Close()
		a.mu.Lock()
		draining := a.draining
		if !draining {
			delete(a.servers, srv)
		}
		a.mu.Unlock()
		// Shutdown closes listeners before flushing accepted responses. Keep
		// the server owned until Close, and do not interrupt that flush.
		if !draining {
			srv.Close()
		}
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
		a.beginDrainLocked()
		servers := make([]*http.Server, 0, len(a.servers))
		for srv := range a.servers {
			servers = append(servers, srv)
		}
		a.mu.Unlock()
		a.cancel()
		a.admission.Close()
		a.web.Close()
		for _, srv := range servers {
			srv.Close()
		}
		a.active.Wait()
		<-a.core.Drain()
		storeErr := a.store.Close()
		a.observer.close()
		a.mu.Lock()
		a.err = errors.Join(a.err, storeErr)
		a.mu.Unlock()
		close(a.done)
	})
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

func (a *App) runManaged(worker *managedmodule.Worker) {
	defer a.active.Done()
	defer close(a.managedDone)
	if err := worker.RunUntil(a.ctx, a.managedDrain); err != nil {
		a.mu.Lock()
		a.err = errors.Join(a.err, err)
		a.mu.Unlock()
		a.cancel()
	}
}

// Done is closed after all application-owned resources have been released.
func (a *App) Done() <-chan struct{} { return a.done }

// SetPrincipalEnabled lets a trusted host administrator suspend or re-enable a
// Dune principal. The caller must authorize its administrator; this is not a
// user HTTP endpoint. Suspension revokes sessions, pending enrollments and user access,
// while preserving machines and their running work. Re-enabling requires a new
// login. Close cancels and waits for this operation as for accepted HTTP work.
func (a *App) SetPrincipalEnabled(ctx context.Context, principalID string, enabled bool) error {
	ctx, finish, err := a.adminContext(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return a.store.SetPrincipalEnabled(ctx, principalID, enabled)
}

// LinkIdentity associates a verified external identity with an existing enabled
// principal. The caller must authenticate and authorize the administrator and
// verify both identities as documented by identity.LinkRequest. It is not a user
// HTTP endpoint. Success revokes existing sessions and pending enrollment grants,
// preserves machine/Runner ownership, and atomically records the decision.
// An identical RequestID and payload returns the original result without another
// revocation. After an uncertain commit, inspect IdentityLink before proceeding.
func (a *App) LinkIdentity(ctx context.Context, request externalidentity.LinkRequest) (externalidentity.LinkRecord, error) {
	ctx, finish, err := a.adminContext(ctx)
	if err != nil {
		return externalidentity.LinkRecord{}, err
	}
	defer finish()
	return a.store.LinkIdentity(ctx, request)
}

// IdentityLink reads one committed administrator decision by its request ID.
// The host must authorize access to this identity and audit information.
func (a *App) IdentityLink(ctx context.Context, requestID string) (externalidentity.LinkRecord, error) {
	ctx, finish, err := a.adminContext(ctx)
	if err != nil {
		return externalidentity.LinkRecord{}, err
	}
	defer finish()
	return a.store.IdentityLink(ctx, requestID)
}

func (a *App) adminContext(ctx context.Context) (context.Context, func(), error) {
	a.mu.Lock()
	if a.closed || a.draining || a.ctx.Err() != nil || a.admission.Remaining() <= 0 {
		a.mu.Unlock()
		return nil, nil, fmt.Errorf("Dune is closed")
	}
	a.active.Add(1)
	a.requests++
	a.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
		a.finishRequest()
		a.active.Done()
	}, nil
}
