package client

import (
	"context"
	"fmt"
	"slices"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// RuntimeSubscription observes all Runtimes on this exact Runner connection.
// Subscribe before bulk discovery. Any error invalidates this subscription;
// reconnect and rediscover rather than replaying a missed event history.
type RuntimeSubscription struct{ stream *Stream }

func (c *Client) SubscribeRuntimes(ctx context.Context) (*RuntimeSubscription, error) {
	if !slices.Contains(c.Binding.Capabilities, "runtime.watch") {
		return nil, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not advertise directory observations"}
	}
	stream, _, err := c.open(ctx, "runtime.watch", wire.ID(), struct{}{}, nil)
	if err != nil {
		return nil, err
	}
	return &RuntimeSubscription{stream: stream}, nil
}

func (s *RuntimeSubscription) Close() error { return s.stream.Close() }

func (s *RuntimeSubscription) Next() (api.RuntimeChange, error) {
	var change api.RuntimeChange
	message, err := s.stream.Recv()
	if err != nil {
		return change, err
	}
	if message.Kind != "runtime_changed" {
		return change, fmt.Errorf("unexpected Runtime observation %q; resynchronize", message.Kind)
	}
	if err := wire.Decode(message, &change); err != nil {
		return change, err
	}
	if change.Runtime.ID == "" || change.Runtime.Incarnation == "" || change.Runtime.Generation == 0 {
		return api.RuntimeChange{}, fmt.Errorf("incomplete Runtime observation identity")
	}
	return change, nil
}
