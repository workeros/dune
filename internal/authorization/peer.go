package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"google.golang.org/protobuf/proto"
)

// PeerAccess is carried only inside the mutually authenticated peer stream.
// It is short-lived and single-use at the owner. The owner revalidates the
// enterprise/local session, current Runner state and policy independently.
type PeerAccess struct {
	Nonce, SourceBootID, OwnerBootID, Target, Namespace, RequestDigest string
	ExpiresAt                                                          int64
	Connection                                                         ConnectionAccess
	Request                                                            access.Request
}

func (l *Service) WithPeers(bootID string) (*Service, error) {
	if !wire.ValidID(bootID) {
		return nil, identity.ErrUnauthorized
	}
	l.bootID = bootID
	return l, nil
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
		if l.ctx.Err() != nil || record.Scope() != request.Scope || route.Target != record.Target || route.Epoch != message.RouteEpoch || !wire.ValidID(route.OwnerBootID) || route.OwnerBootID == l.bootID || !l.validAccess(record)() {
			return nil, identity.ErrUnauthorized
		}
		digest, err := peerDigest(message)
		if err != nil {
			return nil, err
		}
		expiresAt := time.Now().Add(TicketLifetime)
		if decision.ValidUntil.Before(expiresAt) {
			expiresAt = decision.ValidUntil
		}
		expires := expiresAt.Unix()
		value := PeerAccess{Nonce: wire.ID(), SourceBootID: l.bootID, OwnerBootID: route.OwnerBootID, Target: record.Target, Namespace: record.Namespace, RequestDigest: digest, ExpiresAt: expires, Connection: record, Request: request}
		return json.Marshal(value)
	}
}

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

func (p *peerConnection) Open(ctx context.Context, message *pb.Message, flow *gateway.Stream) (gateway.StreamHandler, error) {
	l := p.service
	var value PeerAccess
	now := time.Now()
	if l.ctx.Err() != nil || json.Unmarshal(message.AccessContext, &value) != nil || !wire.ValidID(value.Nonce) || value.SourceBootID != p.source || value.OwnerBootID != l.bootID || value.Target != p.target || value.Namespace != l.sessions.Namespace() || value.ExpiresAt <= now.Unix() || value.ExpiresAt > now.Add(TicketLifetime).Unix() {
		return nil, identity.ErrUnauthorized
	}
	digest, err := peerDigest(message)
	if err != nil || digest != value.RequestDigest || value.Connection.Target != p.target {
		return nil, identity.ErrUnauthorized
	}
	l.mu.Lock()
	for nonce, expires := range l.peerSeen {
		if !now.Before(expires) {
			delete(l.peerSeen, nonce)
		}
	}
	if _, replayed := l.peerSeen[value.Nonce]; replayed || len(l.peerSeen) >= maxSeenPeerNonces {
		l.mu.Unlock()
		return nil, identity.ErrUnauthorized
	}
	l.peerSeen[value.Nonce] = time.Unix(value.ExpiresAt, 0)
	l.mu.Unlock()
	request, err := access.Describe(value.Connection.Scope(), message)
	if err != nil || request != value.Request || !l.validAccess(value.Connection)() {
		return nil, identity.ErrUnauthorized
	}
	if _, err := l.evaluate(ctx, request); err != nil {
		return nil, err
	}
	return (access.Grant{Target: p.target, Role: gateway.RoleSDK, Valid: l.validAccess(value.Connection), Policy: &access.Policy{Scope: value.Connection.Scope(), Checker: l.checker, Observer: l.observer}}).Open(ctx, message, flow)
}
