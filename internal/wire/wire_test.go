package wire

import (
	"bytes"
	"encoding/binary"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"google.golang.org/protobuf/proto"
	"io"
	"testing"
)

type fragments struct{ io.Reader }

func (f fragments) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return f.Reader.Read(p)
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
