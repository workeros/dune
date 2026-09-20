package fabricd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
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
	"google.golang.org/protobuf/proto"
)

// sessionProxy owns only an IPC connection and last observed description. It
// must never construct a controller, run initialize or restart an Agent.
type sessionProxy struct {
	mu           sync.Mutex
	registration sessionRegistration
	directory    string
	term         uint64
	connector    string
	connection   *yamux.Session
	control      *wire.Stream
	registry     *sessionregistry.Registry
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
	p.connection, p.control = sess, ctrl
	p.registration.Runtime = observed.Runtime
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
	p.mu.Lock()
	request := p.request("runtime.get", struct{}{})
	last := p.registration.Runtime
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := p.open(ctx, request)
	if err == nil {
		defer stream.Close()
		_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
		var m *pb.Message
		m, err = stream.Recv()
		if err == nil && m.Kind == "accepted" {
			m, err = stream.Recv()
		}
		if err == nil && m.Kind == "result" {
			var current api.Runtime
			if wire.Decode(m, &current) == nil && current.ID == last.ID && current.Incarnation == last.Incarnation && current.Generation == last.Generation {
				p.mu.Lock()
				p.registration.Runtime = current
				p.mu.Unlock()
				return current
			}
		}
	}
	// A failed probe is not a confirmed process exit.
	last.Availability = "unavailable"
	if p.lossProven() {
		last.State, last.Availability, last.StopReason, last.ExitCode = "lost", "lost", "host_lost", nil
	}
	return last
}

func (p *sessionProxy) lossProven() bool {
	if p.registry == nil {
		return false
	}
	p.mu.Lock()
	reg := p.registration
	p.mu.Unlock()
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
	reg := sessionRegistration{Target: target, Version: sessionProtocol, Installation: installationID(d.stateDir), Machine: machine, Instance: wire.ID(), Runtime: r.info()}
	if err := savePrivateFile(filepath.Join(directory, "instance.json"), reg.Instance); err != nil {
		return err
	}
	boot := sessionBootstrap{Launch: launch, Registration: reg, StateDir: d.stateDir, Profile: p, Environment: environment(p.Env)}
	if err := savePrivateFile(filepath.Join(directory, "bootstrap.json"), boot); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := d.acpTmux.CreateHost(r.id, reg.Instance, executable, directory); err != nil {
		return err
	}
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
		if time.Now().After(deadline) || d.ctx.Err() != nil {
			return &api.Error{Code: "RESULT_UNKNOWN", Detail: "ACP host launch was not confirmed; discover the original Runtime"}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (d *Engine) discoverSessions() error {
	root := filepath.Join(d.stateDir, "acp", "runtimes")
	if err := tmux.PrivateDir(root); err != nil {
		return err
	}
	hosts, err := d.registry.Hosts(d.ctx)
	if err != nil {
		return err
	}
	for _, host := range hosts {
		var reg sessionRegistration
		if json.Unmarshal(host.Registration, &reg) != nil || reg.Installation != installationID(d.stateDir) || reg.Target != host.Target || reg.Instance != host.Instance {
			return fmt.Errorf("registered ACP host identity could not be verified")
		}
		if _, err := sessionSocket(reg); err != nil {
			return err
		}
		reg.Runtime = host.Runtime
		directory := filepath.Join(root, reg.Runtime.ID)
		d.runtimes[reg.Runtime.ID] = &runtime{id: reg.Runtime.ID, inc: reg.Runtime.Incarnation, adapter: "acp", host: &sessionProxy{registration: reg, directory: directory, term: d.sessionTerm, connector: d.inc, registry: d.registry}}
	}
	return nil
}
