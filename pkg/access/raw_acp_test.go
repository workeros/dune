package access

import (
	"testing"

	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func TestRawSubmissionsAuthorizeActualActionAndOriginalScope(t *testing.T) {
	scope := testScope()
	key := api.SubmissionKey{SubmissionID: "original", Target: api.SubmissionTarget{OwnerID: scope.OwnerID, RunnerID: scope.Binding.RunnerID, FabricID: scope.Binding.FabricID, MachineID: scope.Binding.MachineID, BindingRevision: scope.Binding.Revision, RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1}}
	for _, action := range []string{"take", "write"} {
		request := api.SubmissionRequest{SubmissionKey: key, Operation: "acp.raw." + action, Payload: api.Payload(map[string]string{"owner_id": "private-owner", "body": "private-body"})}
		message := &pb.Message{Kind: "request", RequestId: "transport", Operation: "submission.raw", Target: key.Target.MachineID, RuntimeId: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1, Payload: api.Payload(request)}
		description, err := Describe(scope, message)
		if err != nil || description.Operation != "acp.raw" || description.Suboperation != action || description.Resource != (Resource{}) {
			t.Fatal(description, err)
		}
		for _, alter := range []func(*api.SubmissionKey){
			func(k *api.SubmissionKey) { k.Target.OwnerID = "another-owner" },
			func(k *api.SubmissionKey) { k.Target.RunnerID = "another-runner" },
			func(k *api.SubmissionKey) { k.Target.BindingRevision++ },
			func(k *api.SubmissionKey) { k.Target.RuntimeIncarnation = "another-host" },
		} {
			forged := request
			alter(&forged.SubmissionKey)
			message.Payload = api.Payload(forged)
			if _, err := Describe(scope, message); err == nil {
				t.Fatal("raw input escaped exact authorized scope")
			}
		}
	}
	for _, kind := range []string{"input", "signal"} {
		if _, err := continuation(Request{Operation: "runtime.attach"}, RuntimeIdentity{ID: "runtime", Adapter: "acp"}, &pb.Message{Kind: kind, Data: []byte("TERM")}); err == nil {
			t.Fatal("old stream bypasses raw submission", kind)
		}
	}
}
