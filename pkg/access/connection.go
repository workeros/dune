// Package access supplies connection-scoped access handling above the protocol
// core. Identity and grant validity come from the assembling application.
package access

import (
	"context"
	"fmt"
	"time"

	"github.com/aiomni/dune/pkg/gateway"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

// Grant is constructed by the application, never from a protocol request.
// A nil Valid is an explicit static grant for standalone deployments.
type Grant struct {
	Target string
	Role   string
	Valid  func() bool
	// Policy optionally enforces per-operation checks on an authenticated SDK connection.
	Policy *Policy
}

func (g Grant) check() error {
	if g.Valid != nil && !g.Valid() {
		return fmt.Errorf("access revoked")
	}
	return nil
}

// Bind creates a distinct handler retaining this connection's grant.
func (g Grant) Bind() (gateway.BindingContext, gateway.ConnectionHandler, error) {
	if g.Target == "" || (g.Role != gateway.RoleSDK && g.Role != gateway.RoleDaemon && g.Role != gateway.RoleEither) {
		return gateway.BindingContext{}, nil, fmt.Errorf("invalid grant")
	}
	if g.Policy != nil {
		policy := *g.Policy
		if g.Role != gateway.RoleSDK || policy.Checker == nil || policy.Scope.PrincipalID == "" || !policy.Scope.Binding.Valid() || policy.Scope.Binding.MachineID != g.Target {
			return gateway.BindingContext{}, nil, fmt.Errorf("invalid access policy")
		}
		g.Policy = &policy
	}
	if err := g.check(); err != nil {
		return gateway.BindingContext{}, nil, err
	}
	return gateway.BindingContext{Target: g.Target, Role: g.Role}, &connection{grant: g}, nil
}

type connection struct {
	grant Grant
}

func (c *connection) Connected(ctx context.Context, conn *gateway.Connection) error {
	if err := c.grant.check(); err != nil {
		return err
	}
	if c.grant.Valid != nil {
		go c.grant.watch(ctx, conn.Cancel)
	}
	return nil
}

func (g Grant) watch(ctx context.Context, cancel func(error)) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.check(); err != nil {
				cancel(err)
				return
			}
		}
	}
}

// Open authenticates one peer-delivered user stream using a grant reconstructed
// by the application. It requires a real user policy and ongoing validity check;
// it does not let a peer connection's identity become a standalone user grant.
func (g Grant) Open(ctx context.Context, request *pb.Message, stream *gateway.Stream) (gateway.StreamHandler, error) {
	if g.Role != gateway.RoleSDK || g.Policy == nil || g.Valid == nil {
		return nil, ErrDenied
	}
	_, handler, err := g.Bind()
	if err != nil {
		return nil, err
	}
	checked, err := handler.Open(ctx, request, stream)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(stream.Context())
	go g.watch(life, stream.Cancel)
	return &scopedStream{StreamHandler: checked, cancel: cancel}, nil
}

type scopedStream struct {
	gateway.StreamHandler
	cancel context.CancelFunc
}

func (s *scopedStream) Closed(err error) { s.cancel(); s.StreamHandler.Closed(err) }

func (c *connection) Open(ctx context.Context, request *pb.Message, stream *gateway.Stream) (gateway.StreamHandler, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.grant.check(); err != nil {
		return nil, err
	}
	if c.grant.Policy != nil {
		return c.openChecked(ctx, request, stream)
	}
	if err := stream.Forward(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *connection) Message(ctx context.Context, direction gateway.Direction, message *pb.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.grant.check()
}
func (c *connection) Closed(error) {}
