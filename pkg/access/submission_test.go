package access

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
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
