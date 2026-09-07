package gateway

import (
	"context"
	"fmt"
	"maps"
	"net"
	"reflect"
	"slices"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

const MaxAccessContext = 16 * 1024

// PeerDialer establishes an authenticated, confidential connection to the exact
// destination instance. The application authenticates both boot identities and
// validates the direct address; a load balancer or redirect is not a substitute.
// ctx bounds establishment only. Core owns every returned conn, even on error.
// Dialers must honor cancellation. Core never retries a dial or business request.
type PeerDialer func(ctx context.Context, sourceBootID string, destination Route) (net.Conn, error)

// NewWithPeers enables one-hop SDK routing in addition to directory ownership.
// Inbound peer connections must use an authenticated RolePeer BindingContext and
// a handler that independently validates every request's AccessContext. Default
// user or machine grants cannot authenticate this role. No HTTP or credentials
// are supplied by core; applications assemble the controlled peer entry point.
func NewWithPeers(directory Directory, address, recovery string, dial PeerDialer) (*Gateway, error) {
	if dial == nil {
		return nil, fmt.Errorf("peer dialer required")
	}
	g, err := NewWithDirectory(directory, address, recovery)
	if err != nil {
		return nil, err
	}
	g.dialPeer = dial
	return g, nil
}

// BootID identifies this Gateway incarnation to its peer transport and access
// module. It is empty for standalone Gateways and changes on every construction.
func (g *Gateway) BootID() string { return g.bootID }

func cloneRoute(route Route) Route {
	route.Binding.Capabilities = slices.Clone(route.Binding.Capabilities)
	route.Binding.Limits = maps.Clone(route.Binding.Limits)
	return route
}

func (g *Gateway) connectPeer(parent context.Context, target string) (*route, error) {
	started := time.Now()
	lookup, cancelLookup := context.WithTimeout(parent, directoryTimeout)
	lease, err := g.directory.Resolve(lookup, target)
	lookupErr := lookup.Err()
	cancelLookup()
	if err != nil {
		return nil, err
	}
	if lookupErr != nil {
		return nil, lookupErr
	}
	if !lease.Published || lease.Target != target || lease.RecoveryGeneration != g.recovery || lease.Epoch == 0 || lease.ValidFor <= 0 || lease.ValidFor > ownerLeaseLimit {
		return nil, ErrRouteStale
	}
	if !wire.ValidID(lease.OwnerBootID) || lease.OwnerBootID == g.bootID || lease.OwnerAddress == "" {
		return nil, ErrRouteStale
	}
	if lease.Binding.Target != target || lease.Binding.Version != api.Version || lease.Binding.Incarnation == "" || lease.Binding.Generation == 0 || lease.Binding.RouteRecovery != "" || lease.Binding.RouteEpoch != 0 {
		return nil, ErrRouteStale
	}
	// A delayed directory response cannot grant a fresh dialing window. Once the
	// original term is confirmed by its owner, that connection's owner/input
	// deadlines govern its lifetime; this entry does not renew someone else's term.
	deadline := minTime(started.Add(lease.ValidFor), time.Now().Add(5*time.Second))
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := g.dialPeer(ctx, g.bootID, cloneRoute(lease.Route))
	if conn != nil && (err != nil || ctx.Err() != nil) {
		conn.Close()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, fmt.Errorf("peer dialer returned nil connection")
	}
	keep := false
	defer func() {
		if !keep {
			conn.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	session, err := yamux.Client(conn, wire.Config())
	if err != nil {
		return nil, err
	}
	defer func() {
		if !keep {
			session.Close()
		}
	}()
	binding := lease.Binding
	binding.RouteRecovery, binding.RouteEpoch = lease.RecoveryGeneration, lease.Epoch
	control, welcome, err := wire.Handshake(session, &pb.Message{
		Kind: "hello", Target: target, Incarnation: binding.Incarnation,
		ConnectionGeneration: binding.Generation, RouteRecovery: binding.RouteRecovery, RouteEpoch: binding.RouteEpoch,
		Payload: api.Payload(api.Hello{Version: api.Version, Role: RolePeer, PeerSource: g.bootID, PeerOwner: lease.OwnerBootID}),
	})
	if err != nil {
		return nil, err
	}
	var accepted api.Binding
	if wire.Decode(welcome, &accepted) != nil || !reflect.DeepEqual(accepted, binding) || ctx.Err() != nil {
		return nil, ErrRouteStale
	}
	if !stop() || ctx.Err() != nil {
		return nil, ErrRouteStale
	}
	life, closeLife := context.WithCancel(parent)
	go func() {
		select {
		case <-session.CloseChan():
		case <-life.Done():
		}
		closeLife()
		session.Close()
	}()
	go func() { _, _ = control.Recv(); session.Close() }()
	// The destination must never initiate business streams on a peer session.
	go func() {
		if raw, err := session.AcceptStream(); err == nil {
			raw.Close()
			session.Close()
		}
	}()
	keep = true
	return &route{ctx: life, s: session, b: binding, peer: &lease.Route}, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
