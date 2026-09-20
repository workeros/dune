package fabricd

import (
	"errors"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

// Submission reads bypass the transport result cache. Admission evidence is
// independently bounded and may remain after a Runtime leaves the active map.
func (d *Engine) querySubmission(s *executionStream, message *pb.Message, machine string) {
	var key api.SubmissionKey
	if err := wire.Decode(message, &key); err != nil || key.Validate() != nil {
		s.Fail("INVALID_ARGUMENT", errors.New("complete submission query key required"))
		return
	}
	target := key.Target
	if target.MachineID != machine || target.RuntimeID != message.RuntimeId || target.RuntimeIncarnation != message.RuntimeIncarnation || target.RuntimeGeneration != message.RuntimeGeneration {
		s.Fail("STALE_BINDING", errors.New("submission query target differs from the routed target"))
		return
	}
	if d.registry == nil {
		s.Fail("SESSION_UNAVAILABLE", errors.New("submission registry is unavailable"))
		return
	}
	select {
	case d.submissionReads <- struct{}{}:
		defer func() { <-d.submissionReads }()
	default:
		s.Fail("RESOURCE_EXHAUSTED", errors.New("submission read concurrency limit reached"))
		return
	}
	if err := s.Send(&pb.Message{Kind: "accepted", RequestId: message.RequestId}); err != nil {
		return
	}
	receipt, err := d.registry.Get(s.ctx, key)
	if err != nil {
		s.Fail("SESSION_UNAVAILABLE", errors.New("submission evidence could not be read"))
		return
	}
	_ = s.Send(&pb.Message{Kind: "result", RequestId: message.RequestId, Payload: api.Payload(receipt)})
}
