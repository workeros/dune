package fabricd

import (
	"context"
	"errors"
	"fmt"
	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/runningprogram"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func (d *Engine) inspectUpgrade(s *executionStream, message *pb.Message) {
	var binding runner.Binding
	if wire.Decode(message, &binding) != nil || !binding.Valid() || binding.MachineID != message.Target || message.RuntimeId != "" {
		s.Fail("INVALID_ARGUMENT", fmt.Errorf("complete Runner binding required"))
		return
	}
	if err := s.Send(&pb.Message{Kind: "accepted", RequestId: message.RequestId}); err != nil {
		return
	}
	observation, err := d.inspectInstallation(s.ctx, binding)
	if err != nil {
		s.Fail("INSTALLATION_UNVERIFIABLE", fmt.Errorf("actual Runner installation cannot be verified"))
		return
	}
	_ = s.Send(&pb.Message{Kind: "result", Payload: api.Payload(observation)})
}

func (d *Engine) inspectInstallation(ctx context.Context, binding runner.Binding) (upgrade.Inspection, error) {
	result := upgrade.Inspection{Binding: binding, Issues: []upgrade.Issue{}}
	program, err := runningprogram.Inspect(ctx)
	if err != nil {
		return result, err
	}
	result.Running = program
	result.StartedForUpgrade = d.upgradeStartup
	registration, err := installation.Find(d.stateDir)
	if err != nil {
		result.Issues = append(result.Issues, upgrade.Issue{Code: "STANDARD_INSTALLATION_REQUIRED"})
		return result, nil
	}
	var observed upgrade.Installation
	installed, err := installation.Lock(registration.Root)
	if errors.Is(err, installation.ErrBusy) {
		observed, err = installation.View(ctx, registration.Root)
	} else if err == nil {
		observed, err = installed.Observe(ctx)
		installed.Close()
	}
	if err != nil {
		return result, err
	}
	result.Installation, result.Supported = &observed, true
	return result, nil
}
