package daemon

import (
	"fmt"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"io"
	"net"
	"strconv"
	"time"
)

func (d *Daemon) port(s *wire.Stream, m *pb.Message) {
	var a api.Port
	if wire.Decode(m, &a) != nil || a.Port < 1 || a.Port > 65535 {
		s.Fail("INVALID_ARGUMENT", fmt.Errorf("port must be 1..65535"))
		return
	}
	select {
	case d.bulk <- struct{}{}:
		defer func() { <-d.bulk }()
	default:
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("bulk concurrency limit"))
		return
	}
	c, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(a.Port)), 3*time.Second)
	if e != nil {
		s.Fail("CONNECT_FAILED", e)
		return
	}
	defer c.Close()
	if s.Send(&pb.Message{Kind: "accepted"}) != nil {
		return
	}
	done := make(chan error, 2)
	go func() {
		b := make([]byte, wire.ChunkSize)
		for {
			n, e := c.Read(b)
			if n > 0 {
				if er := s.Send(&pb.Message{Kind: "data", Data: b[:n]}); er != nil {
					done <- er
					return
				}
			}
			if e != nil {
				if e == io.EOF {
					e = s.Send(&pb.Message{Kind: "eof"})
				}
				done <- e
				return
			}
		}
	}()
	go func() {
		for {
			m, e := s.Recv()
			if e != nil {
				done <- e
				return
			}
			switch m.Kind {
			case "data":
				if len(m.Data) > wire.ChunkSize {
					done <- fmt.Errorf("port chunk too large")
					return
				}
				_ = c.SetWriteDeadline(time.Now().Add(wire.WriteTimeout))
				_, e = c.Write(m.Data)
				if e != nil {
					done <- e
					return
				}
			case "eof":
				done <- c.(*net.TCPConn).CloseWrite()
				return
			default:
				done <- fmt.Errorf("invalid port message")
				return
			}
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case e := <-done:
			if e != nil {
				s.Fail("STREAM_INTERRUPTED", e)
				return
			}
		case <-d.ctx.Done():
			return
		}
	}
	_ = s.Send(&pb.Message{Kind: "result", Payload: []byte(`{"closed":true}`)})
}
