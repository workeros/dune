package access

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func TestSubmissionQueriesAuthorizeExactScopeWithoutActiveRuntime(t *testing.T) {
	var denied, revoked atomic.Bool
	ctx, client := policyFixture(t, checkFunc(func(ctx context.Context, request Request) (Decision, error) {
		decision, err := (Owner{}).Check(ctx, request)
		if request.Operation == "submission.get" {
			decision.Allowed = decision.Allowed && !denied.Load()
		}
		return decision, err
	}), func() bool { return !revoked.Load() })
	scope := testScope()
	key := api.SubmissionKey{SubmissionID: "original-submit", Target: api.SubmissionTarget{
		OwnerID: scope.OwnerID, RunnerID: scope.Binding.RunnerID, FabricID: scope.Binding.FabricID,
		MachineID: scope.Binding.MachineID, BindingRevision: scope.Binding.Revision,
		RuntimeID: "runtime-no-longer-listed", RuntimeIncarnation: "original-boot", RuntimeGeneration: 1,
	}}
	if receipt, err := client.QuerySubmission(ctx, key); err != nil || receipt.Admission != api.SubmissionUnknown || receipt.SubmissionKey != key {
		t.Fatalf("original absent Runtime could not be queried: %+v, %v", receipt, err)
	}
	for _, change := range []func(*api.SubmissionKey){
		func(k *api.SubmissionKey) { k.Target.OwnerID = "other-owner" },
		func(k *api.SubmissionKey) { k.Target.RunnerID = "other-runner" },
		func(k *api.SubmissionKey) { k.Target.FabricID = "other-fabric" },
		func(k *api.SubmissionKey) { k.Target.MachineID = "other-machine" },
		func(k *api.SubmissionKey) { k.Target.BindingRevision++ },
	} {
		wrong := key
		change(&wrong)
		if _, err := client.QuerySubmission(ctx, wrong); err == nil {
			t.Fatal("query escaped the authenticated scope", wrong)
		}
	}
	wrongRuntime := api.Runtime{ID: "another-runtime", Incarnation: key.Target.RuntimeIncarnation, Generation: 1}
	if err := client.CallID(ctx, "submission.get", wire.ID(), key, nil, &wrongRuntime); err == nil {
		t.Fatal("payload target bypassed the exact outer Runtime selector")
	}
	denied.Store(true)
	if _, err := client.QuerySubmission(ctx, key); err == nil {
		t.Fatal("current policy denial did not prevent receipt access")
	}
	denied.Store(false)
	revoked.Store(true)
	if _, err := client.QuerySubmission(ctx, key); err == nil {
		t.Fatal("revoked connection read submission evidence")
	}
}

func TestSubmissionEnvelopeDescribesActualBusinessAction(t *testing.T) {
	scope := testScope()
	key := api.SubmissionKey{SubmissionID: "caller-id", Target: api.SubmissionTarget{
		OwnerID: scope.OwnerID, RunnerID: scope.Binding.RunnerID, FabricID: scope.Binding.FabricID,
		MachineID: scope.Binding.MachineID, BindingRevision: scope.Binding.Revision,
		RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1,
	}}
	request := api.SubmissionRequest{SubmissionKey: key, Operation: "acp.action", Payload: api.Payload(api.ACPAction{Action: "load", Cwd: "/allowed"})}
	message := &pb.Message{Kind: "request", RequestId: "transport", Operation: "submission.acp", Target: key.Target.MachineID, RuntimeId: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1, Payload: api.Payload(request)}
	description, err := Describe(scope, message)
	if err != nil || description.Operation != "acp.action" || description.Suboperation != "load" || description.Resource.Directory != "/allowed" {
		t.Fatal("envelope hid business policy inputs", description, err)
	}
	request.Target.OwnerID = "forged-owner"
	message.Payload = api.Payload(request)
	if _, err := Describe(scope, message); err == nil {
		t.Fatal("submission key escaped owner scope")
	}
}

func testACPEnvelope(message *pb.Message) *pb.Message {
	scope := testScope()
	message.RuntimeId, message.RuntimeIncarnation, message.RuntimeGeneration = "runtime", "host", 1
	key := api.SubmissionKey{SubmissionID: "caller-id", Target: api.SubmissionTarget{OwnerID: scope.OwnerID, RunnerID: scope.Binding.RunnerID, FabricID: scope.Binding.FabricID, MachineID: scope.Binding.MachineID, BindingRevision: scope.Binding.Revision, RuntimeID: message.RuntimeId, RuntimeIncarnation: message.RuntimeIncarnation, RuntimeGeneration: message.RuntimeGeneration}}
	message.Payload = api.Payload(api.SubmissionRequest{SubmissionKey: key, Operation: message.Operation, Payload: message.Payload})
	message.Operation = "submission.acp"
	return message
}

func TestManagedActionsRequireIdentifiedEnvelope(t *testing.T) {
	message := &pb.Message{Kind: "request", Target: "machine", RequestId: "transport-id", Operation: "acp.action", Payload: api.Payload(api.ACPAction{Action: "permission", PermissionID: "permission", OptionID: "allow"})}
	if _, err := Describe(testScope(), message); !errors.Is(err, ErrDenied) {
		t.Fatal("unidentified action was authorized", err)
	}
	message = testACPEnvelope(message)
	if request, err := Describe(testScope(), message); err != nil || request.Operation != "acp.action" || request.Suboperation != "permission" {
		t.Fatal("identified action lost business authorization", request, err)
	}
}

func TestStopRequiresOriginalIdentifiedRuntimeScope(t *testing.T) {
	message := &pb.Message{Kind: "request", Target: "machine", RequestId: "transport-id", Operation: "runtime.stop", Payload: api.Payload(struct{}{})}
	if _, err := Describe(testScope(), message); !errors.Is(err, ErrDenied) {
		t.Fatal("unidentified stop was authorized", err)
	}
	message = testACPEnvelope(message)
	message.Operation = "runtime.stop"
	if description, err := Describe(testScope(), message); err != nil || description.Operation != "runtime.stop" {
		t.Fatal("identified stop lost its authorization vocabulary", description, err)
	}
	message.RuntimeIncarnation = "replacement-host"
	if _, err := Describe(testScope(), message); !errors.Is(err, ErrDenied) {
		t.Fatal("stop crossed original Runtime identity", err)
	}
}
