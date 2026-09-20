package fabricd

import (
	"context"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
)

// Test callers create their key before invoking the public Start contract.
func testStartProfile(connection *client.Client, ctx context.Context, profile api.Profile) (api.Runtime, *client.Stream, error) {
	target := api.SubmissionTarget{OwnerID: "test-owner", RunnerID: "test-runner", FabricID: "test-fabric", MachineID: connection.Binding.Target, BindingRevision: 1}
	key := api.SubmissionKey{SubmissionID: wire.ID(), Target: target}
	result, stream, err := connection.Start(ctx, api.StartRequest{SubmissionKey: key, Profile: profile})
	if result.Stage == "started" && result.Runtime != nil {
		return *result.Runtime, stream, err
	}
	return api.Runtime{}, stream, err
}

func testStopRuntime(connection *client.Client, ctx context.Context, runtime api.Runtime) error {
	target := api.SubmissionTarget{OwnerID: "test-owner", RunnerID: "test-runner", FabricID: "test-fabric", MachineID: connection.Binding.Target, BindingRevision: 1}
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	_, err := connection.Stop(ctx, api.SubmissionKey{SubmissionID: "test-stop", Target: target})
	return err
}
