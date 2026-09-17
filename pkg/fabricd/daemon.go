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

var capabilities = []string{"profile.prepare", "profile.start", "profile.status", "acp.action", "acp.state", "agent.operation.wait", "agent.operation.read", "pty.prompt", "pty.keys", "machine.info", "runtime.list", "runtime.get", "runtime.attach", "runtime.stop", "runtime.forget", "runtime.capture", "runtime.history", "exec", "files", "upload", "git", "worktree.list", "worktree.create", "ports.connect"}

type cached struct {
	hash   [32]byte
	result *pb.Message
	at     time.Time
}
type profileAttempt struct {
	hash   [32]byte
	status api.ProfileStatus
	at     time.Time
}
type Engine struct {
	cleaner     *process.Cleaner
	starts      chan struct{}
	mu          sync.Mutex
	inc         string
	generation  uint64
	runtimes    map[string]*runtime
	uploads     map[string]*upload
	cache       map[string]*cached
	attempts    map[string]*profileAttempt
	bulk        chan struct{}
	searchSlots chan struct{}
	fileMu      sync.Mutex
	gitLocks    gitRepositoryLocks
	ctx         context.Context
	tmux        *tmux.Server
	stateDir    string
	cancel      context.CancelFunc
	closeOnce   sync.Once
	active      sync.WaitGroup
	lock        *os.File
}

func newEngine(parent context.Context) *Engine {
	ctx, cancel := context.WithCancel(parent)
	return &Engine{cancel: cancel, inc: wire.ID(), starts: make(chan struct{}, 64), runtimes: map[string]*runtime{}, uploads: map[string]*upload{}, cache: map[string]*cached{}, attempts: map[string]*profileAttempt{}, bulk: make(chan struct{}, 4), searchSlots: make(chan struct{}, 2), ctx: ctx}
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
			r.closePTYInput()
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
	case "profile.prepare":
		d.prepare(s, m)
		return
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
	hash := requestHash(m)
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
	searchRequest := false
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
				var a api.ACPAction
				e = wire.Decode(m, &a)
				if e == nil {
					result, e = r.acp.action(a)
				}
			}
		}
	case "pty.prompt", "pty.keys":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil && r.tmux == nil {
			e = &api.Error{Code: "UNSUPPORTED", Detail: "PTY submission requires a terminal Runtime"}
		}
		if e == nil {
			if m.Operation == "pty.prompt" {
				var request api.PTYPrompt
				e = wire.Decode(m, &request)
				if e == nil {
					result, e = r.inputQueue(d.ctx).prompt(request)
				}
			} else {
				var request api.PTYKeys
				e = wire.Decode(m, &request)
				if e == nil {
					result, e = r.inputQueue(d.ctx).keys(request)
				}
			}
		}
	case "agent.operation.wait", "agent.operation.read":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil {
			if m.Operation == "agent.operation.wait" {
				var request api.AgentOperationWait
				e = wire.Decode(m, &request)
				if e == nil {
					result, e = r.operationLog().wait(s.ctx, request)
				}
			} else {
				var request api.AgentOperationRead
				e = wire.Decode(m, &request)
				if e == nil {
					result, e = r.operationLog().read(request)
				}
			}
		}
	case "runtime.list":
		result = d.list()
	case "profile.status":
		var request api.ProfileStatusRequest
		e = wire.Decode(m, &request)
		if e == nil {
			e = api.ValidateExecutionID(request.ExecutionID)
		}
		if e == nil {
			result = d.profileStatus(request.ExecutionID)
		}
	case "machine.info":
		home, err := os.UserHomeDir()
		e = err
		result = map[string]string{"home": home, "os": goruntime.GOOS, "arch": goruntime.GOARCH}
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
		if e == nil {
			r.closePTYInput()
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
					e = r.inputQueue(d.ctx).write(s.ctx, func(*tmux.Viewer) error { return r.tmux.History(req.Action) })
				}
			}
		}
	case "files":
		var a api.File
		e = wire.Decode(m, &a)
		if e == nil {
			if a.Action == "search" {
				searchRequest = true
				ctx, cancel := context.WithCancel(s.ctx)
				// Only this read-only unary operation is canceled by requester disconnect.
				go func() { _, _ = s.Recv(); cancel() }()
				result, e = d.filesContext(ctx, a)
				cancel()
			} else {
				result, e = d.files(a)
			}
		}
	case "upload":
		var a api.Upload
		e = wire.Decode(m, &a)
		if e == nil {
			result, e = d.uploadOp(a)
		}
	case "worktree.list", "worktree.create":
		var request api.WorktreeCreate
		e = wire.Decode(m, &request)
		if e == nil {
			if m.Operation == "worktree.list" {
				result, e = d.worktrees(request.Directory)
			} else {
				result, e = d.createWorktree(request)
			}
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
	if searchRequest || m.Operation == "agent.operation.read" {
		// Retain deduplication without duplicating up to 256 large read responses.
		d.cache[m.RequestId].result = &pb.Message{Kind: "error", RequestId: m.RequestId, Code: "RESULT_UNKNOWN", Detail: "read response is not retained; issue a new read"}
	} else {
		d.cache[m.RequestId].result = res
	}
	d.mu.Unlock()
	_ = s.Send(res)
}

func requestHash(m *pb.Message) [32]byte {
	return sha256.Sum256(append([]byte(m.Operation+"\x00"+m.RuntimeId+"\x00"+m.RuntimeIncarnation+fmt.Sprint(m.RuntimeGeneration)), m.Payload...))
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
			for id, attempt := range d.attempts {
				if attempt.status.State != "running" && now.Sub(attempt.at) > api.ProfileStatusRetentionSeconds*time.Second {
					delete(d.attempts, id)
				}
			}
			for _, r := range d.runtimes {
				r.mu.Lock()
				log := r.operations
				r.mu.Unlock()
				if log != nil {
					log.mu.Lock()
					log.expireLocked(now)
					log.mu.Unlock()
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

func (d *Engine) profileStatus(executionID string) api.ProfileStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	attempt := d.attempts[executionID]
	if attempt == nil {
		return api.ProfileStatus{ExecutionID: executionID, State: "unknown", Progress: api.ProfileProgress{ExecutionID: executionID, Stage: "unknown", Step: -1}}
	}
	return cloneProfileStatus(attempt.status)
}

func cloneProfileStatus(status api.ProfileStatus) api.ProfileStatus {
	if status.Progress.StepResult != nil {
		result := *status.Progress.StepResult
		status.Progress.StepResult = &result
	}
	if status.Progress.Failure != nil {
		failure := *status.Progress.Failure
		if failure.StepResult != nil {
			result := *failure.StepResult
			failure.StepResult = &result
		}
		status.Progress.Failure = &failure
	}
	if status.Result != nil {
		result := *status.Result
		status.Result = &result
	}
	if status.Failure != nil {
		failure := *status.Failure
		if failure.StepResult != nil {
			result := *failure.StepResult
			failure.StepResult = &result
		}
		status.Failure = &failure
	}
	return status
}

func (d *Engine) attemptRoomLocked() bool {
	if len(d.attempts) < api.MaxProfileAttempts {
		return true
	}
	id := ""
	var oldest time.Time
	for key, attempt := range d.attempts {
		if attempt.status.State != "running" && (id == "" || attempt.at.Before(oldest)) {
			id, oldest = key, attempt.at
		}
	}
	if id == "" {
		return false
	}
	delete(d.attempts, id)
	return true
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
