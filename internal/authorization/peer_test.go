package authorization

import (
	"testing"

	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"google.golang.org/protobuf/proto"
)

func TestPeerDigestBindsBusinessAndExecutionIdentity(t *testing.T) {
	base := &pb.Message{Kind: "request", Target: "machine", RequestId: "request", Operation: "files", Payload: []byte(`{"action":"write","path":"file"}`), Data: []byte("private file bytes"), Incarnation: "boot", ConnectionGeneration: 1, RuntimeId: "runtime", RuntimeIncarnation: "runtime-boot", RuntimeGeneration: 1, RouteEpoch: 1}
	digest, err := peerDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*pb.Message){
		func(m *pb.Message) { m.Target += "x" }, func(m *pb.Message) { m.RequestId += "x" },
		func(m *pb.Message) { m.Operation += "x" }, func(m *pb.Message) { m.Payload = []byte(`{"action":"remove"}`) },
		func(m *pb.Message) { m.Data = []byte("replacement") }, func(m *pb.Message) { m.Incarnation += "x" },
		func(m *pb.Message) { m.ConnectionGeneration++ }, func(m *pb.Message) { m.RuntimeId += "x" },
		func(m *pb.Message) { m.RuntimeIncarnation += "x" }, func(m *pb.Message) { m.RuntimeGeneration++ },
		func(m *pb.Message) { m.RouteEpoch++ },
	}
	for i, mutate := range mutations {
		copy := proto.Clone(base).(*pb.Message)
		mutate(copy)
		if got, err := peerDigest(copy); err != nil || got == digest {
			t.Fatal("unbound request field", i, err)
		}
	}
	copy := proto.Clone(base).(*pb.Message)
	copy.AccessContext, copy.InputLeaseId, copy.InputLeaseMs = []byte("capability"), "grant", 15000
	if got, err := peerDigest(copy); err != nil || got != digest {
		t.Fatal("transport grant changed business digest", err)
	}
	if len(base.AccessContext) != 0 || base.InputLeaseId != "" || string(base.Data) != "private file bytes" {
		t.Fatal("digest mutated caller message")
	}
}
