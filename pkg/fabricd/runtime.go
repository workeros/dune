package fabricd

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

	"google.golang.org/protobuf/proto"
)

const (
	acpLiveQueueMessages   = 16
	acpLiveQueueBytes      = wire.MaxMessage
	acpReplayQueueMessages = 1024
)

type subscription struct {
	q               chan *pb.Message
	failed          chan struct{}
	once            sync.Once
	owner           bool
	queueMu         sync.Mutex
	queuedMessages  int
	queuedBytes     int
	replayByteLimit int
	space           chan struct{}
}
type runtime struct {
	mu               sync.Mutex
	id, inc, adapter string
	title, cwd       string
	p                *process.Process
	subs             map[*subscription]bool
	exit             *int
	failure          string
	stopReason       string
	startedAt        *time.Time
	deadlineAt       *time.Time
	tmux             *tmux.Session
	done             chan struct{}
	acp              *acpController
	activity         api.AgentActivity
	nativeSession    *api.NativeSession
	operations       *operationLog
	ptyInput         *ptyInputQueue
}

// ACP parsing and replay budgets scale with the development machine while the
// wire frame stays fixed across peers. The caps keep one Runtime from turning
// a large native transcript into an unbounded process allocation.
func acpMemoryLimits(total uint64) (lineBytes, replayBytes int) {
	const gib = uint64(1024 * 1024 * 1024)
	switch {
	case total > 0 && total < 4*gib:
		return 2 * 1024 * 1024, 8 * 1024 * 1024
	case total >= 16*gib:
		return 8 * 1024 * 1024, 32 * 1024 * 1024
	default:
		return 4 * 1024 * 1024, 16 * 1024 * 1024
	}
}

func machineMemoryBytes() uint64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			var kib uint64
			if _, err := fmt.Sscan(fields[1], &kib); err == nil {
				return kib * 1024
			}
		}
	}
	return 0
}

func (r *runtime) info() api.Runtime {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := "running"
	if r.exit != nil {
		state = "exited"
	}
	activity := r.activityLocked()
	if r.exit != nil {
		activity.State = "unknown"
	}
	return api.Runtime{ID: r.id, Incarnation: r.inc, Generation: 1, Adapter: r.adapter, State: state, ExitCode: r.exit, StopReason: r.stopReason, StartedAt: r.startedAt, DeadlineAt: r.deadlineAt, Title: r.title, WorkingDirectory: r.cwd, Activity: &activity, NativeSession: r.nativeSession}
}

// The timeout helper owns these facts. fabricd only publishes its record; it
// does not reconstruct a deadline or restart a timer when the engine reopens.
func (r *runtime) readTimeoutState(state *process.PTYState) {
	if state == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startedAt, r.deadlineAt = &state.StartedAt, &state.DeadlineAt
	r.stopReason = state.StopReason
	if r.exit == nil && state.ExitCode != nil {
		r.exit = state.ExitCode
		r.endActivityLocked()
		close(r.done)
	}
}
func (r *runtime) stop() error {
	if r.tmux != nil {
		r.closePTYInput()
		// Publish completion before a closing viewer can report EOF.
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := r.tmux.Destroy(); err != nil {
			return err
		}
		if r.exit == nil {
			code := -1
			r.exit = &code
			r.stopReason = "stopped"
			r.endActivityLocked()
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
func (d *Engine) stop(r *runtime) error {
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
	r.endActivityLocked()
	if r.stopReason == "" {
		r.stopReason = "exited"
	}
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
	queueSize := acpLiveQueueMessages
	if r.acp != nil {
		queueSize = acpReplayQueueMessages
	}
	_, replayByteLimit := acpMemoryLimits(machineMemoryBytes())
	s := &subscription{q: make(chan *pb.Message, queueSize), failed: make(chan struct{}), owner: owner, replayByteLimit: replayByteLimit, space: make(chan struct{}, 1)}
	r.subs[s] = true
	if r.exit != nil {
		m := &pb.Message{Kind: "exit", Payload: api.Payload(r.exit)}
		if r.acp != nil {
			s.tryEnqueue(m, acpLiveQueueMessages, acpLiveQueueBytes)
		} else {
			s.q <- m
		}
	}
	return s, nil
}

func (s *subscription) tryEnqueue(m *pb.Message, messageLimit, byteLimit int) bool {
	size := proto.Size(m)
	s.queueMu.Lock()
	if s.queuedMessages >= messageLimit || s.queuedBytes+size > byteLimit {
		s.queueMu.Unlock()
		return false
	}
	s.queuedMessages++
	s.queuedBytes += size
	s.queueMu.Unlock()
	s.q <- m
	return true
}

func (s *subscription) enqueueReplay(m *pb.Message) bool {
	if s.tryEnqueue(m, acpReplayQueueMessages, s.replayByteLimit) {
		return true
	}
	timer := time.NewTimer(wire.WriteTimeout)
	defer timer.Stop()
	for {
		select {
		case <-s.failed:
			return false
		case <-s.space:
			if s.tryEnqueue(m, acpReplayQueueMessages, s.replayByteLimit) {
				return true
			}
		case <-timer.C:
			return false
		}
	}
}

func (s *subscription) release(m *pb.Message) {
	size := proto.Size(m)
	s.queueMu.Lock()
	s.queuedMessages--
	s.queuedBytes -= size
	s.queueMu.Unlock()
	select {
	case s.space <- struct{}{}:
	default:
	}
}

func (r *runtime) failSubscription(s *subscription) {
	s.once.Do(func() { close(s.failed) })
	r.mu.Lock()
	delete(r.subs, s)
	r.mu.Unlock()
}

func (r *runtime) emit(m *pb.Message) {
	r.mu.Lock()
	subs := make([]*subscription, 0, len(r.subs))
	for s := range r.subs {
		subs = append(subs, s)
	}
	r.mu.Unlock()
	replaying := r.acp != nil && r.acp.replaying.Load()
	for _, s := range subs {
		if r.acp != nil {
			queued := false
			if replaying {
				// session/load can synchronously replay hundreds of updates. Let a
				// bounded queue absorb the burst, then slow the Agent stdout reader
				// until the browser catches up instead of truncating valid history.
				queued = s.enqueueReplay(m)
			} else {
				queued = s.tryEnqueue(m, acpLiveQueueMessages, acpLiveQueueBytes)
			}
			if !queued {
				r.failSubscription(s)
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
		r.readACP(rd)
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

func (r *runtime) readACP(rd io.Reader) {
	lineLimit, _ := acpMemoryLimits(machineMemoryBytes())
	reader := bufio.NewReaderSize(rd, 64*1024)
	line := make([]byte, 0, 64*1024)
	lineBytes := 0
	oversized := false
	for {
		fragment, more, err := reader.ReadLine()
		lineBytes += len(fragment)
		if !oversized {
			if len(line)+len(fragment) <= lineLimit {
				line = append(line, fragment...)
			} else {
				oversized = true
				line = nil
			}
		}
		if !more && (lineBytes > 0 || err == nil) {
			if oversized {
				if r.acp != nil {
					r.acp.markOutputIncomplete()
				}
				r.emit(&pb.Message{Kind: "acp_notice", Payload: api.Payload(map[string]any{
					"code":          "MESSAGE_OMITTED",
					"detail":        "ACP output exceeded this machine's per-message memory limit; content was omitted and the Runtime continues",
					"message_bytes": lineBytes,
					"limit_bytes":   lineLimit,
				})})
			} else if !r.acceptACPLine(line) {
				return
			}
			line = line[:0]
			lineBytes = 0
			oversized = false
		}
		if err != nil {
			if err != io.EOF {
				r.emit(&pb.Message{Kind: "error", Code: "INVALID_ACP", Detail: err.Error()})
				r.stop()
			}
			return
		}
	}
}

func (r *runtime) acceptACPLine(b []byte) bool {
	if e := validateRPC(b); e != nil {
		r.emit(&pb.Message{Kind: "error", Code: "INVALID_ACP", Detail: e.Error()})
		r.stop()
		return false
	}
	if r.acp != nil {
		r.acp.receive(b)
	} else if len(b)+1024 <= wire.MaxMessage {
		r.emit(&pb.Message{Kind: "data", Data: append(append([]byte(nil), b...), '\n')})
	} else {
		r.emit(&pb.Message{Kind: "acp_notice", Payload: api.Payload(map[string]any{
			"code":          "MESSAGE_OMITTED",
			"detail":        "ACP output exceeded the transport frame; content was omitted and the Runtime continues",
			"message_bytes": len(b),
			"limit_bytes":   wire.MaxMessage - 1024,
		})})
	}
	return true
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
func (d *Engine) lookup(m *pb.Message) (*runtime, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.runtimes[m.RuntimeId]
	if r == nil || m.RuntimeGeneration != 1 || m.RuntimeIncarnation != r.inc {
		return nil, &api.Error{Code: "STALE_RUNTIME", Detail: "Runtime handle invalid"}
	}
	return r, nil
}
func (d *Engine) list() []api.Runtime {
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
func (d *Engine) prepare(s *executionStream, m *pb.Message) {
	d.profile(s, m, "environment")
}

func (d *Engine) start(s *executionStream, m *pb.Message) {
	d.profile(s, m, "agent")
}

func (d *Engine) profile(s *executionStream, m *pb.Message, kind string) {
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
	if p.Kind != kind {
		s.Fail("INVALID_ARGUMENT", fmt.Errorf("%s requires kind: %s", m.Operation, kind))
		return
	}
	hash := requestHash(m)
	d.mu.Lock()
	if kind == "environment" {
		if attempt := d.attempts[m.RequestId]; attempt != nil {
			d.mu.Unlock()
			if attempt.hash != hash {
				s.Fail("IDEMPOTENCY_CONFLICT", fmt.Errorf("execution ID has different Profile"))
				return
			}
			s.Fail("RESULT_UNKNOWN", fmt.Errorf("Profile execution already admitted; query profile.status"))
			return
		}
	}
	if kind == "agent" && len(d.runtimes)+len(d.starts) > 64 {
		d.mu.Unlock()
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("Runtime limit"))
		return
	}
	if cached := d.cache[m.RequestId]; cached != nil {
		d.mu.Unlock()
		if cached.hash != hash {
			s.Fail("IDEMPOTENCY_CONFLICT", fmt.Errorf("request ID has different Profile"))
			return
		}
		detail := "profile request already admitted; result unavailable"
		if kind == "agent" {
			detail += "; query Runtime list"
		}
		s.Fail("RESULT_UNKNOWN", fmt.Errorf("%s", detail))
		return
	}
	if !d.cacheRoomLocked() {
		d.mu.Unlock()
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("request cache full"))
		return
	}
	if kind == "environment" && !d.attemptRoomLocked() {
		d.mu.Unlock()
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("Profile attempt capacity, retry with a new execution ID after a completed attempt expires"))
		return
	}
	d.cache[m.RequestId] = &cached{hash: hash, at: time.Now()}
	if kind == "environment" {
		progress := api.ProfileProgress{ExecutionID: m.RequestId, Stage: "accepted", Step: -1}
		d.attempts[m.RequestId] = &profileAttempt{hash: hash, status: api.ProfileStatus{ExecutionID: m.RequestId, Kind: p.Kind, State: "running", Progress: progress}, at: time.Now()}
	}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.cache[m.RequestId].result = &pb.Message{Kind: "error", Code: "RESULT_UNKNOWN", Detail: "profile already executed; no stream replay"}
		d.mu.Unlock()
	}()
	if err := s.Send(&pb.Message{Kind: "accepted", RequestId: m.RequestId}); err != nil && kind != "environment" {
		return
	}
	for i, step := range p.Setup.Steps {
		if kind == "environment" {
			progress := api.ProfileProgress{ExecutionID: m.RequestId, Stage: "setup_running", Step: i, StepName: step.Name, StepsCompleted: i}
			d.updateProfileAttempt(m.RequestId, "running", progress, nil, nil)
			_ = s.Send(&pb.Message{Kind: "progress", RequestId: m.RequestId, Payload: api.Payload(progress)})
		}
		res, e := d.exec(api.Exec{Command: step, WorkingDirectory: p.WorkingDirectory, Env: p.Env})
		if e != nil || res.ExitCode != 0 || res.TimedOut {
			progress, failure := profileSetupFailure(m.RequestId, p.Kind, i, step.Name, res, e)
			if kind == "environment" {
				d.updateProfileAttempt(m.RequestId, "failed", progress, nil, failure)
			}
			_ = s.Send(profileFailureMessage(m.RequestId, progress, failure))
			return
		}
		if kind == "environment" {
			stepResult := res
			progress := api.ProfileProgress{ExecutionID: m.RequestId, Stage: "setup_completed", Step: i, StepName: step.Name, StepsCompleted: i + 1, StepResult: &stepResult}
			d.updateProfileAttempt(m.RequestId, "running", progress, nil, nil)
			_ = s.Send(&pb.Message{Kind: "progress", RequestId: m.RequestId, Payload: api.Payload(progress)})
		} else if s.Send(&pb.Message{Kind: "progress", Payload: api.Payload(map[string]any{"step": i, "name": step.Name, "result": res})}) != nil {
			return
		}
	}
	if kind == "environment" {
		result := &api.ProfileResult{Kind: p.Kind, Stage: "succeeded", StepsCompleted: len(p.Setup.Steps)}
		progress := api.ProfileProgress{ExecutionID: m.RequestId, Stage: "succeeded", Step: -1, StepsCompleted: result.StepsCompleted}
		d.updateProfileAttempt(m.RequestId, "succeeded", progress, result, nil)
		_ = s.Send(&pb.Message{Kind: "result", RequestId: m.RequestId, Payload: api.Payload(result)})
		return
	}
	d.startAgent(s, p, releaseSlot)
}

func profileSetupFailure(executionID, kind string, step int, stepName string, result api.ExecResult, cause error) (api.ProfileProgress, *api.ProfileFailure) {
	detail := fmt.Sprintf("setup step %d (%s) failed: exit_code=%d timed_out=%t", step, stepName, result.ExitCode, result.TimedOut)
	if cause != nil {
		detail = fmt.Sprintf("setup step %d (%s) failed: %v", step, stepName, cause)
	}
	detail = boundedProfileText(detail, api.MaxProfileFailureDetailBytes)
	stepResult := result
	failure := &api.ProfileFailure{Code: "SETUP_FAILED", Detail: detail, Step: step, StepName: stepName, StepResult: &stepResult}
	progress := api.ProfileProgress{ExecutionID: executionID, Stage: "failed", Step: step, StepName: stepName, StepsCompleted: step, Failure: failure}
	return fitProfileFailure(executionID, kind, progress, failure)
}

func fitProfileFailure(executionID, kind string, progress api.ProfileProgress, failure *api.ProfileFailure) (api.ProfileProgress, *api.ProfileFailure) {
	for !profileFailureFits(executionID, kind, progress, failure) {
		result := failure.StepResult
		if result == nil || (result.Stdout == "" && result.Stderr == "") {
			// Preserve a small structured error even if future response fields grow
			// without acquiring their own bound.
			failure = &api.ProfileFailure{Code: failure.Code, Detail: fmt.Sprintf("setup step %d failed", failure.Step), Step: failure.Step}
			progress = api.ProfileProgress{ExecutionID: executionID, Stage: "failed", Step: failure.Step, StepsCompleted: failure.Step, Failure: failure}
			return progress, failure
		}
		copy := *result
		if len(copy.Stdout) >= len(copy.Stderr) && copy.Stdout != "" {
			copy.Stdout = copy.Stdout[:len(copy.Stdout)/2]
			copy.StdoutTruncated, copy.Truncated = true, true
		} else {
			copy.Stderr = copy.Stderr[:len(copy.Stderr)/2]
			copy.StderrTruncated, copy.Truncated = true, true
		}
		failure.StepResult = &copy
		progress.Failure = failure
	}
	return progress, failure
}

func profileFailureFits(requestID, kind string, progress api.ProfileProgress, failure *api.ProfileFailure) bool {
	errorMessage := profileFailureMessage(requestID, progress, failure)
	statusProgress := progress
	statusProgress.StepResult = nil
	statusProgress.Failure = nil
	status := api.ProfileStatus{ExecutionID: progress.ExecutionID, Kind: kind, State: "failed", Progress: statusProgress, Failure: failure}
	statusMessage := &pb.Message{Kind: "result", RequestId: requestID, Payload: api.Payload(status)}
	return proto.Size(errorMessage) <= wire.MaxMessage && proto.Size(statusMessage) <= wire.MaxMessage
}

func profileFailureMessage(requestID string, progress api.ProfileProgress, failure *api.ProfileFailure) *pb.Message {
	return &pb.Message{Kind: "error", RequestId: requestID, Code: failure.Code, Detail: failure.Detail, Payload: api.Payload(progress)}
}

func boundedProfileText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func (d *Engine) updateProfileAttempt(executionID, state string, progress api.ProfileProgress, result *api.ProfileResult, failure *api.ProfileFailure) {
	d.mu.Lock()
	defer d.mu.Unlock()
	attempt := d.attempts[executionID]
	if attempt == nil {
		return
	}
	attempt.status.State = state
	if failure != nil {
		// Failure output lives only in Status.Failure. Keeping it in Progress as
		// well would multiply escaped binary output in the status response.
		progress.StepResult = nil
		progress.Failure = nil
	}
	attempt.status.Progress = progress
	attempt.status.Result = result
	attempt.status.Failure = failure
	attempt.at = time.Now()
}

func (d *Engine) startAgent(s *executionStream, p api.Profile, releaseSlot func()) {
	r := &runtime{id: wire.ID(), inc: d.inc, adapter: p.Adapter, subs: map[*subscription]bool{}, done: make(chan struct{})}
	argv, _ := p.Start.Args()
	r.title = filepath.Base(argv[0])
	r.cwd = p.WorkingDirectory
	if p.Adapter == "pty" {
		r.inc = wire.ID()
		session, err := d.tmux.Create(r.info(), argv, environment(p.Env), p.HistoryLines, time.Duration(p.Start.TimeoutSeconds)*time.Second)
		if err != nil {
			s.Fail("START_FAILED", err)
			return
		}
		r.tmux = session
		state, err := session.TimeoutState()
		if err != nil {
			_ = session.Destroy()
			s.Fail("START_FAILED", err)
			return
		}
		r.readTimeoutState(state)
		sub, _ := r.subscribe(true)
		d.mu.Lock()
		d.runtimes[r.id] = r
		releaseSlot()
		d.mu.Unlock()
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
func (d *Engine) attach(s *executionStream, m *pb.Message) {
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
func (d *Engine) interact(s *executionStream, r *runtime, sub *subscription) {
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
			e := s.Send(m)
			if r.acp != nil {
				sub.release(m)
			}
			if e != nil {
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
	keep := api.MaxExecOutputBytes - b.b.Len()
	if keep < len(p) {
		b.truncated = true
		p = p[:keep]
	}
	b.b.Write(p)
	return n, nil
}
func (d *Engine) exec(a api.Exec) (api.ExecResult, error) {
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
	out.StdoutTruncated = stdout.truncated
	out.StderrTruncated = stderr.truncated
	out.Truncated = out.StdoutTruncated || out.StderrTruncated
	out.ExitCode = p.Exit
	return out, nil
}
