package access

import (
	"context"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
)

// Test callers create their key before invoking the public Start contract.
func testStartProfile(connection *client.Client, ctx context.Context, profile api.Profile) (api.Runtime, *client.Stream, error) {
	scope := testScope()
	target := api.SubmissionTarget{OwnerID: scope.OwnerID, RunnerID: scope.Binding.RunnerID, FabricID: scope.Binding.FabricID, MachineID: scope.Binding.MachineID, BindingRevision: scope.Binding.Revision}
	key := api.SubmissionKey{SubmissionID: wire.ID(), Target: target}
	result, stream, err := connection.Start(ctx, api.StartRequest{SubmissionKey: key, Profile: profile})
	if result.Stage == "started" && result.Runtime != nil {
		return *result.Runtime, stream, err
	}
	return api.Runtime{}, stream, err
}

func testACPSubmit(connection *client.Client, ctx context.Context, runtime api.Runtime, action api.ACPAction) (api.AgentOperation, error) {
	target := api.SubmissionTarget{OwnerID: testScope().OwnerID, RunnerID: testScope().Binding.RunnerID, FabricID: testScope().Binding.FabricID, MachineID: testScope().Binding.MachineID, BindingRevision: testScope().Binding.Revision}
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	return connection.ACPSubmit(ctx, api.SubmissionKey{SubmissionID: wire.ID(), Target: target}, action)
}

func testStopRuntime(connection *client.Client, ctx context.Context, runtime api.Runtime) error {
	target := api.SubmissionTarget{OwnerID: testScope().OwnerID, RunnerID: testScope().Binding.RunnerID, FabricID: testScope().Binding.FabricID, MachineID: testScope().Binding.MachineID, BindingRevision: testScope().Binding.Revision}
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	_, err := connection.Stop(ctx, api.SubmissionKey{SubmissionID: "test-stop", Target: target})
	return err
}
