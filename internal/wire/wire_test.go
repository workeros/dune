package wire

import (
	"bytes"
	"encoding/binary"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
	"google.golang.org/protobuf/proto"
	"io"
	"net"
	"testing"
	"time"
)

type fragments struct{ io.Reader }

func (f fragments) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return f.Reader.Read(p)
}

func TestCloseUnblocksIdleReadWithoutPeerClose(t *testing.T) {
	local, remote := net.Pipe()
	client, err := yamux.Client(local, Config())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := yamux.Server(remote, Config())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	raw, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	stream := Wrap(raw)
	done := make(chan error, 1)
	go func() { _, err := stream.Recv(); done <- err }()
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close waited for peer FIN")
	}
	if err := stream.Send(&pb.Message{Kind: "data"}); err == nil {
		t.Fatal("closed write succeeded")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestFraming(t *testing.T) {
	m := &pb.Message{Kind: "data", Data: bytes.Repeat([]byte("x"), 65536)}
	var b bytes.Buffer
	if e := Write(&b, m); e != nil {
		t.Fatal(e)
	}
	out, e := Read(fragments{&b})
	if e != nil || !proto.Equal(m, out) {
		t.Fatal(e)
	}
	for _, input := range [][]byte{{0, 0}, {0, 0, 0, 8, 1}, {0, 0, 0, 0}, {255, 255, 255, 255}} {
		if _, e := Read(bytes.NewReader(input)); e == nil {
			t.Fatalf("accepted %v", input)
		}
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], MaxMessage+1)
	if _, e := Read(bytes.NewReader(h[:])); e == nil {
		t.Fatal("oversize allocation")
	}
}

func TestFramingAllowsMultiMiBMessage(t *testing.T) {
	m := &pb.Message{Kind: "data", Data: bytes.Repeat([]byte("x"), 2*1024*1024)}
	var b bytes.Buffer
	if err := Write(&b, m); err != nil {
		t.Fatal(err)
	}
	out, err := Read(&b)
	if err != nil || !proto.Equal(m, out) {
		t.Fatal(err)
	}
}
