package wire

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
	"google.golang.org/protobuf/proto"
)

const MaxMessage = 1024 * 1024
const ChunkSize = 32 * 1024
const MaxStreams = 64
const WriteTimeout = 5 * time.Second

func ID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func ValidID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}

func Config() *yamux.Config {
	c := yamux.DefaultConfig()
	c.AcceptBacklog = MaxStreams
	c.MaxStreamWindowSize = 256 * 1024
	c.StreamOpenTimeout = 5 * time.Second
	c.StreamCloseTimeout = 5 * time.Second
	c.ConnectionWriteTimeout = 5 * time.Second
	c.LogOutput = io.Discard
	return c
}

type Stream struct {
	*yamux.Stream
	mu     sync.Mutex
	closed atomic.Bool
}

func Wrap(s *yamux.Stream) *Stream { return &Stream{Stream: s} }
func Read(r io.Reader) (*pb.Message, error) {
	var h [4]byte
	if _, e := io.ReadFull(r, h[:]); e != nil {
		return nil, e
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > MaxMessage {
		return nil, fmt.Errorf("invalid message size %d", n)
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(r, b); e != nil {
		return nil, e
	}
	m := new(pb.Message)
	if e := proto.Unmarshal(b, m); e != nil {
		return nil, e
	}
	return m, nil
}
func Write(w io.Writer, m *pb.Message) error {
	b, e := proto.Marshal(m)
	if e != nil {
		return e
	}
	if len(b) == 0 || len(b) > MaxMessage {
		return fmt.Errorf("message exceeds limit")
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	if e = writeAll(w, h[:]); e != nil {
		return e
	}
	return writeAll(w, b)
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func (s *Stream) Send(m *pb.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return net.ErrClosed
	}
	_ = s.SetWriteDeadline(time.Now().Add(WriteTimeout))
	return Write(s.Stream, m)
}
func (s *Stream) Recv() (*pb.Message, error) {
	if s.closed.Load() {
		return nil, net.ErrClosed
	}
	return Read(s.Stream)
}

// Close cancels both directions. Yamux's Close alone is a local half-close and
// can leave an idle reader blocked until the peer responds or a timer expires.
func (s *Stream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	_ = s.Stream.SetDeadline(time.Now())
	return s.Stream.Close()
}
func (s *Stream) Fail(code string, e error) {
	_ = s.Send(&pb.Message{Kind: "error", Code: code, Detail: e.Error()})
}
func Decode(m *pb.Message, v any) error {
	if len(m.Payload) == 0 {
		return fmt.Errorf("missing payload")
	}
	return json.Unmarshal(m.Payload, v)
}
func Error(m *pb.Message) error {
	if m.Kind == "error" {
		return &api.Error{Code: m.Code, Detail: m.Detail}
	}
	return nil
}
func Handshake(s *yamux.Session, m *pb.Message) (*Stream, *pb.Message, error) {
	raw, e := s.OpenStream()
	if e != nil {
		return nil, nil, e
	}
	st := Wrap(raw)
	_ = st.SetDeadline(time.Now().Add(5 * time.Second))
	if e = st.Send(m); e != nil {
		st.Close()
		return nil, nil, e
	}
	res, e := st.Recv()
	_ = st.SetDeadline(time.Time{})
	if e == nil {
		e = Error(res)
	}
	if e == nil && res.Kind != "welcome" {
		e = fmt.Errorf("expected welcome")
	}
	if e != nil {
		st.Close()
		return nil, nil, e
	}
	return st, res, nil
}
