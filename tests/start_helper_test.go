package tests

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

func testACPControl(connection *client.Client, ctx context.Context, runtime api.Runtime, action api.ACPAction) (api.SubmissionReceipt, error) {
	target := api.SubmissionTarget{OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: connection.Binding.Target, BindingRevision: 1, RuntimeID: runtime.ID, RuntimeIncarnation: runtime.Incarnation, RuntimeGeneration: runtime.Generation}
	return connection.ACPControl(ctx, api.SubmissionKey{SubmissionID: wire.ID(), Target: target}, action)
}

func testStopRuntime(connection *client.Client, ctx context.Context, runtime api.Runtime) error {
	target := api.SubmissionTarget{OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: connection.Binding.Target, BindingRevision: 1}
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	_, err := connection.Stop(ctx, api.SubmissionKey{SubmissionID: "test-stop", Target: target})
	return err
}

func testForgetRuntime(connection *client.Client, ctx context.Context, runtime api.Runtime) error {
	target := api.SubmissionTarget{OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: connection.Binding.Target, BindingRevision: 1}
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	receipt, err := connection.Forget(ctx, api.SubmissionKey{SubmissionID: "test-forget", Target: target})
	if err == nil && receipt.Stage != "completed" {
		return fmt.Errorf("cleanup remains %s: %s", receipt.Stage, receipt.ErrorCode)
	}
	return err
}

// Only the immutable executable is shared; every test still owns its process,
// working directory, logs and session state. TestMain removes the binary directory.
var buildMockACP = sync.OnceValues(func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	path := filepath.Join(filepath.Dir(binary), "mock-acp")
	output, err := exec.CommandContext(ctx, "go", "build", "-o", path, "../samples/mock-acp").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build mock ACP: %w\n%s", err, output)
	}
	return path, nil
})

func mockACPBinary(t *testing.T) string {
	t.Helper()
	path, err := buildMockACP()
	if err != nil {
		t.Fatal(err)
	}
	return path
}
