package fabricd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"os"
	goruntime "runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

var capabilities = []string{"submission.get", "profile.prepare", "profile.start", "profile.status", "agent.mcp.configure", "acp.state", "acp.conversation.read", "acp.conversation.get", "acp.conversation.changed", "agent.operation.wait", "agent.operation.read", "pty.prompt", "pty.keys", "machine.info", "runtime.list", "runtime.get", "runtime.attach", "runtime.stop", "runtime.forget", "runtime.capture", "runtime.scrollback", "runtime.history", "exec", "files", "upload", "git", "worktree.list", "worktree.create", "ports.connect"}

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
	cleaner           *process.Cleaner
	registry          *sessionregistry.Registry
	submissionReads   chan struct{}
	stateReads        chan struct{}
	discoveryReads    chan struct{}
	discoveryIssues   []api.RuntimeDiscoveryIssue
	discoveryScanMu   sync.Mutex
	discoveryNextScan time.Time
	launching         map[string]string
	operationWaits    chan struct{}
	streams           *wire.StreamCapacity
	starts            chan struct{}
	mu                sync.Mutex
	inc               string
	generation        uint64
	runtimes          map[string]*runtime
	uploads           map[string]*upload
	cache             map[string]*cached
	attempts          map[string]*profileAttempt
	bulk              chan struct{}
	searchSlots       chan struct{}
	conversations     *conversationStore
	conversationReads chan struct{}
	fileMu            sync.Mutex
	gitLocks          gitRepositoryLocks
	ctx               context.Context
	tmux              *tmux.Server
	stateDir          string
	acpTmux           *tmux.Server
	sessionTerm       uint64
	cancel            context.CancelFunc
	closeOnce         sync.Once
	active            sync.WaitGroup
	lock              *os.File
	cleanups          map[api.SubmissionKey]*cleanupExecution
	cleanupBarrier    func(api.SubmissionKey, string) error
}

func newEngine(parent context.Context) *Engine {
	ctx, cancel := context.WithCancel(parent)
	engine := &Engine{cancel: cancel, inc: wire.ID(), starts: make(chan struct{}, 64), runtimes: map[string]*runtime{}, uploads: map[string]*upload{}, cache: map[string]*cached{}, attempts: map[string]*profileAttempt{}, bulk: make(chan struct{}, 4), searchSlots: make(chan struct{}, 2), conversations: newConversationStore(), conversationReads: make(chan struct{}, 8), ctx: ctx}
	engine.submissionReads = make(chan struct{}, 8)
	engine.stateReads = make(chan struct{}, 16)
	engine.discoveryReads = make(chan struct{}, 8)
	engine.launching = make(map[string]string)
	engine.operationWaits = make(chan struct{}, 16)
	engine.streams = wire.NewStreamCapacity(1)
	engine.cleanups = make(map[api.SubmissionKey]*cleanupExecution)
	go engine.conversations.run(ctx)
	return engine
}

// Close releases connector resources and its state lock. Independent ACP hosts
// retain their Agent, controller, queue and pipes; host shutdown owns their cleanup.
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
			if r.host != nil {
				r.host.close()
				continue
			}
			r.closePTYInput()
			if r.tmux == nil {
				_ = r.stop()
				_ = r.waitStop(context.Background())
			}
		}
		for _, u := range d.uploads {
			u.cleanup()
		}
		if d.cleaner != nil {
			d.cleaner.Close()
		}
		if d.registry != nil {
			_ = d.registry.Close()
		}
		if d.lock != nil {
			_ = syscall.Flock(int(d.lock.Fd()), syscall.LOCK_UN)
			_ = d.lock.Close()
		}
	})
}
func (d *Engine) handle(s *executionStream, target string, gen uint64, lease *wire.StreamLease) {
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
	if class := wire.RequestClass(m); !lease.Move(class) {
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("fabricd %s stream capacity exhausted", class))
		return
	}
	d.dispatch(s, m, target)
}

func (d *Engine) dispatch(s *executionStream, m *pb.Message, target string) {
	if m.RuntimeId != "" && m.Operation != "submission.get" && m.Operation != "runtime.forget" {
		if r, err := d.lookup(m); err == nil && r.host != nil {
			if !sessionOperation(m.Operation) {
				s.Fail("UNSUPPORTED", fmt.Errorf("operation is not available on an ACP host"))
				return
			}
			r.host.forward(s, m, false)
			return
		}
	}
	var e error
	switch m.Operation {
	case "runtime.forget":
		d.submitForget(s, m, target)
		return
	case "runtime.stop":
		d.submitStop(s, m, target)
		return
	case "submission.acp":
		d.submitACP(s, m, target)
		return
	case "submission.raw":
		d.submitRaw(s, m, target)
		return
	case "submission.get":
		d.querySubmission(s, m, target)
		return
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
	retainResult := !sessionRead(m.Operation)
	if !retainResult {
		readers := d.stateReads
		if m.Operation == "agent.operation.wait" {
			readers = d.operationWaits
		}
		select {
		case readers <- struct{}{}:
			defer func() { <-readers }()
		default:
			s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("state read concurrency limit reached"))
			return
		}
	} else {
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
	}
	if s.Send(&pb.Message{Kind: "accepted", RequestId: m.RequestId}) != nil {
		if retainResult {
			d.mu.Lock()
			delete(d.cache, m.RequestId)
			d.mu.Unlock()
		}
		return
	}
	var result any
	searchRequest := false
	var conversationReadStarted time.Time
	switch m.Operation {
	case "acp.raw.state", "acp.raw.read":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil && r.raw == nil {
			e = &api.Error{Code: "UNSUPPORTED", Detail: "Runtime is not raw ACP"}
		}
		if e == nil {
			if m.Operation == "acp.raw.state" {
				result = r.raw.state()
			} else {
				var request api.RawACPRead
				if e = wire.Decode(m, &request); e == nil {
					result, e = r.raw.read(request)
				}
			}
		}
	case "exec":
		var a api.Exec
		e = wire.Decode(m, &a)
		if e == nil {
			result, e = d.exec(a)
		}
	case "agent.mcp.configure":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil {
			var config api.AgentMCP
			e = wire.Decode(m, &config)
			if e == nil {
				switch {
				case r.acp != nil:
					result, e = r.acp.configureMCP(config)
				case r.tmux != nil:
					result, e = r.tmux.ConfigureMCP(d.ctx, config)
				default:
					e = &api.Error{Code: "UNSUPPORTED", Detail: "Runtime has no managed MCP integration"}
				}
			}
		}
	case "acp.state", "acp.conversation.read", "acp.conversation.get":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil && r.acp == nil {
			e = &api.Error{Code: "UNSUPPORTED", Detail: "session is not managed ACP"}
		}
		if e == nil {
			if m.Operation == "acp.conversation.read" || m.Operation == "acp.conversation.get" {
				conversationReadStarted = time.Now()
				select {
				case d.conversationReads <- struct{}{}:
					defer func() { <-d.conversationReads }()
				default:
					e = &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "conversation read concurrency limit reached"}
				}
			}
			if e != nil {
				break
			}
			switch m.Operation {
			case "acp.state":
				result = r.acp.snapshot()
			case "acp.conversation.read":
				var request api.ACPConversationRead
				if e = wire.Decode(m, &request); e == nil {
					result, e = r.acp.conversation.read(request)
				} else {
					e = conversationArgument("invalid conversation read request")
				}
			case "acp.conversation.get":
				var request api.ACPConversationGet
				if e = wire.Decode(m, &request); e == nil {
					result, e = r.acp.conversation.get(request)
				} else {
					e = conversationArgument("invalid conversation get request")
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
		result = d.list(s.ctx)
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
		info := api.MachineInfo{Home: home, UserID: strconv.Itoa(os.Getuid()), OS: goruntime.GOOS, Arch: goruntime.GOARCH, ACPConversations: d.conversations.statistics(), StreamCapacity: d.streams.Snapshot()}
		if e == nil && d.registry != nil {
			var capacity api.SubmissionCapacity
			capacity, e = d.registry.Capacity(s.ctx)
			if e == nil {
				info.SubmissionCapacity = &capacity
			}
		}
		result = info
	case "runtime.get":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil {
			if r.tmux != nil {
				r.readNativeSession()
			}
			result = r.info()
		}
	case "runtime.capture", "runtime.scrollback", "runtime.history":
		var r *runtime
		r, e = d.lookup(m)
		if e == nil && r.tmux == nil {
			e = &api.Error{Code: "UNSUPPORTED", Detail: "terminal operation requires PTY"}
		}
		if e == nil {
			switch m.Operation {
			case "runtime.capture":
				result, e = r.tmux.Capture()
			case "runtime.scrollback":
				var req api.TerminalScrollbackRequest
				e = wire.Decode(m, &req)
				if e == nil {
					result, e = r.tmux.Scrollback(req.Limit)
				}
			case "runtime.history":
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
	if !conversationReadStarted.IsZero() {
		d.conversations.recordRead(len(res.Payload), time.Since(conversationReadStarted))
	}
	if e != nil {
		res = &pb.Message{Kind: "error", RequestId: m.RequestId, Code: "OPERATION_FAILED", Detail: e.Error()}
		if ae, ok := e.(*api.Error); ok {
			res.Code = ae.Code
			res.Payload = ae.Payload
		}
	}
	if retainResult {
		d.mu.Lock()
		if searchRequest || m.Operation == "agent.operation.read" || m.Operation == "runtime.scrollback" || m.Operation == "acp.conversation.read" || m.Operation == "acp.conversation.get" {
			// Retain deduplication without duplicating up to 256 large read responses.
			d.cache[m.RequestId].result = &pb.Message{Kind: "error", RequestId: m.RequestId, Code: "RESULT_UNKNOWN", Detail: "read response is not retained; issue a new read"}
		} else {
			d.cache[m.RequestId].result = res
		}
		d.mu.Unlock()
	}
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

// Observations consume bounded concurrent readers, never submission evidence or
// the ordinary transport cache. Repeating a read has no business side effect.
func sessionRead(operation string) bool {
	switch operation {
	case "machine.info", "runtime.list", "runtime.get", "acp.state", "acp.raw.state", "acp.raw.read", "acp.conversation.read", "acp.conversation.get", "agent.operation.wait", "agent.operation.read", "runtime.capture", "runtime.scrollback", "profile.status":
		return true
	default:
		return false
	}
}
