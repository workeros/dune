package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"google.golang.org/protobuf/proto"
)

const peerPrefix = "dune_peer_access_"

// PeerReference is matched atomically when consuming a one-shot capability.
// RequestDigest covers all business bytes and execution/ownership identities,
// including Runtime identity. Raw commands, payloads and credentials are absent.
type PeerReference struct {
	SourceBootID, OwnerBootID, Target, Namespace, RequestDigest string
}

type PeerAccess struct {
	PeerReference
	Connection ConnectionAccess
	Request    access.Request
	DecisionID string
}

// WithPeers creates an immutable assembly for one Gateway boot. The underlying
// repository remains the selected shared Dune transaction domain.
func (l *Service) WithPeers(bootID string) (*Service, error) {
	if !wire.ValidID(bootID) {
		return nil, identity.ErrUnauthorized
	}
	peer := *l
	peer.bootID = bootID
	return &peer, nil
}

func peerDigest(m *pb.Message) (string, error) {
	copy := proto.Clone(m).(*pb.Message)
	copy.AccessContext, copy.InputLeaseId, copy.InputLeaseMs = nil, "", 0
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (l *Service) delegate(record ConnectionAccess) func(context.Context, gateway.Route, *pb.Message, access.Request, access.Decision) ([]byte, error) {
	return func(ctx context.Context, route gateway.Route, message *pb.Message, request access.Request, decision access.Decision) ([]byte, error) {
		if l.ctx.Err() != nil || record.Scope() != request.Scope || route.Target != record.Target || route.RecoveryGeneration != message.RouteRecovery || route.Epoch != message.RouteEpoch || !wire.ValidID(route.OwnerBootID) || route.OwnerBootID == l.bootID || !l.validAccess(record)() {
			return nil, identity.ErrUnauthorized
		}
		digest, err := peerDigest(message)
		if err != nil {
			return nil, err
		}
		value := peerPrefix + wire.ID() + wire.ID()
		r := PeerAccess{PeerReference: PeerReference{SourceBootID: l.bootID, OwnerBootID: route.OwnerBootID, Target: record.Target, Namespace: record.Namespace, RequestDigest: digest}, Connection: record, Request: request, DecisionID: decision.ID}
		err = l.bindings.CreatePeerAccess(ctx, credentialHash(value), r, min(TicketLifetime, time.Until(decision.ValidUntil)))
		if err != nil {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []byte(value), nil
	}
}

// Peer is only called after the confidential peer transport authenticates the
// source boot and target. This grants no user authority: Open consumes and checks
// an independent capability for each request before rechecking current policy.
func (l *Service) Peer(sourceBootID, target string) (gateway.BindingContext, gateway.ConnectionHandler, error) {
	if l.ctx.Err() != nil || !wire.ValidID(l.bootID) || !wire.ValidID(sourceBootID) || sourceBootID == l.bootID || target == "" {
		return gateway.BindingContext{}, nil, identity.ErrUnauthorized
	}
	return gateway.BindingContext{Target: target, Role: gateway.RolePeer, PeerBootID: sourceBootID}, &peerConnection{service: l, source: sourceBootID, target: target}, nil
}

type peerConnection struct {
	service        *Service
	source, target string
}

func (p *peerConnection) Connected(ctx context.Context, _ *gateway.Connection) error {
	return ctx.Err()
}
func (p *peerConnection) Open(ctx context.Context, m *pb.Message, flow *gateway.Stream) (gateway.StreamHandler, error) {
	l := p.service
	value := string(m.AccessContext)
	if l.ctx.Err() != nil || len(value) != len(peerPrefix)+64 || value[:len(peerPrefix)] != peerPrefix || m.Target != p.target {
		return nil, identity.ErrUnauthorized
	}
	digest, err := peerDigest(m)
	if err != nil {
		return nil, err
	}
	r, err := l.bindings.ConsumePeerAccess(ctx, credentialHash(value), PeerReference{SourceBootID: p.source, OwnerBootID: l.bootID, Target: p.target, Namespace: l.sessions.Namespace(), RequestDigest: digest})
	if err != nil {
		return nil, err
	}
	request, err := access.Describe(r.Connection.Scope(), m)
	if err != nil || request != r.Request {
		return nil, identity.ErrUnauthorized
	}
	return (access.Grant{Target: p.target, Role: gateway.RoleSDK, Valid: l.validAccess(r.Connection), Policy: &access.Policy{Scope: r.Connection.Scope(), Checker: l.checker}}).Open(ctx, m, flow)
}
