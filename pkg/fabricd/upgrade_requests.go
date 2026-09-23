package fabricd

import (
	"errors"
	"fmt"
	"github.com/aiomni/dune/internal/runnerupgrade"
	"github.com/aiomni/dune/internal/runningprogram"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func (d *Engine) upgradeRequest(s *executionStream, message *pb.Message) {
	if message.RuntimeId != "" {
		s.Fail("INVALID_ARGUMENT", fmt.Errorf("upgrade belongs to the Runner installation"))
		return
	}
	slots := d.upgradeReads
	if message.Operation == "runner.upgrade.preview" {
		slots = d.upgradePreviews
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("Runner upgrade request capacity exhausted"))
		return
	}
	controller, err := runnerupgrade.ControllerFor(d.stateDir)
	if err != nil {
		s.Fail("UPGRADE_UNSUPPORTED", fmt.Errorf("standard installation required"))
		return
	}
	if err := s.Send(&pb.Message{Kind: "accepted", RequestId: message.RequestId}); err != nil {
		return
	}
	var result any
	switch message.Operation {
	case "runner.upgrade.start":
		var request upgrade.Request
		if wire.Decode(message, &request) != nil || request.Validate() != nil || request.Binding.MachineID != message.Target {
			s.Fail("INVALID_ARGUMENT", fmt.Errorf("complete original upgrade request required"))
			return
		}
		running, inspectErr := runningprogram.Inspect(s.ctx)
		if inspectErr != nil {
			s.Fail("RUNNING_PROGRAM_UNVERIFIABLE", fmt.Errorf("kernel image unavailable"))
			return
		}
		result, err = controller.Start(s.ctx, request, running)
	case "runner.upgrade.preview":
		var request upgrade.PreviewRequest
		if wire.Decode(message, &request) != nil || !request.Binding.Valid() || request.Binding.MachineID != message.Target {
			s.Fail("INVALID_ARGUMENT", fmt.Errorf("complete preview binding required"))
			return
		}
		source, inspectErr := d.inspectInstallation(s.ctx, request.Binding)
		if inspectErr != nil {
			s.Fail("INSTALLATION_UNVERIFIABLE", fmt.Errorf("source installation unavailable"))
			return
		}
		result, err = controller.Preview(s.ctx, request, source)
	case "runner.upgrade.get":
		var query upgrade.Query
		if wire.Decode(message, &query) != nil || query.Validate() != nil || query.Binding.MachineID != message.Target {
			s.Fail("INVALID_ARGUMENT", fmt.Errorf("original upgrade selector required"))
			return
		}
		result, err = controller.Get(s.ctx, query)
	case "runner.upgrade.list":
		var request upgrade.ListRequest
		if wire.Decode(message, &request) != nil || request.Validate() != nil || request.Binding.MachineID != message.Target {
			s.Fail("INVALID_ARGUMENT", fmt.Errorf("complete upgrade history scope required"))
			return
		}
		result, err = controller.List(s.ctx, request)
	}
	if err != nil {
		code := "UPGRADE_UNAVAILABLE"
		var failure *api.Error
		if errors.As(err, &failure) {
			code = failure.Code
		}
		s.Fail(code, fmt.Errorf("Runner upgrade request failed"))
		return
	}
	_ = s.Send(&pb.Message{Kind: "result", Payload: api.Payload(result)})
}
