package fabricd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/aiomni/dune/internal/agentintegration"
	"github.com/aiomni/dune/internal/lifecycle"
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
	acpLiveQueueMessages = 16
	acpLiveQueueBytes    = wire.MaxMessage
)

type subscription struct {
	q                chan *pb.Message
	failed           chan struct{}
	once             sync.Once
	canInput         bool
	controlChanged   chan struct{}
	queueMu          sync.Mutex
	queuedMessages   int
	queuedBytes      int
	space            chan struct{}
	conversationOnly bool
	changed          chan struct{}
	pendingChange    *api.ACPConversationChanged
}
type runtime struct {
	mu                     sync.Mutex
	spawnMu                sync.Mutex
	id, inc, adapter       string
	title, cwd             string
	projectID, directoryID string
	p                      *process.Process
	acpStart               func() (*process.Process, error)
	acpReadDone            chan struct{}
	stopped                bool
	stopDone               chan struct{}
	stopErr                error
	subs                   map[*subscription]bool
	exit                   *int
	failure                string
	stopReason             string
	startedAt              *time.Time
	deadlineAt             *time.Time
	tmux                   *tmux.Session
	done                   chan struct{}
	acp                    *acpController
	raw                    *rawACP
	host                   *sessionProxy
	hostInfo               *api.ACPHostInfo
	events                 *lifecycle.Log
	target                 api.SubmissionTarget
	activity               api.AgentActivity
	nativeSession          *api.NativeSession
	ptyActivityMu          sync.Mutex
	ptyProbeAfter          time.Time
	operations             *operationLog
	conversations          *conversationStore
	ptyInput               *ptyInputQueue
	control                terminalControl
}

// ACP parsing and replay budgets scale with the development machine while the
// wire frame stays fixed across peers. The caps keep one Runtime from turning
// a large native transcript into an unbounded process allocation.
func acpInputLimit(total uint64) int {
	const gib = uint64(1024 * 1024 * 1024)
	switch {
	case total > 0 && total < 4*gib:
		return 2 * 1024 * 1024
	case total >= 16*gib:
		return 8 * 1024 * 1024
	default:
		return 4 * 1024 * 1024
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
	if r.host != nil {
		return r.host.information()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := "running"
	if r.exit != nil {
		state = "exited"
	}
	conversationID := ""
	if r.acp != nil {
		if description := r.acp.conversation.describe(); description != nil {
			conversationID = description.ID
		}
	}
	activity := r.activityLocked()
	if r.exit != nil {
		activity.State = "unknown"
	}
	info := api.Runtime{ConversationID: conversationID, ProjectID: r.projectID, DirectoryID: r.directoryID, ID: r.id, Incarnation: r.inc, Generation: 1, Adapter: r.adapter, State: state, ExitCode: r.exit, StopReason: r.stopReason, StartedAt: r.startedAt, DeadlineAt: r.deadlineAt, Title: r.title, WorkingDirectory: r.cwd, Activity: &activity, NativeSession: r.nativeSession}
	if r.hostInfo != nil {
		host := *r.hostInfo
		host.LifecycleLog = lifecycleUsage(r.events)
		if r.p != nil && r.p.Cmd != nil && r.p.Cmd.Process != nil {
			host.AgentPID, host.GroupID = r.p.PID, r.p.Cmd.Process.Pid
		}
		info.ACPHost = &host
	}
	if r.acp != nil || r.raw != nil {
		info.PersistentACP = true
		info.ACPMode = "managed"
		if r.raw != nil {
			info.ACPMode = "raw"
		}
	}
	return info
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
	if r.raw != nil {
		r.raw.closeInput()
	}
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
	first := !r.stopped
	r.stopped = true
	if first {
		r.stopDone = make(chan struct{})
		if r.exit == nil && r.stopReason == "" {
			r.stopReason = "stopped"
		}
	}
	p := r.p
	r.mu.Unlock()
	if p != nil {
		p.Close()
	}
	if first {
		go r.confirmStop()
	}
	return nil
}

// Closing the current pipe does not wait for ACP writes or a replacement's
// initialize. The short spawn boundary accounts for a concurrently starting
// replacement before publishing confirmed exit.
func (r *runtime) confirmStop() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r.spawnMu.Lock()
	r.mu.Lock()
	p := r.p
	r.mu.Unlock()
	r.spawnMu.Unlock()
	var err error
	code := -1
	if p != nil {
		p.Close()
		err = p.WaitGroupExit(ctx)
		if err == nil {
			code = p.Exit
		}
	}
	if err == nil {
		if r.raw != nil {
			select {
			case <-r.raw.done:
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
	}
	if err == nil {
		if r.acp != nil {
			r.acp.mu.Lock()
			r.acp.closedWithExitLocked(&code)
			r.acp.mu.Unlock()
		}
		r.finish(code)
	}
	r.mu.Lock()
	r.stopErr = err
	close(r.stopDone)
	r.mu.Unlock()
}

func (r *runtime) waitStop(ctx context.Context) error {
	if r.tmux != nil {
		return nil // Destroy synchronously confirms the PTY session removal.
	}
	r.mu.Lock()
	done := r.stopDone
	r.mu.Unlock()
	select {
	case <-done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
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
	if r.raw != nil {
		r.raw.closeInput()
	}
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
func (r *runtime) subscribe(owner bool, conversationOnly bool) (*subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.subs) >= 8 {
		return nil, &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "Runtime subscription limit reached"}
	}
	if owner && r.tmux == nil {
		for s := range r.subs {
			if s.canInput {
				return nil, &api.Error{Code: "INPUT_OWNED", Detail: "Runtime already has input owner"}
			}
		}
	}
	s := &subscription{conversationOnly: conversationOnly, changed: make(chan struct{}, 1), q: make(chan *pb.Message, acpLiveQueueMessages), failed: make(chan struct{}), canInput: owner}
	r.subs[s] = true
	if r.tmux != nil {
		s.controlChanged = make(chan struct{}, 1)
		r.control.add(s)
	}
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

func (s *subscription) release(m *pb.Message) {
	size := proto.Size(m)
	s.queueMu.Lock()
	s.queuedMessages--
	s.queuedBytes -= size
	s.queueMu.Unlock()
}

func (r *runtime) failSubscription(s *subscription) {
	s.once.Do(func() { close(s.failed) })
	r.unsubscribe(s)
}

func (r *runtime) unsubscribe(s *subscription) {
	r.mu.Lock()
	delete(r.subs, s)
	r.mu.Unlock()
	if r.tmux != nil {
		r.control.remove(s)
	}
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
			if m.Kind == "acp_conversation_changed" {
				if s.enqueueConversationChange(m) {
					r.conversations.mu.Lock()
					r.conversations.usage.NotificationMerges++
					r.conversations.mu.Unlock()
				}
				continue
			}
			if s.conversationOnly && m.Kind != "acp_state" && m.Kind != "exit" && m.Kind != "error" {
				continue
			}
			// Diagnostics never backpressure model ingestion, including load replay.
			queued := s.tryEnqueue(m, acpLiveQueueMessages, acpLiveQueueBytes)
			if !queued {
				r.conversations.mu.Lock()
				r.conversations.usage.SlowConsumerClosures++
				r.conversations.mu.Unlock()
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
	if r.acp != nil && kind == "stderr" {
		r.acp.readStderr(rd)
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

func (r *runtime) readACP(rd io.Reader) { r.readACPConnection(rd, nil) }

func (r *runtime) readACPConnection(rd io.Reader, connection *process.Process) {
	lineLimit := acpInputLimit(machineMemoryBytes())
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
				r.omitACPLineFrom(connection, lineBytes, lineLimit)
			} else if !r.acceptACPLineFrom(line, connection) {
				return
			}
			line = line[:0]
			lineBytes = 0
			oversized = false
		}
		if err != nil {
			if err != io.EOF {
				r.failACPReadFrom(connection, err)
			}
			return
		}
	}
}

func (r *runtime) omitACPLineFrom(connection *process.Process, lineBytes, lineLimit int) {
	if r.acp != nil {
		r.acp.mu.Lock()
		defer r.acp.mu.Unlock()
		if !r.acp.acceptsOutputLocked(connection) {
			return
		}
		r.acp.markOutputIncompleteLocked()
	}
	r.emit(&pb.Message{Kind: "acp_notice", Payload: api.Payload(map[string]any{
		"code":          "MESSAGE_OMITTED",
		"detail":        "ACP output exceeded this machine's per-message memory limit; content was omitted and the Runtime continues",
		"message_bytes": lineBytes,
		"limit_bytes":   lineLimit,
	})})
}

func (r *runtime) failACPReadFrom(connection *process.Process, err error) {
	if r.acp != nil {
		r.acp.mu.Lock()
		defer r.acp.mu.Unlock()
		if !r.acp.acceptsOutputLocked(connection) {
			return
		}
	}
	r.emit(&pb.Message{Kind: "error", Code: "INVALID_ACP", Detail: err.Error()})
	r.stop()
}

func (r *runtime) acceptACPLine(b []byte) bool { return r.acceptACPLineFrom(b, nil) }

func (r *runtime) acceptACPLineFrom(b []byte, connection *process.Process) bool {
	if r.acp != nil {
		r.acp.inputMu.Lock()
		defer r.acp.inputMu.Unlock()
		r.acp.mu.Lock()
		current := !r.acp.reconnecting && (connection == nil || r.acp.connection == connection)
		r.acp.mu.Unlock()
		if !current {
			return true
		}
	}
	if e := validateRPC(b); e != nil {
		// The conversation may have changed while the line was validated.
		r.failACPReadFrom(connection, e)
		return false
	}
	if r.acp != nil {
		r.acp.receive(b)
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
	missing := d.runtimes[m.RuntimeId] == nil
	d.mu.Unlock()
	if missing {
		d.refreshSessionRegistrations(d.ctx)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.runtimes[m.RuntimeId]
	if r == nil {
		if incarnation := d.launching[m.RuntimeId]; incarnation != "" && incarnation == m.RuntimeIncarnation && m.RuntimeGeneration == 1 {
			return nil, &api.Error{Code: "HOST_REGISTRATION_PENDING", Detail: "original Runtime launch has not confirmed its host"}
		}
		for _, issue := range d.discoveryIssues {
			if known := issue.Runtime; known != nil && known.ID == m.RuntimeId && known.Incarnation == m.RuntimeIncarnation && known.Generation == m.RuntimeGeneration {
				return nil, &api.Error{Code: issue.Code, Detail: "original Runtime registration could not be verified"}
			}
		}
		for _, issue := range d.discoveryIssues {
			if issue.Runtime == nil {
				return nil, &api.Error{Code: "SESSION_UNAVAILABLE", Detail: "original Runtime registration cannot currently be checked"}
			}
		}
	}
	if r == nil || m.RuntimeGeneration != 1 || m.RuntimeIncarnation != r.inc {
		return nil, &api.Error{Code: "STALE_RUNTIME", Detail: "Runtime handle invalid"}
	}
	return r, nil
}
func (d *Engine) list(ctx context.Context) api.RuntimeList {
	d.refreshSessionRegistrations(ctx)
	d.mu.Lock()
	runtimes := make([]*runtime, 0, len(d.runtimes))
	for _, r := range d.runtimes {
		runtimes = append(runtimes, r)
	}
	out := api.RuntimeList{Items: make([]api.Runtime, len(runtimes)), Issues: append([]api.RuntimeDiscoveryIssue{}, d.discoveryIssues...), Complete: true}
	d.mu.Unlock()
	// Never hold the Engine lock over IPC. One bad endpoint cannot block exact
	// lookup/stop of another Runtime, and concurrent lists share this hard bound.
	jobs := make(chan int, len(runtimes))
	for i := range runtimes {
		jobs <- i
	}
	close(jobs)
	var group sync.WaitGroup
	for range min(8, len(runtimes)) {
		group.Go(func() {
			for i := range jobs {
				r := runtimes[i]
				if r.host == nil {
					out.Items[i] = r.info()
					continue
				}
				select {
				case d.discoveryReads <- struct{}{}:
					out.Items[i] = r.host.informationContext(ctx)
					<-d.discoveryReads
				case <-ctx.Done():
					r.host.mu.Lock()
					out.Items[i] = r.host.registration.Runtime
					r.host.mu.Unlock()
					out.Items[i].Availability = "unavailable"
				}
			}
		})
	}
	group.Wait()
	for _, runtime := range out.Items {
		if runtime.Availability == "unavailable" {
			out.Issues = append(out.Issues, api.RuntimeDiscoveryIssue{Runtime: &runtime, Code: "SESSION_UNAVAILABLE"})
		}
	}
	out.Complete = len(out.Issues) == 0 && ctx.Err() == nil
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].ID < out.Items[j].ID })
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
	select {
	case d.starts <- struct{}{}:
		defer func() { <-d.starts }()
	default:
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("profile concurrency limit"))
		return
	}
	var p api.Profile
	if e := wire.Decode(m, &p); e != nil {
		s.Fail("INVALID_ARGUMENT", e)
		return
	}
	if e := p.Validate(); e != nil {
		s.Fail("INVALID_ARGUMENT", e)
		return
	}
	if p.Kind != "environment" {
		s.Fail("INVALID_ARGUMENT", fmt.Errorf("profile.prepare requires kind: environment"))
		return
	}
	hash := requestHash(m)
	d.mu.Lock()
	if attempt := d.attempts[m.RequestId]; attempt != nil {
		d.mu.Unlock()
		if attempt.hash != hash {
			s.Fail("IDEMPOTENCY_CONFLICT", fmt.Errorf("execution ID has different Profile"))
			return
		}
		s.Fail("RESULT_UNKNOWN", fmt.Errorf("Profile execution already admitted; query profile.status"))
		return
	}
	if cached := d.cache[m.RequestId]; cached != nil {
		d.mu.Unlock()
		if cached.hash != hash {
			s.Fail("IDEMPOTENCY_CONFLICT", fmt.Errorf("request ID has different Profile"))
			return
		}
		s.Fail("RESULT_UNKNOWN", fmt.Errorf("profile request already admitted; result unavailable"))
		return
	}
	if !d.cacheRoomLocked() || !d.attemptRoomLocked() {
		d.mu.Unlock()
		s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("Profile attempt capacity reached"))
		return
	}
	d.cache[m.RequestId] = &cached{hash: hash, at: time.Now()}
	progress := api.ProfileProgress{ExecutionID: m.RequestId, Stage: "accepted", Step: -1}
	d.attempts[m.RequestId] = &profileAttempt{hash: hash, status: api.ProfileStatus{ExecutionID: m.RequestId, Kind: p.Kind, State: "running", Progress: progress}, at: time.Now()}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.cache[m.RequestId].result = &pb.Message{Kind: "error", Code: "RESULT_UNKNOWN", Detail: "profile already executed; no stream replay"}
		d.mu.Unlock()
	}()
	_ = s.Send(&pb.Message{Kind: "accepted", RequestId: m.RequestId})
	for i, step := range p.Setup.Steps {
		progress := api.ProfileProgress{ExecutionID: m.RequestId, Stage: "setup_running", Step: i, StepName: step.Name, StepsCompleted: i}
		d.updateProfileAttempt(m.RequestId, "running", progress, nil, nil)
		_ = s.Send(&pb.Message{Kind: "progress", RequestId: m.RequestId, Payload: api.Payload(progress)})
		res, e := d.exec(api.Exec{Command: step, WorkingDirectory: p.WorkingDirectory, Env: p.Env})
		if e != nil || res.ExitCode != 0 || res.TimedOut {
			progress, failure := profileSetupFailure(m.RequestId, p.Kind, i, step.Name, res, e)
			d.updateProfileAttempt(m.RequestId, "failed", progress, nil, failure)
			_ = s.Send(profileFailureMessage(m.RequestId, progress, failure))
			return
		}
		stepResult := res
		progress = api.ProfileProgress{ExecutionID: m.RequestId, Stage: "setup_completed", Step: i, StepName: step.Name, StepsCompleted: i + 1, StepResult: &stepResult}
		d.updateProfileAttempt(m.RequestId, "running", progress, nil, nil)
		_ = s.Send(&pb.Message{Kind: "progress", RequestId: m.RequestId, Payload: api.Payload(progress)})
	}
	result := &api.ProfileResult{Kind: p.Kind, Stage: "succeeded", StepsCompleted: len(p.Setup.Steps)}
	progress = api.ProfileProgress{ExecutionID: m.RequestId, Stage: "succeeded", Step: -1, StepsCompleted: result.StepsCompleted}
	d.updateProfileAttempt(m.RequestId, "succeeded", progress, result, nil)
	_ = s.Send(&pb.Message{Kind: "result", RequestId: m.RequestId, Payload: api.Payload(result)})
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

func (d *Engine) createAgent(p api.Profile, machine string, r *runtime, launch api.SubmissionReceipt) error {
	argv, _ := p.Start.Args()
	if p.Adapter == "pty" {
		session, err := d.tmux.Create(r.info(), argv, environment(p.Env), tmux.CreateOptions{HistoryLines: p.HistoryLines, Timeout: time.Duration(p.Start.TimeoutSeconds) * time.Second, NativeAgent: agentintegration.Agent(p.Start.Argv), RequireMCP: p.RequireAgentMCP})
		if err != nil {
			return err
		}
		r.tmux = session
		state, err := session.TimeoutState()
		if err != nil {
			_ = session.Destroy()
			return err
		}
		r.readTimeoutState(state)
		return nil
	}
	return d.launchSession(p, machine, r, launch)
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
	if a.Conversation && (r.acp == nil || !a.Observe) {
		s.Fail("UNSUPPORTED", fmt.Errorf("conversation subscription requires managed ACP observation"))
		return
	}
	if r.adapter == "acp" && !a.Observe {
		s.Fail("UNSUPPORTED", fmt.Errorf("ACP input requires a caller-owned submission"))
		return
	}
	sub, e := r.subscribe(!a.Observe, a.Conversation)
	if e != nil {
		code := "INPUT_OWNED"
		if ae, ok := e.(*api.Error); ok {
			code = ae.Code
		}
		s.Fail(code, e)
		return
	}
	_, epoch := r.control.state(sub)
	if s.Send(&pb.Message{Kind: "accepted", Payload: api.Payload(r.info()), ControlEpoch: epoch}) != nil {
		r.unsubscribe(sub)
		sub.once.Do(func() { close(sub.failed) })
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
	if r.raw != nil {
		if s.Send(&pb.Message{Kind: "raw_acp_state", Payload: api.Payload(r.raw.state())}) != nil {
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
			if !sub.canInput {
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
				if r.adapter == "acp" {
					done <- fmt.Errorf("ACP input requires a caller-owned submission")
					return
				}
				if len(m.Data) > wire.ChunkSize {
					e = fmt.Errorf("input exceeds chunk limit")
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
		case <-sub.changed:
			if change := sub.takeConversationChange(); change != nil {
				if e := s.Send(change); e != nil {
					return
				}
			}
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
