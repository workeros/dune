package fabricd

import (
	"fmt"
	"io"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func (d *Engine) watchTmux() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-tick.C:
			d.mu.Lock()
			var runtimes []*runtime
			for _, r := range d.runtimes {
				if r.tmux != nil {
					runtimes = append(runtimes, r)
				}
			}
			d.mu.Unlock()
			if len(runtimes) == 0 {
				continue
			}
			panes, err := d.tmux.Panes()
			if err != nil {
				continue
			} // An unavailable server is not evidence that a process exited.
			for _, r := range runtimes {
				p, exists := panes[r.id]
				if !exists {
					r.finish(-1)
				} else if p.Dead {
					r.finish(p.ExitCode)
				}
			}
		}
	}
}
func (d *Engine) interactTmux(s *executionStream, r *runtime, sub *subscription) {
	defer func() { r.mu.Lock(); delete(r.subs, sub); r.mu.Unlock() }()
	view, err := r.tmux.Attach(!sub.owner)
	if err != nil {
		s.Fail("TERMINAL_FAILED", err)
		return
	}
	defer view.Close()
	outputDone := make(chan error, 1)
	go func() {
		buf := make([]byte, wire.ChunkSize)
		for {
			n, e := view.Read(buf)
			if n > 0 {
				if err := s.Send(&pb.Message{Kind: "data", Data: append([]byte(nil), buf[:n]...)}); err != nil {
					outputDone <- err
					return
				}
			}
			if e != nil {
				outputDone <- e
				return
			}
		}
	}()
	inputDone := make(chan error, 1)
	go func() {
		for {
			m, e := s.Recv()
			if e != nil {
				inputDone <- e
				return
			}
			if !sub.owner {
				inputDone <- fmt.Errorf("observer cannot send input")
				return
			}
			switch m.Kind {
			case "input":
				if len(m.Data) > wire.ChunkSize {
					e = fmt.Errorf("input exceeds chunk limit")
				} else {
					e = view.Write(m.Data)
				}
			case "resize":
				var a api.Resize
				e = wire.Decode(m, &a)
				if e == nil {
					e = view.Resize(a.Rows, a.Cols)
				}
			case "signal":
				switch string(m.Data) {
				case "INT":
					e = view.Write([]byte{3})
				case "QUIT":
					e = view.Write([]byte{28})
				case "TERM", "HUP":
					e = d.stop(r)
				default:
					e = fmt.Errorf("unsupported signal")
				}
			default:
				e = fmt.Errorf("unsupported terminal input")
			}
			if e != nil {
				inputDone <- e
				return
			}
			if e = s.Send(&pb.Message{Kind: "written", RequestId: m.RequestId}); e != nil {
				inputDone <- e
				return
			}
		}
	}()
	exit := r.done
	for {
		select {
		case <-exit:
			// A dead pane remains available for native tmux history browsing. Consumers
			// interested only in process completion can close after receiving exit.
			if s.Send(&pb.Message{Kind: "exit", Payload: api.Payload(r.info().ExitCode)}) != nil {
				return
			}
			exit = nil
		case <-outputDone:
			if info := r.info(); info.ExitCode != nil && exit != nil {
				_ = s.Send(&pb.Message{Kind: "exit", Payload: api.Payload(info.ExitCode)})
			}
			return
		case err := <-inputDone:
			if err != io.EOF {
				s.Fail("INPUT_FAILED", err)
			}
			return
		case <-d.ctx.Done():
			return
		}
	}
}
