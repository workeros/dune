package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"log"
	"os"
	"path/filepath"
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
type Daemon struct {
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
}

func New(ctx context.Context) *Daemon {
	return &Daemon{inc: wire.ID(), starts: make(chan struct{}, 64), runtimes: map[string]*runtime{}, uploads: map[string]*upload{}, cache: map[string]*cached{}, bulk: make(chan struct{}, 4), ctx: ctx}
}
func Run(ctx context.Context, c config.Config) error {
	if e := c.Validate(); e != nil {
		return e
	}
	d := New(ctx)
	manager, e := tmux.Open(c.SessionDir)
	if e != nil {
		return e
	}
	d.tmux = manager
	d.stateDir = c.SessionDir
	lock, e := os.OpenFile(filepath.Join(c.SessionDir, "fabricd.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return fmt.Errorf("fabricd already running: %w", e)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	sessions, e := manager.Restore()
	if e != nil {
		return e
	}
	for _, session := range sessions {
		m := session.Runtime
		d.runtimes[m.ID] = &runtime{id: m.ID, inc: m.Incarnation, title: m.Title, cwd: m.WorkingDirectory, adapter: "pty", tmux: session, subs: map[*subscription]bool{}, done: make(chan struct{})}
	}
	go d.watchTmux()
	cleaner, e := process.NewCleaner()
	if e != nil {
		return e
	}
	d.cleaner = cleaner
	defer d.Close()
	go d.expire(ctx)
	tc, e := c.TLS()
	if e != nil {
		return e
	}
	delay := 100 * time.Millisecond
	for ctx.Err() == nil {
		sess, e := wire.Dial(ctx, c.Gateway, c.Token, tc)
		if e == nil {
			d.mu.Lock()
			d.generation++
			gen := d.generation
			d.mu.Unlock()
			b := api.Binding{Capabilities: capabilities, Limits: map[string]int{"message_bytes": wire.MaxMessage, "streams": wire.MaxStreams, "bulk": 4, "runtimes": 64, "uploads": 64, "dedup_entries": 256, "chunk_bytes": wire.ChunkSize}}
			ctrl, _, err := wire.Handshake(sess, &pb.Message{Kind: "hello", Target: c.Target, Incarnation: d.inc, ConnectionGeneration: gen, Payload: api.Payload(api.Hello{Version: api.Version, Role: "daemon"}), Data: api.Payload(b)})
			if err == nil {
				log.Printf("fabricd connected incarnation=%s generation=%d", d.inc, gen)
				delay = 100 * time.Millisecond
				stop := context.AfterFunc(ctx, func() { sess.Close() })
				go func() { _, _ = ctrl.Recv(); sess.Close() }()
				sem := make(chan struct{}, wire.MaxStreams)
				for {
					raw, err := sess.AcceptStream()
					if err != nil {
						break
					}
					select {
					case sem <- struct{}{}:
						go func() { defer func() { <-sem }(); d.handle(wire.Wrap(raw), c.Target, gen) }()
					default:
						raw.Close()
					}
				}
				stop()
			}
			sess.Close()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
	return nil
}
func (d *Daemon) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.runtimes {
		if r.tmux == nil {
			_ = r.stop()
		}
	}
	for _, u := range d.uploads {
		u.cleanup()
	}
	if d.cleaner != nil {
		d.cleaner.Close()
	}
}
func (d *Daemon) handle(s *wire.Stream, target string, gen uint64) {
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
func (d *Daemon) expire(ctx context.Context) {
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
func (d *Daemon) cacheRoomLocked() bool {
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
