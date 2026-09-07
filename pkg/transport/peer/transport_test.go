package peer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/testcert"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/fasthttp/websocket"
	"github.com/hashicorp/yamux"
)

type emptyDirectory struct{}

func (emptyDirectory) Resolve(context.Context, string) (gateway.RouteLease, error) {
	return gateway.RouteLease{}, gateway.ErrRouteNotFound
}
func (emptyDirectory) Acquire(context.Context, gateway.RouteClaim, uint64) (gateway.RouteLease, error) {
	return gateway.RouteLease{}, gateway.ErrRouteStale
}
func (emptyDirectory) Publish(context.Context, gateway.Route) error { return gateway.ErrRouteStale }
func (emptyDirectory) Renew(context.Context, gateway.Route) (gateway.RouteLease, error) {
	return gateway.RouteLease{}, gateway.ErrRouteStale
}
func (emptyDirectory) Release(context.Context, gateway.Route) error { return nil }

type denyUser struct{}

func (denyUser) Connected(context.Context, *gateway.Connection) error { return nil }
func (denyUser) Open(context.Context, *pb.Message, *gateway.Stream) (gateway.StreamHandler, error) {
	return nil, errors.New("no user grant")
}

type fixture struct {
	ctx               context.Context
	ca                *testcert.Authority
	server            *httptest.Server
	transport, client *Transport
	gateway           *gateway.Gateway
	route             gateway.Route
	source            string
	authorized        atomic.Int32
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	f := &fixture{ctx: ctx, ca: testcert.New(t), source: wire.ID()}
	f.server = httptest.NewUnstartedServer(nil)
	t.Cleanup(f.server.Close)
	var err error
	f.transport, err = New(Config{Address: "https://" + f.server.Listener.Addr().String() + "/private/peer", Certificate: f.ca.Issue(t, "127.0.0.1", nil), Roots: f.ca.Roots()})
	if err != nil {
		t.Fatal(err)
	}
	f.client, err = New(Config{Address: "https://127.0.0.1:1/private/peer", Certificate: f.ca.Issue(t, "127.0.0.1", nil), Roots: f.ca.Roots()})
	if err != nil {
		t.Fatal(err)
	}
	f.gateway, err = gateway.NewWithDirectory(emptyDirectory{}, f.transport.Address(), wire.ID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.gateway.Close)
	h, err := f.transport.Handler(ctx, f.gateway, func(ctx context.Context, source, target string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		f.authorized.Add(1)
		return gateway.BindingContext{Role: gateway.RolePeer, Target: target, PeerBootID: source}, denyUser{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	f.server.Config.Handler = h
	f.server.TLS = f.transport.ServerTLSConfig()
	f.server.StartTLS()
	f.route = gateway.Route{RouteClaim: gateway.RouteClaim{Target: "machine", OwnerBootID: f.gateway.BootID(), OwnerAddress: f.transport.Address(), RecoveryGeneration: wire.ID()}}
	return f
}

func TestMutualTLSNegotiatesPeerWithoutGrantingUserAccess(t *testing.T) {
	f := setup(t)
	var proxies atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxies.Add(1); http.Error(w, "proxy forbidden", 500) }))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	conn, err := f.client.Dial(f.ctx, f.source, f.route)
	if err != nil {
		t.Fatal(err)
	}
	session, err := yamux.Client(conn, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_, _, err = wire.Handshake(session, &pb.Message{Kind: "hello", Target: "machine", Payload: api.Payload(api.Hello{Version: api.Version, Role: gateway.RolePeer, PeerSource: f.source, PeerOwner: f.gateway.BootID()})})
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "ROUTE_STALE" {
		t.Fatal("certificate bypassed local ownership", err)
	}
	if f.authorized.Load() != 1 || proxies.Load() != 0 {
		t.Fatal("unexpected authorization or proxy routing")
	}
}

func TestPeerRejectsUnknownCAAndWrongServerName(t *testing.T) {
	f := setup(t)
	foreign := testcert.New(t)
	config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.ca.Roots(), Certificates: []tls.Certificate{foreign.Issue(t, "127.0.0.1", nil)}}
	dialer := websocket.Dialer{TLSClientConfig: config, HandshakeTimeout: time.Second}
	conn, _, err := dialer.DialContext(f.ctx, strings.Replace(f.transport.Address(), "https", "wss", 1), nil)
	if conn != nil {
		conn.Close()
	}
	if err == nil || f.authorized.Load() != 0 {
		t.Fatal("foreign certificate reached peer authorizer", err)
	}
	wrong := f.route
	wrong.OwnerAddress = strings.Replace(wrong.OwnerAddress, "127.0.0.1", "localhost", 1)
	if conn, err := f.client.Dial(f.ctx, f.source, wrong); err == nil {
		conn.Close()
		t.Fatal("wrong server hostname accepted")
	}
	if f.authorized.Load() != 0 {
		t.Fatal("wrong TLS identity reached authorizer")
	}
}

func TestPeerRequestBoundary(t *testing.T) {
	f := setup(t)
	leaf, err := x509.ParseCertificate(f.client.certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"plaintext", "old-tls", "forwarded-tls", "no-certificate", "foreign-chain", "origin", "bearer", "cookie", "source", "owner", "version", "duplicate", "path", "query", "host"} {
		t.Run(mode, func(t *testing.T) {
			r := httptest.NewRequest("GET", f.transport.Address(), nil)
			r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{leaf}}
			r.Header.Set(versionHeader, version)
			r.Header.Set(sourceHeader, f.source)
			r.Header.Set(ownerHeader, f.gateway.BootID())
			r.Header.Set(targetHeader, "machine")
			switch mode {
			case "plaintext":
				r.TLS = nil
			case "old-tls":
				r.TLS.Version = tls.VersionTLS12
			case "no-certificate":
				r.TLS.PeerCertificates = nil
			case "foreign-chain":
				foreign := testcert.New(t).Issue(t, "127.0.0.1", nil)
				cert, err := x509.ParseCertificate(foreign.Certificate[0])
				if err != nil {
					t.Fatal(err)
				}
				r.TLS.PeerCertificates = []*x509.Certificate{cert}
			case "forwarded-tls":
				r.TLS = nil
				r.Header.Set("X-Forwarded-Proto", "https")
				r.Header.Set("X-SSL-Client-Verify", "SUCCESS")
			case "origin":
				r.Header.Set("Origin", "https://browser.test")
			case "bearer":
				r.Header.Set("Authorization", "Bearer user-or-machine-token")
			case "cookie":
				r.Header.Set("Cookie", "dune_session=user")
			case "source":
				r.Header.Set(sourceHeader, "not-a-boot")
			case "owner":
				r.Header.Set(ownerHeader, wire.ID())
			case "version":
				r.Header.Set(versionHeader, "0")
			case "duplicate":
				r.Header.Add(targetHeader, "other-machine")
			case "path":
				r.URL.Path = "/tunnel"
			case "query":
				r.URL.RawQuery = "target=other"
			case "host":
				r.Host = "another-instance.test"
			}
			w := httptest.NewRecorder()
			f.server.Config.Handler.ServeHTTP(w, r)
			if w.Code != 401 && w.Code != 404 {
				t.Fatal("invalid peer request reached upgrade", w.Code)
			}
			if f.authorized.Load() != 0 {
				t.Fatal("invalid peer request reached authorizer")
			}
		})
	}
}

func TestPeerDoesNotFollowRedirectsOrAcceptWrongOwner(t *testing.T) {
	f := setup(t)
	wrong := f.route
	wrong.OwnerBootID = wire.ID()
	if conn, err := f.client.Dial(f.ctx, f.source, wrong); err == nil {
		conn.Close()
		t.Fatal("wrong boot accepted")
	}
	var requests atomic.Int32
	redirect := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, f.transport.Address(), http.StatusTemporaryRedirect)
	}))
	redirect.TLS = f.transport.ServerTLSConfig()
	redirect.StartTLS()
	defer redirect.Close()
	wrong = f.route
	wrong.OwnerAddress = redirect.URL + "/peer"
	if conn, err := f.client.Dial(f.ctx, f.source, wrong); err == nil {
		conn.Close()
		t.Fatal("redirect followed")
	}
	if requests.Load() != 1 || f.authorized.Load() != 0 {
		t.Fatal("request replayed or redirected")
	}
}

func TestPeerConfigurationAndRotation(t *testing.T) {
	ca, next := testcert.New(t), testcert.New(t)
	base := Config{Address: "https://127.0.0.1:443/private/peer", Certificate: ca.Issue(t, "127.0.0.1", nil), Roots: ca.Roots()}
	for _, value := range []string{"http://127.0.0.1/peer", "https://0.0.0.0/peer", "https://[::]/peer", "https://*/peer", "https://user:secret@127.0.0.1/peer", "https://127.0.0.1/peer?q=x", "https://127.0.0.1/a/../peer", "https://127.0.0.1:0/peer", "https://127.0.0.1:99999/peer", "https://127.0.0.1:443/peer#"} {
		bad := base
		bad.Address = value
		if _, err := New(bad); err == nil {
			t.Fatal("invalid address accepted", value)
		}
	}
	for _, mode := range []string{"no-roots", "unknown-root", "wrong-key", "wrong-host", "expired", "server-only"} {
		bad := base
		switch mode {
		case "no-roots":
			bad.Roots = nil
		case "unknown-root":
			bad.Roots = next.Roots()
		case "wrong-key":
			bad.Certificate.PrivateKey = next.Issue(t, "127.0.0.1", nil).PrivateKey
		case "wrong-host":
			bad.Certificate = ca.Issue(t, "another.test", nil)
		case "expired":
			bad.Certificate = ca.Issue(t, "127.0.0.1", func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Minute) })
		case "server-only":
			bad.Certificate = ca.Issue(t, "127.0.0.1", func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} })
		}
		if _, err := New(bad); err == nil {
			t.Fatal("invalid certificate accepted", mode)
		}
	}
	old, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	wrongGateway, err := gateway.NewWithDirectory(emptyDirectory{}, "https://other.test/peer", wire.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer wrongGateway.Close()
	if _, err := old.Handler(context.Background(), wrongGateway, func(context.Context, string, string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		return gateway.BindingContext{}, denyUser{}, nil
	}); err == nil {
		t.Fatal("mismatched directory advertisement accepted")
	}
	base.Roots.AddCert(next.Certificate)
	base.Certificate = next.Issue(t, "127.0.0.1", nil)
	if _, err := New(base); err != nil {
		t.Fatal("overlapping CA rotation rejected", err)
	}
	leaf, _ := x509.ParseCertificate(base.Certificate.Certificate[0])
	if _, err := verify([]*x509.Certificate{leaf}, old.roots, "", x509.ExtKeyUsageClientAuth); err == nil {
		t.Fatal("caller mutated installed trust roots")
	}
	base.Roots = next.Roots()
	if _, err := New(base); err != nil {
		t.Fatal("rotated trust set rejected", err)
	}
	oldLeaf, _ := x509.ParseCertificate(old.certificate.Certificate[0])
	if _, err := verify([]*x509.Certificate{oldLeaf}, base.Roots, "", x509.ExtKeyUsageClientAuth); err == nil {
		t.Fatal("removed CA remained trusted")
	}
}

func TestPeerCertificateExpiryClosesIdleConnection(t *testing.T) {
	f := setup(t)
	// Use a plain WebSocket dialer to exercise the server's independent expiry
	// timer; this client does not proactively close an expired certificate.
	cert := f.ca.Issue(t, "127.0.0.1", func(c *x509.Certificate) { c.NotAfter = time.Now().Add(3 * time.Second) })
	dialer := websocket.Dialer{HandshakeTimeout: time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.ca.Roots(), Certificates: []tls.Certificate{cert}}}
	conn, _, err := dialer.DialContext(f.ctx, strings.Replace(f.transport.Address(), "https", "wss", 1), http.Header{versionHeader: {version}, sourceHeader: {f.source}, ownerHeader: {f.gateway.BootID()}, targetHeader: {"machine"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := time.Now()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	remaining := time.Until(leaf.NotAfter)
	conn.SetReadDeadline(started.Add(4 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	if time.Since(started) > 3500*time.Millisecond {
		t.Fatal("idle connection outlived certificate")
	}
	if time.Since(started) < remaining-100*time.Millisecond {
		t.Fatal("peer connection closed before certificate expiry")
	}
}

func TestPeerAuthorizerCancellation(t *testing.T) {
	f := setup(t)
	h, err := f.transport.Handler(f.ctx, f.gateway, func(ctx context.Context, _, _ string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
		<-ctx.Done()
		return gateway.BindingContext{}, nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the callback directly with an authenticated TLS request; no
	// upgraded connection may survive cancellation of this admission attempt.
	leaf, _ := x509.ParseCertificate(f.client.certificate.Certificate[0])
	r := httptest.NewRequest("GET", f.transport.Address(), nil)
	r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{leaf}}
	r.Header = http.Header{versionHeader: {version}, sourceHeader: {f.source}, ownerHeader: {f.gateway.BootID()}, targetHeader: {"machine"}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || len(h.(*handler).slots) != 0 {
		t.Fatal("expired admission retained a slot", w.Code)
	}
}

func TestPeerCARotationOverHTTPS(t *testing.T) {
	old, next := testcert.New(t), testcert.New(t)
	roots := old.Roots()
	roots.AddCert(next.Certificate)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	newClient := func(ca *testcert.Authority) *Transport {
		t.Helper()
		client, err := New(Config{Address: "https://127.0.0.1:1/peer", Certificate: ca.Issue(t, "127.0.0.1", nil), Roots: roots})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	oldClient, nextClient := newClient(old), newClient(next)
	for _, overlap := range []bool{true, false} {
		t.Run(map[bool]string{true: "overlap", false: "old-ca-removed"}[overlap], func(t *testing.T) {
			server := httptest.NewUnstartedServer(nil)
			defer server.Close()
			trust := next.Roots()
			if overlap {
				trust = roots
			}
			transport, err := New(Config{Address: "https://" + server.Listener.Addr().String() + "/peer", Certificate: next.Issue(t, "127.0.0.1", nil), Roots: trust})
			if err != nil {
				t.Fatal(err)
			}
			g, err := gateway.NewWithDirectory(emptyDirectory{}, transport.Address(), wire.ID())
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			handler, err := transport.Handler(ctx, g, func(ctx context.Context, source, target string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
				return gateway.BindingContext{Target: target, Role: gateway.RolePeer, PeerBootID: source}, denyUser{}, ctx.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			server.Config.Handler, server.TLS = handler, transport.ServerTLSConfig()
			server.StartTLS()
			route := gateway.Route{RouteClaim: gateway.RouteClaim{OwnerAddress: transport.Address(), OwnerBootID: g.BootID(), Target: "machine"}}
			for _, client := range []*Transport{oldClient, nextClient} {
				conn, err := client.Dial(ctx, wire.ID(), route)
				if conn != nil {
					conn.Close()
				}
				allowed := overlap || client == nextClient
				if (err == nil) != allowed {
					t.Fatal("unexpected rotated trust decision", allowed, err)
				}
			}
		})
	}
}
