package peer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/transport/ws"
	"github.com/fasthttp/websocket"
)

const version = "1"
const versionHeader = "Dune-Peer-Version"
const sourceHeader = "Dune-Peer-Source"
const ownerHeader = "Dune-Peer-Owner"
const targetHeader = "Dune-Peer-Target"

// Authorize constructs only an authenticated peer connection handler. That
// handler must independently validate each request's user access context.
type Authorize func(ctx context.Context, sourceBootID, target string) (gateway.BindingContext, gateway.ConnectionHandler, error)

// Dial implements gateway.PeerDialer with TLS 1.3 and a dedicated client
// certificate. It never follows redirects, uses an ambient proxy, retries, or
// sends user/machine bearer credentials. ctx bounds establishment only.
func (t *Transport) Dial(ctx context.Context, source string, destination gateway.Route) (net.Conn, error) {
	u, err := address(destination.OwnerAddress)
	if err != nil {
		return nil, err
	}
	if !wire.ValidID(source) || !wire.ValidID(destination.OwnerBootID) || source == destination.OwnerBootID || !validTarget(destination.Target) || !time.Now().Before(t.expires) {
		return nil, fmt.Errorf("invalid peer binding")
	}
	u.Scheme = "wss"
	config := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cloneCertificate(t.certificate)}, RootCAs: t.roots.Clone(), NextProtos: []string{"http/1.1"}}
	dialer := websocket.Dialer{TLSClientConfig: config, HandshakeTimeout: 5 * time.Second}
	conn, response, err := dialer.DialContext(ctx, u.String(), http.Header{
		versionHeader: {version}, sourceHeader: {source}, ownerHeader: {destination.OwnerBootID}, targetHeader: {destination.Target},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return nil, fmt.Errorf("peer connection failed: %w", err)
	}
	if single(response.Header, versionHeader) != version || single(response.Header, ownerHeader) != destination.OwnerBootID {
		conn.Close()
		return nil, fmt.Errorf("peer owner negotiation failed")
	}
	tlsConn, ok := conn.UnderlyingConn().(*tls.Conn)
	if !ok || len(tlsConn.ConnectionState().VerifiedChains) == 0 {
		conn.Close()
		return nil, fmt.Errorf("verified peer TLS connection required")
	}
	expires := earlier(t.expires, chainExpiry(tlsConn.ConnectionState().VerifiedChains[0]))
	stream := ws.NetConn(conn)
	return &expiringConn{Conn: stream, timer: time.AfterFunc(time.Until(expires), func() { stream.Close() })}, nil
}

type expiringConn struct {
	net.Conn
	timer *time.Timer
}

func (c *expiringConn) Close() error { c.timer.Stop(); return c.Conn.Close() }

type handler struct {
	transport *Transport
	ctx       context.Context
	gateway   *gateway.Gateway
	authorize Authorize
	slots     chan struct{}
}

// Handler must be mounted with the complete advertised path preserved. It owns
// upgraded connections under the application lifetime, not the HTTP request's
// lifetime. Only direct TLS is accepted; TLS-termination forwarding headers are
// deliberately not an alternative to authenticating the actual TLS peer.
func (t *Transport) Handler(ctx context.Context, g *gateway.Gateway, authorize Authorize) (http.Handler, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if g == nil || !wire.ValidID(g.BootID()) || g.OwnerAddress() != t.Address() || authorize == nil {
		return nil, fmt.Errorf("matching directory Gateway address and peer authorizer required")
	}
	return &handler{transport: t, ctx: ctx, gateway: g, authorize: authorize, slots: make(chan struct{}, 256)}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := h.transport
	if h.ctx.Err() != nil || !time.Now().Before(t.expires) {
		http.Error(w, "peer unavailable", 503)
		return
	}
	if r.Method != http.MethodGet || r.URL.Path != t.address.Path || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || !strings.EqualFold(r.Host, t.address.Host) {
		http.NotFound(w, r)
		return
	}
	select {
	case h.slots <- struct{}{}:
	default:
		http.Error(w, "peer session limit", 503)
		return
	}
	defer func() { <-h.slots }()
	if r.TLS == nil || r.TLS.Version < tls.VersionTLS13 || r.Header.Get("Origin") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		http.Error(w, "unauthorized peer", 401)
		return
	}
	expires, err := verify(r.TLS.PeerCertificates, t.roots, "", x509.ExtKeyUsageClientAuth)
	if err != nil {
		http.Error(w, "unauthorized peer", 401)
		return
	}
	source, owner, target := single(r.Header, sourceHeader), single(r.Header, ownerHeader), single(r.Header, targetHeader)
	if single(r.Header, versionHeader) != version || !wire.ValidID(source) || owner != h.gateway.BootID() || source == owner || !validTarget(target) {
		http.Error(w, "invalid peer binding", 401)
		return
	}
	// The callback does not read an HTTP request or choose transport identities.
	// Bound its execution like the core's other trusted application hooks.
	ctx, cancel := context.WithDeadline(h.ctx, earlier(t.expires, expires))
	defer cancel()
	auth, stopAuth := context.WithTimeout(r.Context(), gateway.HandlerTimeout)
	stopLife := context.AfterFunc(ctx, stopAuth)
	binding, policy, err := h.authorize(auth, source, target)
	authErr := auth.Err()
	stopLife()
	stopAuth()
	if err != nil || authErr != nil || ctx.Err() != nil || policy == nil || binding.Role != gateway.RolePeer || binding.PeerBootID != source || binding.Target != target {
		http.Error(w, "unauthorized peer", 401)
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	conn, err := upgrader.Upgrade(w, r, http.Header{versionHeader: {version}, ownerHeader: {owner}})
	if err != nil {
		return
	}
	_ = h.gateway.ServeConn(ctx, ws.NetConn(conn), binding, policy)
}

func single(header http.Header, key string) string {
	values := header.Values(key)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}
func validTarget(value string) bool {
	return len(value) > 0 && len(value) <= 256 && !strings.ContainsFunc(value, func(r rune) bool { return r < 33 || r > 126 })
}
func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
