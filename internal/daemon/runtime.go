package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type subscription struct {
	q      chan *pb.Message
	failed chan struct{}
	once   sync.Once
	owner  bool
}
type runtime struct {
	mu               sync.Mutex
	id, inc, adapter string
	title, cwd       string
	p                *process.Process
	subs             map[*subscription]bool
	exit             *int
	failure          string
	tmux             *tmux.Session
	done             chan struct{}
	acp              *acpController
}

func (r *runtime) info() api.Runtime {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := "running"
	if r.exit != nil {
		state = "exited"
	}
	return api.Runtime{ID: r.id, Incarnation: r.inc, Generation: 1, Adapter: r.adapter, State: state, ExitCode: r.exit, Title: r.title, WorkingDirectory: r.cwd}
}
func (r *runtime) stop() error {
	if r.tmux != nil {
		// Publish completion before a closing viewer can report EOF.
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := r.tmux.Destroy(); err != nil {
			return err
		}
		if r.exit == nil {
			code := -1
			r.exit = &code
			close(r.done)
		}
		return nil
	}
	r.mu.Lock()
	p := r.p
	r.mu.Unlock()
	if p != nil {
		p.Close()
	}
	return nil
}

// Explicit PTY stop destroys the native session and its history. Naturally
// exited panes remain listed until runtime.forget.
func (d *Daemon) stop(r *runtime) error {
	if err := r.stop(); err != nil {
		return err
	}
	if r.tmux != nil {
		d.mu.Lock()
		delete(d.runtimes, r.id)
		d.mu.Unlock()
	}
	return nil
}
func (r *runtime) finish(code int) {
	r.mu.Lock()
	if r.exit != nil {
		r.mu.Unlock()
		return
	}
	r.exit = &code
	if r.done != nil {
		close(r.done)
	}
	r.mu.Unlock()
	if r.tmux == nil {
		r.emit(&pb.Message{Kind: "exit", Payload: api.Payload(code)})
	}
}
func (r *runtime) subscribe(owner bool) (*subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.subs) >= 8 {
		return nil, fmt.Errorf("subscription limit")
	}
	if owner {
		for s := range r.subs {
			if s.owner {
				return nil, &api.Error{Code: "INPUT_OWNED", Detail: "Runtime already has input owner"}
			}
		}
	}
	s := &subscription{q: make(chan *pb.Message, 16), failed: make(chan struct{}), owner: owner}
	r.subs[s] = true
	if r.exit != nil {
		s.q <- &pb.Message{Kind: "exit", Payload: api.Payload(r.exit)}
	}
	return s, nil
}
func (r *runtime) emit(m *pb.Message) {
	r.mu.Lock()
	subs := make([]*subscription, 0, len(r.subs))
	for s := range r.subs {
		subs = append(subs, s)
	}
	r.mu.Unlock()
	for _, s := range subs {
		if r.acp != nil {
			// A detached or stalled browser must never block the ACP controller.
			select {
			case s.q <- m:
			case <-s.failed:
			default:
				s.once.Do(func() { close(s.failed) })
				r.mu.Lock()
				delete(r.subs, s)
				r.mu.Unlock()
			}
			continue
		}
		timer := time.NewTimer(wire.WriteTimeout)
		select {
		case s.q <- m:
		case <-s.failed:
		case <-timer.C:
			s.once.Do(func() { close(s.failed) })
			r.mu.Lock()
			delete(r.subs, s)
			r.mu.Unlock()
		}
		timer.Stop()
	}
}
func (r *runtime) read(rd io.Reader, kind string, wg *sync.WaitGroup) {
	defer wg.Done()
	if c, ok := rd.(io.Closer); ok {
		defer c.Close()
	}
	if r.adapter == "acp" && kind == "data" {
		scan := bufio.NewScanner(rd)
		scan.Buffer(make([]byte, 4096), 256*1024)
		for scan.Scan() {
			b := append([]byte(nil), scan.Bytes()...)
			if e := validateRPC(b); e != nil {
				r.emit(&pb.Message{Kind: "error", Code: "INVALID_ACP", Detail: e.Error()})
				r.stop()
				return
			}
			if r.acp != nil {
				r.acp.receive(b)
			} else {
				r.emit(&pb.Message{Kind: kind, Data: append(b, '\n')})
			}
		}
		if e := scan.Err(); e != nil {
			r.emit(&pb.Message{Kind: "error", Code: "INVALID_ACP", Detail: e.Error()})
			r.stop()
		}
		return
	}
	b := make([]byte, wire.ChunkSize)
	for {
		n, e := rd.Read(b)
		if n > 0 {

			r.emit(&pb.Message{Kind: kind, Data: append([]byte(nil), b[:n]...)})
		}
		if e != nil {
			return
		}
	}
}
func validateRPC(b []byte) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || m == nil {
		return fmt.Errorf("ACP requires a JSON object per line")
	}
	var v string
	if json.Unmarshal(m["jsonrpc"], &v) != nil || v != "2.0" {
		return fmt.Errorf("ACP requires jsonrpc 2.0")
	}
	method, hasMethod := m["method"]
	id, hasID := m["id"]
	_, result := m["result"]
	er, hasError := m["error"]
	if hasID {
		var x any
		dec := json.NewDecoder(bytes.NewReader(id))
		dec.UseNumber()
		if dec.Decode(&x) != nil {
			return fmt.Errorf("invalid id")
		}
		switch x.(type) {
		case string, json.Number:
		default:
			return fmt.Errorf("id must be string or number")
		}
	}
	if hasMethod {
		var name string
		if json.Unmarshal(method, &name) != nil || name == "" || result || hasError {
			return fmt.Errorf("invalid request envelope")
		}
		if p, ok := m["params"]; ok && len(p) > 0 && p[0] != '{' && p[0] != '[' {
			return fmt.Errorf("params must be object or array")
		}
	} else {
		if !hasID || result == hasError {
			return fmt.Errorf("invalid response envelope")
		}
		if hasError {
			var x struct {
				Code    *int    `json:"code"`
				Message *string `json:"message"`
			}
			if json.Unmarshal(er, &x) != nil || x.Code == nil || x.Message == nil {
				return fmt.Errorf("invalid JSON-RPC error")
			}
		}
	}
	return nil
}
func (d *Daemon) lookup(m *pb.Message) (*runtime, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.runtimes[m.RuntimeId]
	if r == nil || m.RuntimeGeneration != 1 || m.RuntimeIncarnation != r.inc {
		return nil, &api.Error{Code: "STALE_RUNTIME", Detail: "Runtime handle invalid"}
	}
	return r, nil
}
func (d *Daemon) list() []api.Runtime {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []api.Runtime{}
	for _, r := range d.runtimes {
		out = append(out, r.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func environment(extra map[string]string) []string {
	m := map[string]string{}
	for _, s := range os.Environ() {
		k, v, ok := strings.Cut(s, "=")
		if ok {
			m[k] = v
		}
	}
	for k, v := range extra {
		m[k] = v
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
func (d *Daemon) start(s *wire.Stream, m *pb.Message) {
	select {
	case d.starts <- struct{}{}:

	default:
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("profile concurrency limit"))
		return
	}
	var release sync.Once
	releaseSlot := func() { release.Do(func() { <-d.starts }) }
	defer releaseSlot()
	var p api.Profile
	if e := wire.Decode(m, &p); e != nil {
		s.Fail("INVALID_ARGUMENT", e)
		return
	}
	if e := p.Validate(); e != nil {
		s.Fail("INVALID_ARGUMENT", e)
		return
	}
	d.mu.Lock()
	if len(d.runtimes)+len(d.starts) > 64 {
		d.mu.Unlock()
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("Runtime limit"))
		return
	}
	if _, ok := d.cache[m.RequestId]; ok {
		d.mu.Unlock()
		s.Fail("RESULT_UNKNOWN", fmt.Errorf("profile request already admitted; query Runtime list"))
		return
	}
	if !d.cacheRoomLocked() {
		d.mu.Unlock()
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("request cache full"))
		return
	}
	d.cache[m.RequestId] = &cached{at: time.Now()}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.cache[m.RequestId].result = &pb.Message{Kind: "error", Code: "RESULT_UNKNOWN", Detail: "profile already executed; no stream replay"}
		d.mu.Unlock()
	}()
	if s.Send(&pb.Message{Kind: "accepted", RequestId: m.RequestId}) != nil {
		return
	}
	for i, step := range p.Setup.Steps {
		res, e := d.exec(api.Exec{Command: step, WorkingDirectory: p.WorkingDirectory, Env: p.Env})
		if e != nil || res.ExitCode != 0 || res.TimedOut {
			s.Fail("SETUP_FAILED", fmt.Errorf("setup step %d (%s): %v %+v", i, step.Name, e, res))
			return
		}
		if s.Send(&pb.Message{Kind: "progress", Payload: api.Payload(map[string]any{"step": i, "name": step.Name, "result": res})}) != nil {
			return
		}
	}
	r := &runtime{id: wire.ID(), inc: d.inc, adapter: p.Adapter, subs: map[*subscription]bool{}, done: make(chan struct{})}
	argv, _ := p.Start.Args()
	r.title = filepath.Base(argv[0])
	r.cwd = p.WorkingDirectory
	if p.Adapter == "pty" {
		r.inc = wire.ID()
		session, err := d.tmux.Create(r.info(), argv, environment(p.Env), p.HistoryLines)
		if err != nil {
			s.Fail("START_FAILED", err)
			return
		}
		r.tmux = session
		sub, _ := r.subscribe(true)
		d.mu.Lock()
		d.runtimes[r.id] = r
		releaseSlot()
		d.mu.Unlock()
		if p.Start.TimeoutSeconds > 0 {
			go func() {
				select {
				case <-r.done:
				case <-d.ctx.Done():
				case <-time.After(time.Duration(p.Start.TimeoutSeconds) * time.Second):
					_ = d.stop(r)
				}
			}()
		}
		if s.Send(&pb.Message{Kind: "result", Payload: api.Payload(r.info())}) != nil {
			r.mu.Lock()
			delete(r.subs, sub)
			r.mu.Unlock()
			return
		}
		d.interact(s, r, sub)
		return
	}
	proc, e := process.Start(argv, p.WorkingDirectory, environment(p.Env))
	if e != nil {
		s.Fail("START_FAILED", e)
		return
	}
	r.p = proc
	if p.ManagedACP {
		r.acp = newACPController(r)
	}
	sub, _ := r.subscribe(true)
	d.mu.Lock()
	d.runtimes[r.id] = r
	releaseSlot()
	d.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(1)
	go r.read(proc.Output, "data", &wg)
	if proc.Stderr != nil {
		wg.Add(1)
		go r.read(proc.Stderr, "stderr", &wg)
	}
	go func() {
		<-proc.Done
		wg.Wait()
		r.mu.Lock()
		code := proc.Exit
		r.exit = &code
		r.mu.Unlock()
		if r.acp != nil {
			r.acp.closed()
		}
		r.emit(&pb.Message{Kind: "exit", Payload: api.Payload(code)})
		proc.Close()
	}()
	if r.acp != nil {
		go r.acp.initialize()
	}
	if p.Start.TimeoutSeconds > 0 {
		go func() {
			select {
			case <-proc.Done:
			case <-time.After(time.Duration(p.Start.TimeoutSeconds) * time.Second):
				r.stop()
			}
		}()
	}
	if s.Send(&pb.Message{Kind: "result", Payload: api.Payload(r.info())}) != nil {
		r.mu.Lock()
		delete(r.subs, sub)
		sub.once.Do(func() { close(sub.failed) })
		r.mu.Unlock()
		return
	}
	d.interact(s, r, sub)
}
func (d *Daemon) attach(s *wire.Stream, m *pb.Message) {
	r, e := d.lookup(m)
	if e != nil {
		s.Fail("STALE_RUNTIME", e)
		return
	}
	var a api.Attach
	if e = wire.Decode(m, &a); e != nil {
		s.Fail("INVALID_ARGUMENT", e)
		return
	}
	if r.acp != nil && !a.Observe {
		s.Fail("UNSUPPORTED", fmt.Errorf("managed ACP input must use acp.action"))
		return
	}
	sub, e := r.subscribe(!a.Observe)
	if e != nil {
		code := "INPUT_OWNED"
		if ae, ok := e.(*api.Error); ok {
			code = ae.Code
		}
		s.Fail(code, e)
		return
	}
	if s.Send(&pb.Message{Kind: "accepted", Payload: api.Payload(r.info())}) != nil {
		r.mu.Lock()
		delete(r.subs, sub)
		sub.once.Do(func() { close(sub.failed) })
		r.mu.Unlock()
		return
	}
	d.interact(s, r, sub)
}
func (d *Daemon) interact(s *wire.Stream, r *runtime, sub *subscription) {
	if r.tmux != nil {
		d.interactTmux(s, r, sub)
		return
	}
	defer func() { r.mu.Lock(); delete(r.subs, sub); r.mu.Unlock() }()
	done := make(chan error, 1)
	if r.acp != nil {
		if s.Send(&pb.Message{Kind: "acp_state", Payload: api.Payload(r.acp.snapshot())}) != nil {
			return
		}
	}
	go func() {
		for {
			m, e := s.Recv()
			if e != nil {
				done <- e
				return
			}
			if !sub.owner {
				done <- fmt.Errorf("observer cannot send input")
				return
			}
			r.mu.Lock()
			exited := r.exit != nil
			r.mu.Unlock()
			if exited {
				done <- fmt.Errorf("Runtime exited")
				return
			}
			switch m.Kind {
			case "input":
				if r.acp != nil {
					done <- fmt.Errorf("managed ACP input must use acp.action")
					return
				}
				if len(m.Data) > wire.ChunkSize {
					e = fmt.Errorf("input exceeds chunk limit")
				} else if r.adapter == "acp" {
					b := bytes.TrimSuffix(m.Data, []byte{'\n'})
					if bytes.ContainsRune(b, '\n') {
						e = fmt.Errorf("one ACP message per input required")
					} else {
						e = validateRPC(b)
						if e == nil {
							e = r.p.Write(append(append([]byte(nil), b...), '\n'))
						}
					}
				} else {
					e = r.p.Write(m.Data)
				}
			case "resize":
				e = fmt.Errorf("resize requires PTY")
			case "signal":
				e = r.p.Signal(string(m.Data))
			default:
				e = fmt.Errorf("unsupported input message")
			}
			if e != nil {
				done <- e
				return
			}
			if e = s.Send(&pb.Message{Kind: "written", RequestId: m.RequestId}); e != nil {
				done <- e
				return
			}
		}
	}()
	for {
		select {
		case e := <-done:
			if e != io.EOF {
				s.Fail("INPUT_FAILED", e)
			}
			return
		case <-sub.failed:
			s.Fail("SLOW_CONSUMER", fmt.Errorf("subscription queue full; output incomplete"))
			return
		case m := <-sub.q:
			if s.Send(m) != nil {
				return
			}
			if m.Kind == "exit" || m.Kind == "error" {
				return
			}
		case <-d.ctx.Done():
			return
		}
	}
}

type bounded struct {
	mu        sync.Mutex
	b         bytes.Buffer
	truncated bool
}

func (b *bounded) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	keep := 128*1024 - b.b.Len()
	if keep < len(p) {
		b.truncated = true
		p = p[:keep]
	}
	b.b.Write(p)
	return n, nil
}
func (d *Daemon) exec(a api.Exec) (api.ExecResult, error) {
	var out api.ExecResult
	argv, e := a.Command.Args()
	if e != nil {
		return out, e
	}
	if !filepath.IsAbs(a.WorkingDirectory) {
		return out, fmt.Errorf("working_directory required")
	}
	p, e := process.Start(argv, a.WorkingDirectory, environment(a.Env))
	if e != nil {
		return out, e
	}
	defer p.Close()
	var stdout, stderr bounded
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer p.Output.Close(); io.Copy(&stdout, p.Output) }()
	go func() { defer wg.Done(); defer p.Stderr.Close(); io.Copy(&stderr, p.Stderr) }()
	seconds := a.TimeoutSeconds
	if seconds == 0 {
		seconds = 300
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-p.Done:
	case <-timer.C:
		out.TimedOut = true
		p.Close()
		<-p.Done
	case <-d.ctx.Done():
		p.Close()
		<-p.Done
	}
	wg.Wait()
	out.Stdout = stdout.b.String()
	out.Stderr = stderr.b.String()
	out.Truncated = stdout.truncated || stderr.truncated
	out.ExitCode = p.Exit
	return out, nil
}
