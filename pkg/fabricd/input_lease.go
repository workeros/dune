package fabricd

import (
	"context"
	"fmt"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func confirmInputLease(input *wire.InputWindow, control *wire.Stream, grant *pb.Message, binding api.Binding) error {
	if grant.RouteEpoch != binding.RouteEpoch {
		return fmt.Errorf("input grant changed ownership identity")
	}
	if err := input.Confirm(grant.InputLeaseId, time.Duration(grant.InputLeaseMs)*time.Millisecond); err != nil {
		return err
	}
	return control.Send(&pb.Message{Kind: "lease_ready", InputLeaseId: grant.InputLeaseId, RouteEpoch: binding.RouteEpoch})
}

func renewInputLease(ctx context.Context, input *wire.InputWindow, control *wire.Stream, binding api.Binding) error {
	for {
		timer := time.NewTimer(wire.InputLeaseInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		id := wire.ID()
		if err := input.Begin(id); err != nil {
			return err
		}
		_, remaining := input.Current()
		_ = control.SetReadDeadline(time.Now().Add(remaining))
		if err := control.Send(&pb.Message{Kind: "lease_request", InputLeaseId: id, RouteEpoch: binding.RouteEpoch}); err != nil {
			return err
		}
		grant, err := control.Recv()
		if err != nil {
			return err
		}
		if grant.Kind != "lease_grant" {
			return fmt.Errorf("input lease grant required")
		}
		if err := confirmInputLease(input, control, grant, binding); err != nil {
			return err
		}
	}
}
