package tests

import (
	"context"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
)

// Test callers create their key before invoking the public Start contract.
func testStartProfile(connection *client.Client, ctx context.Context, profile api.Profile) (api.Runtime, *client.Stream, error) {
	target := api.SubmissionTarget{OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: connection.Binding.Target, BindingRevision: 1}
	key := api.SubmissionKey{SubmissionID: wire.ID(), Target: target}
	result, stream, err := connection.Start(ctx, api.StartRequest{SubmissionKey: key, Profile: profile})
	if result.Stage == "started" && result.Runtime != nil {
		return *result.Runtime, stream, err
	}
	return api.Runtime{}, stream, err
}

func testACPSubmit(connection *client.Client, ctx context.Context, runtime api.Runtime, action api.ACPAction) (api.AgentOperation, error) {
	target := api.SubmissionTarget{OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: connection.Binding.Target, BindingRevision: 1}
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	return connection.ACPSubmit(ctx, api.SubmissionKey{SubmissionID: wire.ID(), Target: target}, action)
}
