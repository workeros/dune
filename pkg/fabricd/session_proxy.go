package fabricd

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/retainedprogram"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
	"google.golang.org/protobuf/proto"
)

// sessionProxy owns only an IPC connection and last observed description. It
// must never construct a controller, run initialize or restart an Agent.
type sessionProxy struct {
	mu            sync.Mutex
	registration  sessionRegistration
	directory     string
	term          uint64
	connector     string
	connection    *yamux.Session
	control       *wire.Stream
	registry      *sessionregistry.Registry
	probeMu       sync.Mutex
	observations  hostObservations
	nextProbe     time.Time
	probeFailures int
}

func (p *sessionProxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.connection != nil {
		p.connection.Close()
	}
}

func (p *sessionProxy) connect(ctx context.Context) error {
	if p.connection != nil && !p.connection.IsClosed() {
		return nil
	}
	socket, err := sessionSocket(p.registration)
	if err != nil {
		return err
	}
	st, err := os.Lstat(socket)
	if err != nil {
		return err
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if st.Mode()&os.ModeSocket == 0 || st.Mode().Perm()&0077 != 0 || !ok || int(owner.Uid) != os.Getuid() {
		return fmt.Errorf("unsafe ACP host socket")
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	sess, err := yamux.Client(conn, wire.Config())
	if err != nil {
		conn.Close()
		return err
	}
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	raw, err := sess.OpenStream()
	if err != nil {
		sess.Close()
		return err
	}
	ctrl := wire.Wrap(raw)
	_ = ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
	err = ctrl.Send(&pb.Message{Kind: "host.connect", Payload: api.Payload(sessionHello{Registration: p.registration, Term: p.term, Connector: p.connector})})
	var response *pb.Message
	var observed sessionRegistration
	if err == nil {
		response, err = ctrl.Recv()
	}
	if err == nil {
		err = wire.Error(response)
	}
	if err == nil && (response.Kind != "host.ready" || wire.Decode(response, &observed) != nil || !sameSession(observed, p.registration)) {
		err = fmt.Errorf("ACP host handshake identity mismatch")
	}
	if err != nil {
		sess.Close()
		return err
	}
	_ = ctrl.SetReadDeadline(time.Time{})
	if _, err := p.observations.confirm(observed.Runtime); err != nil {
		sess.Close()
		return err
	}
	p.connection, p.control = sess, ctrl
	return nil
}

func (p *sessionProxy) open(ctx context.Context, request *pb.Message) (*wire.Stream, error) {
	p.mu.Lock()
	if err := p.connect(ctx); err != nil {
		p.mu.Unlock()
		return nil, &api.Error{Code: "SESSION_UNAVAILABLE", Detail: "ACP host could not be verified: " + err.Error()}
	}
	connection, connector, term := p.connection, p.connector, p.term
	p.mu.Unlock()
	// A slow ordinary write must not hold the connection mutex while stop or
	// another reserved control opens its own stream. Never redial/replay here.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := connection.OpenStream()
	if err != nil {
		return nil, err
	}
	stream := wire.Wrap(raw)
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	message := proto.Clone(request).(*pb.Message)
	message.Incarnation, message.ConnectionGeneration = connector, term
	message.RouteEpoch, message.InputLeaseId = 0, ""
	if err := stream.Send(message); err != nil {
		stream.Close()
		return nil, &api.Error{Code: "RESULT_UNKNOWN", Detail: "ACP host request delivery was not confirmed"}
	}
	return stream, nil
}

func (p *sessionProxy) request(operation string, payload any) *pb.Message {
	reg := p.registration
	return &pb.Message{Kind: "request", RequestId: wire.ID(), Operation: operation, Target: reg.Machine, RuntimeId: reg.Runtime.ID, RuntimeIncarnation: reg.Runtime.Incarnation, RuntimeGeneration: reg.Runtime.Generation, Payload: api.Payload(payload)}
}

func (p *sessionProxy) information() api.Runtime {
	return p.informationContext(context.Background())
}

func (p *sessionProxy) informationContext(parent context.Context) api.Runtime {
	if p.registry != nil {
		target, instance := p.registration.Target, p.registration.Instance
		if host, err := p.registry.Host(parent, target); err == nil && host.Instance == instance && host.Phase == "failed" {
			return p.observations.failed(host.Runtime)
		}
	}
	// Coalesce concurrent probes before entering the connection mutex. Repeated
	// reads of one bad endpoint cannot occupy every discovery worker in sequence.
	if !p.probeMu.TryLock() {
		return p.lastObservation()
	}
	defer p.probeMu.Unlock()
	if time.Now().Before(p.nextProbe) {
		return p.lastObservation()
	}
	request := p.request("runtime.get", struct{}{})
	last := p.lastObservation()
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	stream, err := p.open(ctx, request)
	if err == nil {
		defer stream.Close()
		stop := context.AfterFunc(ctx, func() { stream.Close() })
		defer stop()
		_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
		var m *pb.Message
		m, err = stream.Recv()
		if err == nil && m.Kind == "accepted" {
			m, err = stream.Recv()
		}
		if err == nil && m.Kind == "result" {
			var current api.Runtime
			if wire.Decode(m, &current) == nil && current.ID == last.ID && current.Incarnation == last.Incarnation && current.Generation == last.Generation {
				observed, confirmErr := p.observations.confirm(current)
				if confirmErr == nil {
					p.nextProbe, p.probeFailures = time.Time{}, 0
					return observed
				}
			}
		}
	}
	// A failed probe is not a confirmed process exit.
	p.probeFailures++
	delay := 250 * time.Millisecond * time.Duration(1<<min(p.probeFailures-1, 5))
	p.nextProbe = time.Now().Add(delay + time.Duration(rand.Int64N(int64(delay/2))))
	return p.observations.unavailable(last.Observation, p.lossProven())
}

func (p *sessionProxy) lossProven() bool {
	if p.registry == nil {
		return false
	}
	reg := p.registration
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	record, err := p.registry.Host(ctx, reg.Target)
	if err != nil || record.Instance != reg.Instance {
		return false
	}
	absent, err := process.Absent(record.BootID, record.PID, record.GroupID)
	return err == nil && absent
}

func (p *sessionProxy) forward(s *executionStream, request *pb.Message, skipAccepted bool) {
	stream, err := p.open(s.ctx, request)
	if err != nil {
		code := "SESSION_UNAVAILABLE"
		var failure *api.Error
		if errors.As(err, &failure) {
			code = failure.Code
		}
		if p.lossProven() {
			code, err = "SESSION_LOST", errors.New("original ACP host and process group no longer exist")
		}
		s.Fail(code, err)
		return
	}
	defer stream.Close()
	stop := context.AfterFunc(s.ctx, func() { stream.Close() })
	defer stop()
	// Managed subscriptions carry no protocol input. The connector consumes
	// disconnects so an absent observer does not occupy a host slot indefinitely.
	if request.Operation == "runtime.attach" {
		go func() { _, _ = s.Recv(); stream.Close() }()
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			s.Fail("RESULT_UNKNOWN", fmt.Errorf("ACP host connection interrupted"))
			return
		}
		if skipAccepted && message.Kind == "accepted" {
			skipAccepted = false
			continue
		}
		if s.Send(message) != nil {
			return
		}
		if message.Kind == "result" || message.Kind == "error" || message.Kind == "exit" {
			return
		}
	}
}

func (d *Engine) launchSession(p api.Profile, machine string, r *runtime, launch api.SubmissionReceipt) error {
	directory := filepath.Join(d.stateDir, "acp", "runtimes", r.id)
	if err := tmux.PrivateDir(filepath.Dir(directory)); err != nil {
		return err
	}
	// Never overwrite the marker or bootstrap of an existing resource path.
	if err := os.Mkdir(directory, 0700); err != nil {
		return err
	}
	target := launch.Target
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = r.id, r.inc, 1
	description := r.info()
	description.PersistentACP, description.ACPMode = true, "raw"
	if p.ManagedACP {
		description.ACPMode = "managed"
	}
	executable := filepath.Join(directory, "program")
	program, err := retainedprogram.Current(executable)
	if err != nil {
		return err
	}
	reg := sessionRegistration{Target: target, Version: sessionProtocol, Installation: installationID(d.stateDir), Machine: machine, Instance: wire.ID(), Runtime: description, Program: program}
	if err := savePrivateFile(filepath.Join(directory, "instance.json"), reg.Instance); err != nil {
		return err
	}
	socket, err := sessionSocketPath(reg)
	if err != nil {
		return err
	}
	resource, err := fileIdentity(directory, os.ModeDir)
	if err != nil {
		return err
	}
	bootID, err := process.BootID()
	if err != nil {
		return err
	}
	description.State = "starting"
	description.ACPHost = &api.ACPHostInfo{Protocol: reg.Version, Instance: reg.Instance, ProgramSHA256: program.SHA256, ProgramBytes: program.Bytes, Startup: &api.ACPStartupDiagnostic{Phase: "host_pending"}}
	prepared := sessionregistry.HostRecord{Target: target, Instance: reg.Instance, BootID: bootID, Runtime: description, Registration: api.Payload(reg), Resources: sessionregistry.CleanupResources{Directory: resource, Socket: sessionregistry.FileIdentity{Path: socket}, Instance: reg.Instance, TmuxSession: "acp-" + r.id}}
	if err := d.registry.PrepareHost(d.ctx, prepared); err != nil {
		return err
	}
	boot := sessionBootstrap{Launch: launch, Registration: reg, StateDir: d.stateDir, Profile: p, Environment: environment(p.Env)}
	if err := savePrivateFile(filepath.Join(directory, "bootstrap.json"), boot); err != nil {
		// No tmux launch was attempted, and the gate seals any copied bootstrap.
		failureErr := d.registry.FailHost(d.ctx, target, reg.Instance, "HOST_START_FAILED", func(host sessionregistry.HostRecord) error {
			if host.Phase != "starting" || host.PID != 0 {
				return fmt.Errorf("host already entered")
			}
			return nil
		})
		if failureErr != nil {
			return &api.Error{Code: "RESULT_UNKNOWN", Detail: "ACP bootstrap failure could not be durably confirmed; query the original key"}
		}
		return &api.Error{Code: "HOST_START_FAILED", Detail: "ACP bootstrap publication failed"}
	}
	pid, launchErr := d.acpTmux.CreateHost(r.id, reg.Instance, executable, directory)
	if launchErr == nil {
		_ = d.registry.ObserveHostPID(d.ctx, target, reg.Instance, pid)
	}
	// A tmux client error can lose an acknowledgement after process creation.
	// Continue original-endpoint probes; never repeat new-session.

	r.host = &sessionProxy{registration: reg, directory: directory, term: d.sessionTerm, connector: d.inc, registry: d.registry}
	// No retry can launch another host. These bounded probes only establish the
	// original endpoint after tmux's asynchronous child startup.
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
		r.host.mu.Lock()
		err = r.host.connect(ctx)
		r.host.mu.Unlock()
		cancel()
		if err == nil {
			return nil
		}
		if host, readErr := d.registry.Host(d.ctx, target); readErr == nil {
			d.confirmAbsentStartup(host)
			if receipt, readErr := d.registry.Get(d.ctx, launch.SubmissionKey); readErr == nil && receipt.Stage == "failed" {
				return &api.Error{Code: receipt.ErrorCode, Detail: "original ACP startup failed; query the original submission key"}
			}
		}
		if time.Now().After(deadline) || d.ctx.Err() != nil {
			_ = d.registry.StartupProgress(d.ctx, target, reg.Instance, "host_pending", true)
			return &api.Error{Code: "RESULT_UNKNOWN", Detail: "ACP host confirmation timed out; query the original submission key"}
		}
		time.Sleep(100 * time.Millisecond)
	}
}
