package fabricd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/tmux"
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
	return a.Target == b.Target && a.Version == b.Version && a.Installation == b.Installation && a.Machine == b.Machine && a.Instance == b.Instance && a.Runtime.ID == b.Runtime.ID && a.Runtime.Incarnation == b.Runtime.Incarnation && a.Runtime.Generation == b.Runtime.Generation
}

// runSessionHost is the only owner of the existing Runtime/controller. Its
// lifetime is independent of every connector and subscription that visits it.
func runSessionHost(directory string) error {
	if err := tmux.PrivateDir(directory); err != nil {
		return err
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
	if filepath.Base(directory) != reg.Runtime.ID || reg.Installation != installationID(boot.StateDir) || !boot.Profile.ManagedACP || boot.Profile.Adapter != "acp" {
		return fmt.Errorf("ACP bootstrap identity does not match its installation")
	}
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
	defer os.Remove(socket)
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
	r := &runtime{target: reg.Target, id: reg.Runtime.ID, inc: reg.Runtime.Incarnation, adapter: "acp", title: reg.Runtime.Title, cwd: boot.Profile.WorkingDirectory, projectID: boot.Profile.ProjectID, directoryID: boot.Profile.DirectoryID, subs: map[*subscription]bool{}, done: make(chan struct{}), conversations: d.conversations}
	argv, _ := boot.Profile.Start.Args()
	r.acpStart = func() (*process.Process, error) { return process.Start(argv, r.cwd, boot.Environment) }
	r.p, err = r.acpStart()
	if err != nil {
		return err
	}
	r.acp = newACPController(r)
	r.acp.requireMCP = boot.Profile.RequireAgentMCP
	r.acp.reserveControl = func(kind, id string) error { return d.registry.ReserveControl(ctx, reg.Target, kind, id) }
	r.acp.releaseControl = func(kind, id string) { _ = d.registry.ReleaseControl(ctx, reg.Target, kind, id) }
	now := time.Now().UTC()
	r.startedAt = &now
	if boot.Profile.Start.TimeoutSeconds > 0 {
		deadline := now.Add(time.Duration(boot.Profile.Start.TimeoutSeconds) * time.Second)
		r.deadlineAt = &deadline
	}
	d.runtimes[r.id] = r
	r.runProcess(r.p)
	go r.acp.initialize()
	reg.Runtime = r.info()
	if err := savePrivateFile(filepath.Join(directory, "registration.json"), reg); err != nil {
		return err
	}
	// Losing a progress acknowledgement cannot destroy an already started Agent.
	_, _ = d.registry.Progress(ctx, boot.Launch.SubmissionKey, boot.Launch.OperationRef, "started", "", &reg.Runtime)
	go func() {
		<-r.done
		terminal := reg
		terminal.Runtime = r.info()
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
			go func() {
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
		ctrl.Fail("HOST_IDENTITY_MISMATCH", fmt.Errorf("ACP host identity does not match registration"))
		return
	}
	if !hello.Probe {
		control.mu.Lock()
		if hello.Term == 0 || !wire.ValidID(hello.Connector) || hello.Term < control.term || (hello.Term == control.term && hello.Connector != control.connector) {
			control.mu.Unlock()
			ctrl.Fail("STALE_CONTROL", fmt.Errorf("connector control term is obsolete"))
			return
		}
		old := control.current
		control.term, control.connector, control.current = hello.Term, hello.Connector, sess
		control.mu.Unlock()
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
	sem := make(chan struct{}, 64)
	for {
		raw, err := sess.AcceptStream()
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
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
		default:
			raw.Close()
		}
	}
}

func sessionOperation(operation string) bool {
	switch operation {
	case "submission.acp", "acp.state", "acp.conversation.read", "acp.conversation.get", "agent.mcp.configure", "agent.operation.wait", "agent.operation.read", "runtime.attach", "runtime.get", "runtime.stop":
		return true
	default:
		return false
	}
}

func sessionMutation(operation string) bool {
	switch operation {
	case "submission.acp", "agent.mcp.configure", "runtime.stop":
		return true
	default:
		return false
	}
}
