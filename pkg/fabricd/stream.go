package fabricd

import (
	"context"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

// executionStream binds all inbound messages to the accepted connection, not
// just its first request. In particular, bytes buffered before cancellation or
// a replacement connection must be checked after decoding and before admission.
// Already admitted operations are not rolled back when the connection changes.
type executionStream struct {
	*wire.Stream
	ctx        context.Context
	engine     *Engine
	generation uint64
}

func (s *executionStream) Recv() (*pb.Message, error) {
	m, err := s.Stream.Recv()
	if err != nil {
		return nil, err
	}
	s.engine.mu.Lock()
	current := s.engine.generation
	s.engine.mu.Unlock()
	if s.ctx.Err() != nil || s.engine.ctx.Err() != nil || current != s.generation {
		err := &api.Error{Code: "STALE_BINDING", Detail: "execution connection is no longer current"}
		s.Fail(err.Code, err)
		return nil, err
	}
	return m, nil
}
