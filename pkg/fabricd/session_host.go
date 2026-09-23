package fabricd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/buildinfo"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/retainedprogram"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

type sessionHello struct {
	Registration sessionRegistration `json:"registration"`
	Term         uint64              `json:"term"`
	Connector    string              `json:"connector"`
	Probe        bool                `json:"probe,omitempty"`
}

type sessionControl struct {
	mu        sync.RWMutex
	term      uint64
	connector string
	current   *yamux.Session
}

func sameSession(a, b sessionRegistration) bool {
	return a.Target == b.Target && a.Version == b.Version && a.Installation == b.Installation && a.Machine == b.Machine && a.Instance == b.Instance && a.Program == b.Program && a.Runtime.ID == b.Runtime.ID && a.Runtime.Incarnation == b.Runtime.Incarnation && a.Runtime.Generation == b.Runtime.Generation
}

// runSessionHost is the only owner of the existing Runtime/controller. Its
// lifetime is independent of every connector and subscription that visits it.
func runSessionHost(directory string) error {
	return runSessionHostWithRawWriter(directory, nil)
}

func runSessionHostWithRawWriter(directory string, wrap func(io.Writer) io.Writer) (resultErr error) {
	// fd3 was opened before stdio was detached. Only this host owns the terminal
	// reference: close-on-exec excludes guardians/Agents and their private fd3/4.
	syscall.CloseOnExec(3)
	defer syscall.Close(3)
	stateDir := filepath.Dir(filepath.Dir(filepath.Dir(directory)))
	startup, err := sessionregistry.Open(context.Background(), filepath.Join(stateDir, "registry"), sessionregistry.Options{})
	if err != nil {
		return err
	}
	defer startup.Close()
	bootID, err := process.BootID()
	if err != nil {
		return err
	}
	prepared, err := startup.EnterHost(context.Background(), directory, bootID, os.Getpid())
	if err != nil {
		return err
	}
	failureCode := "HOST_VALIDATION_FAILED"
	defer func() {
		if resultErr == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = startup.FailHost(ctx, prepared.Target, prepared.Instance, failureCode, func(host sessionregistry.HostRecord) error {
			if host.PID != os.Getpid() || host.BootID != bootID {
				return fmt.Errorf("original host ownership changed")
			}
			if host.GroupID != 0 && !errors.Is(syscall.Kill(-host.GroupID, 0), syscall.ESRCH) {
				return fmt.Errorf("Agent group exit is unconfirmed")
			}
			return nil
		})
	}()
	if err := startup.StartupProgress(context.Background(), prepared.Target, prepared.Instance, "host_validation", false); err != nil {
		return err
	}

	if identity, err := fileIdentity(directory, os.ModeDir); err != nil || identity != prepared.Resources.Directory {
		return fmt.Errorf("original Runtime directory identity changed")
	}
	var boot sessionBootstrap
	path := filepath.Join(directory, "bootstrap.json")
	if err := privateFile(path, 2*wire.MaxMessage, &boot); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	reg := boot.Registration
	var expected sessionRegistration
	if json.Unmarshal(prepared.Registration, &expected) != nil || !sameSession(reg, expected) || boot.StateDir != stateDir {
		return fmt.Errorf("bootstrap differs from original prepared host")
	}
	program := filepath.Join(directory, "program")
	if err := retainedprogram.VerifyExecuting(program, reg.Program); err != nil {
		return err
	}
	if filepath.Base(directory) != reg.Runtime.ID || reg.Installation != installationID(boot.StateDir) || boot.Profile.Adapter != "acp" {
		return fmt.Errorf("ACP bootstrap identity does not match its installation")
	}
	failureCode = "HOST_START_FAILED"
	socket, err := sessionSocket(reg)
	if err != nil {
		return err
	}
	if err := boot.Profile.Validate(); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	// Retirement owns this pathname through its recorded inode. A late host
	// destructor must not unlink a resource that has since reused the path.
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	d := newEngine(ctx)
	defer d.Close()
	d.registry, err = sessionregistry.Open(ctx, filepath.Join(boot.StateDir, "registry"), sessionregistry.Options{})
	if err != nil {
		return err
	}
	d.events, err = lifecycle.Open(directory, "host-events.jsonl")
	if err != nil {
		return err
	}
	r := &runtime{target: reg.Target, id: reg.Runtime.ID, inc: reg.Runtime.Incarnation, adapter: "acp", title: reg.Runtime.Title, cwd: boot.Profile.WorkingDirectory, projectID: boot.Profile.ProjectID, directoryID: boot.Profile.DirectoryID, subs: map[*subscription]bool{}, done: make(chan struct{}), conversations: d.conversations}
	r.observer = d.publishRuntime
	hostStarted := time.Now().UTC()
	build := buildinfo.Current()
	r.events = d.events
	defer d.recordLifecycle("host_exit", r, "", "", 0)
	r.hostInfo = &api.ACPHostInfo{Build: &build, Protocol: reg.Version, Instance: reg.Instance, ProgramSHA256: reg.Program.SHA256, ProgramBytes: reg.Program.Bytes, HostPID: os.Getpid(), StartedAt: &hostStarted, Startup: &api.ACPStartupDiagnostic{Phase: "host_validation"}}
	reserved := reg.Runtime
	reserved.State = "starting"
	reserved.ACPHost = r.hostInfo
	resources, err := captureSessionResources(directory, socket, reg)
	if err != nil {
		return err
	}
	if err := d.registry.RegisterHost(ctx, sessionregistry.HostRecord{Target: reg.Target, Instance: reg.Instance, BootID: bootID, PID: os.Getpid(), Runtime: reserved, Registration: api.Payload(reg), Resources: resources}); err != nil {
		return err
	}
	argv, _ := boot.Profile.Start.Args()
	var processGeneration uint64
	r.acpStart = func() (*process.Process, error) {
		if err := retainedprogram.Verify(program, reg.Program); err != nil {
			return nil, err
		}
		return process.StartRegistered(argv, r.cwd, boot.Environment, func(group int) error {
			generation, err := d.registry.RecordGroup(ctx, reg.Target, reg.Instance, processGeneration, group)
			if err == nil {
				processGeneration = generation
			}
			return err
		})
	}
	failureCode = "AGENT_START_FAILED"
	setStartupPhase := func(phase string) error {
		r.mu.Lock()
		r.hostInfo.Startup = &api.ACPStartupDiagnostic{Phase: phase}
		r.mu.Unlock()
		return startup.StartupProgress(ctx, reg.Target, reg.Instance, phase, false)
	}
	if err := setStartupPhase("agent_start"); err != nil {
		return err
	}
	r.p, err = r.acpStart()
	if err != nil {
		return err
	}
	// Register local ownership immediately so every later error closes the
	// guardian and waits for its complete process group before failure publication.
	d.runtimes[r.id] = r
	if boot.Profile.ManagedACP {
		r.acp = newACPController(r)
		r.acp.v2Draft = boot.Profile.ACPV2Draft
		r.acp.elicitationEnabled = boot.Profile.ACPElicitation
		r.acp.requireMCP = boot.Profile.RequireAgentMCP
		r.acp.reserveControl = func(kind, id string) error { return d.registry.ReserveControl(ctx, reg.Target, kind, id) }
		r.acp.releaseControl = func(kind, id string) { _ = d.registry.ReleaseControl(ctx, reg.Target, kind, id) }
	} else {
		var writer io.Writer = r.p.Input
		if wrap != nil {
			writer = wrap(writer)
		}
		r.raw = newRawACP(ctx, d.registry, "acp:"+r.inc, writer)
		r.raw.stdout.onGap = func(count uint64) {
			d.events.Record(lifecycle.Entry{Kind: "stdout_gap", RuntimeID: r.id, RuntimeIncarnation: r.inc, HostInstance: reg.Instance, Count: count})
		}
		r.raw.stderr.onGap = func(count uint64) {
			d.events.Record(lifecycle.Entry{Kind: "stderr_gap", RuntimeID: r.id, RuntimeIncarnation: r.inc, HostInstance: reg.Instance, Count: count})
		}
	}
	now := time.Now().UTC()
	r.startedAt = &now
	if boot.Profile.Start.TimeoutSeconds > 0 {
		deadline := now.Add(time.Duration(boot.Profile.Start.TimeoutSeconds) * time.Second)
		r.deadlineAt = &deadline
	}
	d.runtimes[r.id] = r
	if err := d.registry.RecordHostRuntime(ctx, reg.Target, reg.Instance, r.info()); err != nil {
		return err
	}
	r.runProcess(r.p)
	d.recordLifecycle("host_started", r, boot.Launch.OperationRef, "", 0)
	if r.acp != nil {
		failureCode = "AGENT_INITIALIZATION_FAILED"
		if err := setStartupPhase("agent_initialization"); err != nil {
			return err
		}
		if err := r.acp.initializeConnection(); err != nil {
			return err
		}
	}
	failureCode = "HOST_START_FAILED"
	if err := setStartupPhase("ready"); err != nil {
		return err
	}
	reg.Runtime = r.info()
	if err := d.registry.RecordHostRuntime(ctx, reg.Target, reg.Instance, reg.Runtime); err != nil {
		return err
	}
	if err := savePrivateFile(filepath.Join(directory, "registration.json"), reg); err != nil {
		return err
	}
	// Losing a progress acknowledgement cannot destroy an already started Agent.
	_, _ = d.registry.Progress(ctx, boot.Launch.SubmissionKey, boot.Launch.OperationRef, "started", "", &reg.Runtime)
	go func() {
		<-r.done
		d.recordLifecycle("agent_exit", r, "", r.info().StopReason, 0)
		terminal := reg
		terminal.Runtime = r.info()
		_ = d.registry.RecordHostRuntime(ctx, reg.Target, reg.Instance, terminal.Runtime)
		_ = savePrivateFile(filepath.Join(directory, "registration.json"), terminal)
	}()
	if r.deadlineAt != nil {
		go func(deadline time.Time) {
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			select {
			case <-r.done:
			case <-ctx.Done():
			case <-timer.C:
				r.mu.Lock()
				r.stopReason = "timeout"
				r.mu.Unlock()
				_ = r.stop()
			}
		}(*r.deadlineAt)
	}
	control := &sessionControl{}
	go func() { <-ctx.Done(); listener.Close() }()
	connections := make(chan struct{}, 8)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case connections <- struct{}{}:
			d.mu.Lock()
			if d.ctx.Err() != nil {
				d.mu.Unlock()
				<-connections
				conn.Close()
				continue
			}
			d.active.Add(1)
			d.mu.Unlock()
			go func() {
				defer d.active.Done()
				defer func() { <-connections }()
				serveSessionConnection(ctx, conn, d, r, reg, control)
			}()
		default:
			conn.Close()
		}
	}
}

func serveSessionConnection(ctx context.Context, conn net.Conn, d *Engine, r *runtime, reg sessionRegistration, control *sessionControl) {
	defer conn.Close()
	sess, err := yamux.Server(conn, wire.Config())
	if err != nil {
		return
	}
	defer sess.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var requests sync.WaitGroup
	defer func() { cancel(); sess.Close(); requests.Wait() }()
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	// An unauthenticated local peer cannot retain a handshake slot forever.
	timer := time.AfterFunc(5*time.Second, func() { sess.Close() })
	defer timer.Stop()
	raw, err := sess.AcceptStream()
	if err != nil {
		return
	}
	ctrl := wire.Wrap(raw)
	defer ctrl.Close()
	m, err := ctrl.Recv()
	var hello sessionHello
	if err != nil || m.Kind != "host.connect" || wire.Decode(m, &hello) != nil || !sameSession(hello.Registration, reg) {
		code := "HOST_IDENTITY_MISMATCH"
		if hello.Registration.Version != reg.Version {
			code = "SESSION_PROTOCOL_UNSUPPORTED"
		}
		d.recordLifecycle("handshake_rejected", r, "", code, 0)
		ctrl.Fail("HOST_IDENTITY_MISMATCH", fmt.Errorf("ACP host identity does not match registration"))
		return
	}
	if !hello.Probe {
		control.mu.Lock()
		if hello.Term == 0 || !wire.ValidID(hello.Connector) || hello.Term < control.term || (hello.Term == control.term && hello.Connector != control.connector) {
			d.recordLifecycle("control_rejected", r, "", "STALE_CONTROL", hello.Term)
			control.mu.Unlock()
			ctrl.Fail("STALE_CONTROL", fmt.Errorf("connector control term is obsolete"))
			return
		}
		old := control.current
		if hello.Term > control.term {
			d.recordLifecycle("control_takeover", r, "", "", hello.Term)
		}
		control.term, control.connector, control.current = hello.Term, hello.Connector, sess
		d.recordLifecycle("attach", r, "", "", hello.Term)
		r.mu.Lock()
		if r.hostInfo != nil {
			now := time.Now().UTC()
			r.hostInfo.Connected, r.hostInfo.ConnectorTerm, r.hostInfo.LastAttachedAt = true, hello.Term, &now
		}
		r.mu.Unlock()
		control.mu.Unlock()
		defer func() {
			d.recordLifecycle("detach", r, "", "", hello.Term)
			control.mu.Lock()
			defer control.mu.Unlock()
			if control.current == sess {
				r.mu.Lock()
				if r.hostInfo != nil {
					r.hostInfo.Connected = false
				}
				r.mu.Unlock()
			}
		}()
		if old != nil {
			old.Close()
		}
	}
	current := reg
	current.Runtime = r.info()
	if ctrl.Send(&pb.Message{Kind: "host.ready", Payload: api.Payload(current)}) != nil || hello.Probe {
		return
	}
	timer.Stop()
	go func() { _, _ = ctrl.Recv(); cancel() }()
	for {
		raw, err := sess.AcceptStream()
		if err != nil {
			return
		}
		if lease := d.streams.Acquire(wire.StreamOpening); lease != nil {
			requests.Add(1)
			go func() {
				defer requests.Done()
				defer lease.Release()
				s := &executionStream{Stream: wire.Wrap(raw), ctx: ctx, engine: d}
				defer s.Close()
				check := func(m *pb.Message) error {
					if control.current != sess || ctx.Err() != nil || m.Incarnation != hello.Connector || m.ConnectionGeneration != hello.Term || m.Target != reg.Machine || m.RuntimeId != r.id || m.RuntimeIncarnation != r.inc || m.RuntimeGeneration != 1 {
						return &api.Error{Code: "STALE_CONTROL", Detail: "ACP host control or Runtime identity changed"}
					}
					return nil
				}
				s.receiveCheck = func(m *pb.Message) error {
					control.mu.RLock()
					defer control.mu.RUnlock()
					return check(m)
				}
				_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
				message, err := s.Recv()
				_ = s.SetReadDeadline(time.Time{})
				if err != nil {
					return
				}
				if message.Kind != "request" || !sessionOperation(message.Operation) || message.RequestId == "" || len(message.RequestId) > 128 {
					s.Fail("UNSUPPORTED", fmt.Errorf("operation is outside the ACP host contract"))
					return
				}
				if class := wire.RequestClass(message); !lease.Move(class) {
					s.Fail("RESOURCE_EXHAUSTED", fmt.Errorf("ACP host %s stream capacity exhausted", class))
					return
				}
				if sessionMutation(message.Operation) {
					// Serialize takeover with validation and durable queue admission.
					// An operation admitted first continues after this lock releases.
					control.mu.RLock()
					defer control.mu.RUnlock()
					if err := check(message); err != nil {
						s.Fail("STALE_CONTROL", err)
						return
					}
				}
				d.dispatch(s, message, reg.Machine)
			}()
		} else {
			raw.Close()
		}
	}
}

func sessionOperation(operation string) bool {
	switch operation {
	case "submission.acp", "submission.raw", "acp.raw.state", "acp.raw.read", "acp.state", "acp.conversation.read", "acp.conversation.get", "agent.mcp.configure", "agent.operation.wait", "agent.operation.read", "runtime.attach", "runtime.get", "runtime.watch", "runtime.stop":
		return true
	default:
		return false
	}
}

func sessionMutation(operation string) bool {
	switch operation {
	case "submission.acp", "submission.raw", "agent.mcp.configure", "runtime.stop":
		return true
	default:
		return false
	}
}
