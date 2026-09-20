package wire

import (
	"sync"
	"testing"

	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"google.golang.org/protobuf/proto"
)

func TestRequestClassUsesDecodedBusinessActionAndExactTarget(t *testing.T) {
	key := api.SubmissionKey{SubmissionID: "original", Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1, RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1}}
	m := &pb.Message{Kind: "request", RequestId: "transport", Target: "machine", RuntimeId: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1}
	for action, want := range map[string]StreamClass{"permission": StreamPermission, "cancel": StreamCancel, "prompt": StreamOrdinary, "new": StreamOrdinary, "load": StreamOrdinary, "list": StreamOrdinary} {
		request := api.SubmissionRequest{SubmissionKey: key, Operation: "acp.action", Payload: api.Payload(api.ACPAction{Action: action, OperationRef: "operation", PermissionID: "permission", OptionID: "allow"})}
		m.Operation, m.Payload = "submission.acp", api.Payload(request)
		if got := RequestClass(m); got != want {
			t.Fatal(action, got)
		}
		for _, mutate := range []func(*pb.Message){
			func(m *pb.Message) { m.RuntimeIncarnation = "replacement" },
			func(m *pb.Message) { m.Target = "another-machine" },
			func(m *pb.Message) { m.Operation = "runtime.stop" },
			func(m *pb.Message) { m.Payload = []byte(`{"operation":"acp.action","action":"permission"}`) },
		} {
			invalid := proto.Clone(m).(*pb.Message)
			mutate(invalid)
			if RequestClass(invalid) != StreamOrdinary {
				t.Fatal("invalid envelope borrowed a reservation", invalid)
			}
		}
	}
	for operation, want := range map[string]StreamClass{"runtime.stop": StreamStop, "runtime.forget": StreamForget} {
		m.Operation = operation
		m.Payload = api.Payload(api.SubmissionRequest{SubmissionKey: key, Operation: operation})
		if RequestClass(m) != want {
			t.Fatal(operation)
		}
	}
	m.Operation, m.Payload = "submission.get", api.Payload(key)
	if RequestClass(m) != StreamRead {
		t.Fatal("original query lost read reservation")
	}
	m.Operation = "agent.operation.wait"
	if RequestClass(m) != StreamWait {
		t.Fatal("long poll can occupy state readers")
	}
}

func TestStreamCapacityBoundsIndependentClassesAndConcurrentRelease(t *testing.T) {
	capacity := NewStreamCapacity(1)
	for class, usage := range capacity.Snapshot() {
		var leases []*StreamLease
		for range usage.Limit {
			lease := capacity.Acquire(StreamClass(class))
			if lease == nil {
				t.Fatal("capacity borrowed by another class", class)
			}
			leases = append(leases, lease)
		}
		if capacity.Acquire(StreamClass(class)) != nil {
			t.Fatal("unbounded class", class)
		}
		t.Cleanup(func() {
			var wg sync.WaitGroup
			for _, lease := range leases {
				for range 3 {
					wg.Go(lease.Release)
				}
			}
			wg.Wait()
			if capacity.Snapshot()[class].Used != 0 {
				t.Fatal("capacity leaked", class)
			}
		})
	}
}

func TestFirstMessageSlotsReturnBeforeOperationFinishes(t *testing.T) {
	c := NewStreamCapacity(1)
	for range MaxStreams {
		l := c.Acquire(StreamOpening)
		if l == nil || !l.Move(StreamOrdinary) {
			t.Fatal("ordinary work retained first-message capacity")
		}
		defer l.Release()
	}
	l := c.Acquire(StreamOpening)
	defer l.Release()
	if l.Move(StreamOrdinary) || c.Snapshot()[string(StreamOpening)].Used != 1 {
		t.Fatal("rejection was not bounded")
	}
	if !l.Move(StreamStop) || c.Snapshot()[string(StreamOpening)].Used != 0 {
		t.Fatal("ordinary saturation blocked stop")
	}
}
