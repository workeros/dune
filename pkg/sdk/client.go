// Package sdk is the Go client for Dune. Every call traverses the Gateway.
package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
	"io"
	"net"
	"sync"
)

type Client struct {
	s       *yamux.Session
	control *wire.Stream
	Binding api.Binding
}

// Connect performs the Dune client handshake on an established byte connection.
// It takes ownership of conn, including on error. ctx bounds the handshake;
// Close ends the client lifetime, and individual calls have their own contexts.
func Connect(ctx context.Context, conn net.Conn, target string) (*Client, error) {
	if conn == nil {
		return nil, fmt.Errorf("nil connection")
	}
	if err := ctx.Err(); err != nil {
		conn.Close()
		return nil, err
	}
	s, e := yamux.Client(conn, wire.Config())
	if e != nil {
		conn.Close()
		return nil, e
	}
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	ctrl, m, e := wire.Handshake(s, &pb.Message{Kind: "hello", Target: target, Payload: api.Payload(api.Hello{Version: api.Version, Role: "sdk"})})
	if e != nil {
		s.Close()
		return nil, e
	}
	c := &Client{s: s, control: ctrl}
	if e = wire.Decode(m, &c.Binding); e != nil {
		s.Close()
		return nil, e
	}
	go func() { _, _ = ctrl.Recv(); s.Close() }()
	return c, nil
}
func (c *Client) Close() error { return c.s.Close() }

// Stream owns one business subscription. EOF before result/exit is interruption.
// Recv has one reader. Send/Input may be called concurrently with it.
type Stream struct {
	s        *wire.Stream
	cancel   func() bool
	once     sync.Once
	terminal bool
}

func (s *Stream) Close() error { s.once.Do(func() { s.cancel(); s.s.Close() }); return nil }
func (s *Stream) Send(m *pb.Message) error {
	if e := s.s.Send(m); e != nil {
		return &api.Error{Code: "RESULT_UNKNOWN", Detail: e.Error()}
	}
	return nil
}
func (s *Stream) Recv() (*pb.Message, error) {
	m, e := s.s.Recv()
	if e != nil {
		if s.terminal && e == io.EOF {
			return nil, io.EOF
		}
		return nil, &api.Error{Code: "STREAM_INTERRUPTED", Detail: e.Error()}
	}
	if m.Kind == "exit" || m.Kind == "result" {
		s.terminal = true
	}
	if e = wire.Error(m); e != nil {
		return nil, e
	}
	return m, nil
}
func (s *Stream) Input(b []byte) (string, error) {
	id := wire.ID()
	if len(b) > wire.ChunkSize {
		return id, fmt.Errorf("input exceeds 32768 bytes")
	}
	return id, s.Send(&pb.Message{Kind: "input", RequestId: id, Data: b})
}
func (s *Stream) Resize(rows, cols uint16) error {
	return s.Send(&pb.Message{Kind: "resize", RequestId: wire.ID(), Payload: api.Payload(api.Resize{Rows: rows, Cols: cols})})
}
func (s *Stream) Signal(name string) error {
	return s.Send(&pb.Message{Kind: "signal", RequestId: wire.ID(), Data: []byte(name)})
}
func (c *Client) open(ctx context.Context, op, id string, payload any, r *api.Runtime) (*Stream, *pb.Message, error) {
	if e := ctx.Err(); e != nil {
		return nil, nil, e
	}
	raw, e := c.s.OpenStream()
	if e != nil {
		return nil, nil, e
	}
	st := &Stream{s: wire.Wrap(raw)}
	st.cancel = context.AfterFunc(ctx, func() { st.s.Close() })
	m := &pb.Message{Kind: "request", RequestId: id, Operation: op, Target: c.Binding.Target, Incarnation: c.Binding.Incarnation, ConnectionGeneration: c.Binding.Generation, Payload: api.Payload(payload)}
	if r != nil {
		m.RuntimeId = r.ID
		m.RuntimeGeneration = r.Generation
		m.RuntimeIncarnation = r.Incarnation
	}
	if e = st.Send(m); e != nil {
		st.Close()
		return nil, nil, e
	}
	res, e := st.Recv()
	if e != nil {
		st.Close()
		return nil, nil, e
	}
	if res.Kind != "accepted" {
		st.Close()
		return nil, nil, fmt.Errorf("expected application admission")
	}
	return st, res, nil
}

// CallID provides explicit finite deduplication. Never automatically retry an
// unknown result, and never reuse an ID for different payloads.
func (c *Client) CallID(ctx context.Context, op, id string, in, out any, r *api.Runtime) error {
	s, _, e := c.open(ctx, op, id, in, r)
	if e != nil {
		return e
	}
	defer s.Close()
	m, e := s.Recv()
	if e != nil {
		if ae, ok := e.(*api.Error); ok && ae.Code != "STREAM_INTERRUPTED" {
			return e
		}
		return &api.Error{Code: "RESULT_UNKNOWN", Detail: e.Error()}
	}
	if m.Kind != "result" {
		return fmt.Errorf("expected result")
	}
	if out != nil {
		return json.Unmarshal(m.Payload, out)
	}
	return nil
}
func (c *Client) Call(ctx context.Context, op string, in, out any) error {
	return c.CallID(ctx, op, wire.ID(), in, out, nil)
}
func (c *Client) Exec(ctx context.Context, a api.Exec) (api.ExecResult, error) {
	var r api.ExecResult
	e := c.Call(ctx, "exec", a, &r)
	return r, e
}
func (c *Client) Start(ctx context.Context, p api.Profile) (api.Runtime, *Stream, error) {
	var r api.Runtime
	s, _, e := c.open(ctx, "profile.start", wire.ID(), p, nil)
	if e != nil {
		return r, nil, e
	}
	for {
		m, e := s.Recv()
		if e != nil {
			s.Close()
			return r, nil, e
		}
		if m.Kind == "progress" {
			continue
		}
		if m.Kind != "result" {
			s.Close()
			return r, nil, fmt.Errorf("expected Runtime result")
		}
		e = wire.Decode(m, &r)
		s.terminal = false
		return r, s, e
	}
}
func (c *Client) List(ctx context.Context) ([]api.Runtime, error) {
	var r []api.Runtime
	e := c.Call(ctx, "runtime.list", struct{}{}, &r)
	return r, e
}
func (c *Client) Get(ctx context.Context, r api.Runtime) (api.Runtime, error) {
	var out api.Runtime
	e := c.CallID(ctx, "runtime.get", wire.ID(), struct{}{}, &out, &r)
	return out, e
}
func (c *Client) Stop(ctx context.Context, r api.Runtime) error {
	return c.CallID(ctx, "runtime.stop", wire.ID(), struct{}{}, nil, &r)
}
func (c *Client) Attach(ctx context.Context, r api.Runtime, observe bool) (*Stream, error) {
	s, _, e := c.open(ctx, "runtime.attach", wire.ID(), api.Attach{Observe: observe}, &r)
	return s, e
}

func (c *Client) Files(ctx context.Context, a api.File, out any) error {
	return c.Call(ctx, "files", a, out)
}
func (c *Client) Upload(ctx context.Context, a api.Upload) (api.UploadState, error) {
	var out api.UploadState
	e := c.Call(ctx, "upload", a, &out)
	return out, e
}
func (c *Client) Git(ctx context.Context, a api.Git) (api.GitResult, error) {
	var out api.GitResult
	e := c.Call(ctx, "git", a, &out)
	return out, e
}
func (c *Client) Connect(ctx context.Context, port int) (*Port, error) {
	s, _, e := c.open(ctx, "ports.connect", wire.ID(), api.Port{Port: port}, nil)
	if e != nil {
		return nil, e
	}
	return &Port{Stream: s}, nil
}

// Port uses application eof messages for half-close; Yamux Close is cancellation.
type Port struct {
	*Stream
	pending  []byte
	readEOF  bool
	writeMu  sync.Mutex
	writeEOF bool
}

func (p *Port) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for len(p.pending) == 0 {
		if p.readEOF {
			return 0, io.EOF
		}
		m, e := p.Recv()
		if e != nil {
			return 0, e
		}
		switch m.Kind {
		case "data":
			p.pending = m.Data
		case "eof":
			p.readEOF = true
		case "result":
			p.readEOF = true
		default:
			return 0, fmt.Errorf("invalid port event")
		}
	}
	n := copy(b, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}
func (p *Port) Write(b []byte) (int, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.writeEOF {
		return 0, io.ErrClosedPipe
	}
	total := 0
	for len(b) > 0 {
		n := len(b)
		if n > wire.ChunkSize {
			n = wire.ChunkSize
		}
		if e := p.Send(&pb.Message{Kind: "data", Data: b[:n]}); e != nil {
			return total, e
		}
		total += n
		b = b[n:]
	}
	return total, nil
}
func (p *Port) CloseWrite() error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.writeEOF {
		return nil
	}
	p.writeEOF = true
	return p.Send(&pb.Message{Kind: "eof"})
}

// Finish waits for explicit confirmation after both directions reach EOF.
func (p *Port) Finish() error {
	if !p.readEOF {
		return fmt.Errorf("read to EOF before Finish")
	}
	m, e := p.Recv()
	if e != nil {
		return e
	}
	if m.Kind != "result" {
		return fmt.Errorf("expected port close result")
	}
	return nil
}
