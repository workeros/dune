package fabricd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"os"
	goruntime "runtime"
	"sync"
	"syscall"
	"time"
)

var capabilities = []string{"profile.start", "acp.action", "acp.state", "agent.config", "machine.info", "runtime.list", "runtime.get", "runtime.attach", "runtime.stop", "runtime.forget", "runtime.capture", "runtime.history", "exec", "files", "upload", "git", "ports.connect"}

type cached struct {
	hash   [32]byte
	result *pb.Message
	at     time.Time
}
type Engine struct {
	cleaner    *process.Cleaner
	starts     chan struct{}
	mu         sync.Mutex
	inc        string
	generation uint64
	runtimes   map[string]*runtime
	uploads    map[string]*upload
	cache      map[string]*cached
	bulk       chan struct{}
	gitMu      sync.Mutex
	ctx        context.Context
	tmux       *tmux.Server
	stateDir   string
	profileMu  sync.Mutex
	cancel     context.CancelFunc
	closeOnce  sync.Once
	active     sync.WaitGroup
	lock       *os.File
}

func newEngine(parent context.Context) *Engine {
	ctx, cancel := context.WithCancel(parent)
	return &Engine{cancel: cancel, inc: wire.ID(), starts: make(chan struct{}, 64), runtimes: map[string]*runtime{}, uploads: map[string]*upload{}, cache: map[string]*cached{}, bulk: make(chan struct{}, 4), ctx: ctx}
}

// Close stops the connector and owned ACP processes and releases its state lock.
// It is idempotent and preserves tmux sessions and their working content.
func (d *Engine) Close() {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.cancel()
		d.mu.Unlock()
		// Stop admission before waiting, and retain the state lock until all
		// accepted work has left the old engine.
		d.active.Wait()
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, r := range d.runtimes {
			if r.tmux == nil {
				_ = r.stop()
				if r.p != nil {
					<-r.p.Done
				}
			}
		}
		for _, u := range d.uploads {
			u.cleanup()
		}
		if d.cleaner != nil {
			d.cleaner.Close()
		}
		if d.lock != nil {
			_ = syscall.Flock(int(d.lock.Fd()), syscall.LOCK_UN)
			_ = d.lock.Close()
		}
	})
}
func (d *Engine) handle(s *executionStream, target string, gen uint64) {
	defer s.Close()
	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, e := s.Recv()
	_ = s.SetReadDeadline(time.Time{})
	if e != nil {
		return
	}
	d.mu.Lock()
	currentGeneration := d.generation
	d.mu.Unlock()
	if gen != currentGeneration || m.Kind != "request" || m.Target != target || m.Incarnation != d.inc || m.ConnectionGeneration != gen || m.RequestId == "" {
		s.Fail("STALE_BINDING", fmt.Errorf("invalid request context"))
		return
	}
	if len(m.RequestId) > 128 {
		s.Fail("INVALID_ARGUMENT", fmt.Errorf("request ID too long"))
		return
	}
	switch m.Operation {
	case "profile.start":
		d.start(s, m)
		return
	case "runtime.attach":
		d.attach(s, m)
		return
	case "ports.connect":
		d.port(s, m)
		return
	}
	// Reserve before admission. Pending and completed entries cannot execute twice.
	hash := sha256.Sum256(append([]byte(m.Operation+"\x00"+m.RuntimeId+"\x00"+m.RuntimeIncarnation+fmt.Sprint(m.RuntimeGeneration)), m.Payload...))
	d.mu.Lock()
	if c := d.cache[m.RequestId]; c != nil {
		saved := c.result
		d.mu.Unlock()
		if c.hash != hash {
			s.Fail("IDEMPOTENCY_CONFLICT", fmt.Errorf("request ID has different payload"))
			return
		}
		if saved == nil {
			s.Fail("RESULT_UNKNOWN", fmt.Errorf("request still running"))
			return
		}
		_ = s.Send(&pb.Message{Kind: "accepted", RequestId: m.RequestId})
		_ = s.Send(saved)
		return
	}
	if !d.cacheRoomLocked() {
		d.mu.Unlock()
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("dedup capacity, retry with new request after 60 seconds"))
		return
	}
	d.cache[m.RequestId] = &cached{hash: hash, at: time.Now()}
	d.mu.Unlock()
	if s.Send(&pb.Message{Kind: "accepted", RequestId: m.RequestId}) != nil {
		d.mu.Lock()
		delete(d.cache, m.RequestId)
		d.mu.Unlock()
		return
	}
	var result any
	switch m.Operation {
	case "exec":
		var a api.Exec
		e = wire.Decode(m, &a)
		if e == nil {
			result, e = d.exec(a)
		}
	case "acp.state", "acp.action":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil && r.acp == nil {
			e = &api.Error{Code: "UNSUPPORTED", Detail: "session is not managed ACP"}
		}
		if e == nil {
			if m.Operation == "acp.state" {
				result = r.acp.snapshot()
			} else {
				var a acpAction
				e = wire.Decode(m, &a)
				if e == nil {
					result, e = r.acp.action(a)
				}
			}
		}
	case "runtime.list":
		result = d.list()
	case "machine.info":
		home, err := os.UserHomeDir()
		e = err
		result = map[string]string{"home": home, "os": goruntime.GOOS, "arch": goruntime.GOARCH}
	case "agent.config":
		var req api.AgentConfigRequest
		e = wire.Decode(m, &req)
		if e == nil {
			result, e = d.agentConfig(req)
		}
	case "runtime.forget":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil {
			d.mu.Lock()
			r.mu.Lock()
			if r.exit == nil {
				e = fmt.Errorf("stop the runtime before deleting its retained terminal history")
			} else {
				if r.tmux != nil {
					e = r.tmux.Destroy()
				}
				if e == nil {
					delete(d.runtimes, r.id)
				}
			}
			r.mu.Unlock()
			d.mu.Unlock()
		}
	case "runtime.get", "runtime.stop":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil {
			if m.Operation == "runtime.stop" {
				e = d.stop(r)
			}
			result = r.info()
		}
	case "runtime.capture", "runtime.history":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil && r.tmux == nil {
			e = &api.Error{Code: "UNSUPPORTED", Detail: "terminal operation requires PTY"}
		}
		if e == nil {
			if m.Operation == "runtime.capture" {
				result, e = r.tmux.Capture()
			} else {
				var req struct {
					Action string `json:"action"`
				}
				e = wire.Decode(m, &req)
				if e == nil {
					e = r.tmux.History(req.Action)
				}
			}
		}
	case "files":
		var a api.File
		e = wire.Decode(m, &a)
		if e == nil {
			result, e = d.files(a)
		}
	case "upload":
		var a api.Upload
		e = wire.Decode(m, &a)
		if e == nil {
			result, e = d.uploadOp(a)
		}
	case "git":
		var a api.Git
		e = wire.Decode(m, &a)
		if e == nil {
			result, e = d.git(a)
		}
	default:
		e = &api.Error{Code: "UNSUPPORTED", Detail: "unknown operation"}
	}
	res := &pb.Message{Kind: "result", RequestId: m.RequestId, Payload: api.Payload(result)}
	if e != nil {
		res = &pb.Message{Kind: "error", RequestId: m.RequestId, Code: "OPERATION_FAILED", Detail: e.Error()}
		if ae, ok := e.(*api.Error); ok {
			res.Code = ae.Code
		}
	}
	d.mu.Lock()
	d.cache[m.RequestId].result = res
	d.mu.Unlock()
	_ = s.Send(res)
}
func (d *Engine) expire(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			d.mu.Lock()
			for id, c := range d.cache {
				if c.result != nil && now.Sub(c.at) > time.Minute {
					delete(d.cache, id)
				}
			}
			for id, u := range d.uploads {
				u.mu.Lock()
				if now.After(u.expires) {
					u.cleanupLocked()
					delete(d.uploads, id)
				}
				u.mu.Unlock()
			}
			d.mu.Unlock()
		}
	}
}

// Completed results have a maximum 60s/256-entry retention, whichever ends first.
func (d *Engine) cacheRoomLocked() bool {
	if len(d.cache) < 256 {
		return true
	}
	id := ""
	var oldest time.Time
	for k, c := range d.cache {
		if c.result != nil && (id == "" || c.at.Before(oldest)) {
			id = k
			oldest = c.at
		}
	}
	if id == "" {
		return false
	}
	delete(d.cache, id)
	return true
}
