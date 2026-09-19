package fabricd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aiomni/dune/internal/tmux"
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
				r.readNativeSession()
				p, exists := panes[r.id]
				if exists && !p.Dead {
					r.observePTY(p, false)
				}
				if !exists || p.Dead {
					state, err := r.tmux.TimeoutState()
					if err == nil {
						r.readTimeoutState(state)
					}
					if err != nil || (state != nil && state.ExitCode == nil) {
						// A lost helper is not evidence of a natural exit or timeout.
						r.mu.Lock()
						if r.exit == nil {
							r.stopReason = "unknown"
						}
						r.mu.Unlock()
					}
				}
				if !exists {
					r.finish(-1)
				} else if p.Dead {
					r.finish(p.ExitCode)
				}
			}
		}
	}
}

func (r *runtime) readNativeSession() {
	native, err := r.tmux.NativeSession()
	if err != nil || native == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nativeSession == nil || native.Sequence > r.nativeSession.Sequence {
		r.nativeSession = native
	}
}
func (d *Engine) interactTmux(s *executionStream, r *runtime, sub *subscription) {
	// Display clients never write or determine pane size. The shared input
	// client below is the only writer, guarded by the current control epoch.
	view, err := r.tmux.Attach(true)
	if err != nil {
		r.unsubscribe(sub)
		s.Fail("TERMINAL_FAILED", err)
		return
	}
	defer func() { r.unsubscribe(sub); view.Close() }()
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
			write := func(fn func(*tmux.Viewer) error) error {
				return r.inputQueue(d.ctx).write(s.ctx, func(input *tmux.Viewer) error {
					return r.control.write(sub, m.ControlEpoch, func() error { return fn(input) })
				})
			}
			switch m.Kind {
			case "control":
				var request api.TerminalControl
				if e = wire.Decode(m, &request); e == nil {
					e = r.control.change(sub, request.Action)
				}
			case "history":
				var request api.TerminalControl
				if e = wire.Decode(m, &request); e == nil {
					e = write(func(*tmux.Viewer) error { return r.tmux.History(request.Action) })
				}
			case "input":
				if len(m.Data) > wire.ChunkSize {
					e = fmt.Errorf("input exceeds chunk limit")
				} else {
					data := append([]byte(nil), m.Data...)
					e = write(func(input *tmux.Viewer) error { return writePTY(input, data) })
				}
			case "resize":
				var a api.Resize
				e = wire.Decode(m, &a)
				if e == nil {
					e = view.Resize(a.Rows, a.Cols)
					if e == nil && m.ControlEpoch != 0 {
						e = write(func(input *tmux.Viewer) error { return input.Resize(a.Rows, a.Cols) })
					}
				}
			case "signal":
				switch string(m.Data) {
				case "INT":
					e = write(func(input *tmux.Viewer) error { return writePTY(input, []byte{3}) })
				case "QUIT":
					e = write(func(input *tmux.Viewer) error { return writePTY(input, []byte{28}) })
				case "TERM", "HUP":
					// Admit the stop before cancelling the queue; stopping while
					// holding its control lock would wait on that same queue.
					e = r.control.write(sub, m.ControlEpoch, func() error { return nil })
					if e == nil {
						e = d.stop(r)
					}
				default:
					e = fmt.Errorf("unsupported signal")
				}
			default:
				e = fmt.Errorf("unsupported terminal input")
			}
			if e != nil {
				var denied *api.Error
				if errors.As(e, &denied) && denied.Code == "READ_ONLY" {
					if s.Send(&pb.Message{Kind: "input_rejected", RequestId: m.RequestId, Code: denied.Code, Detail: denied.Detail}) != nil {
						inputDone <- e
						return
					}
					continue
				}
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
		case <-sub.controlChanged:
			state, epoch := r.control.state(sub)
			if s.Send(&pb.Message{Kind: "control", Payload: api.Payload(state), ControlEpoch: epoch}) != nil {
				return
			}
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
