package fabricd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/pkg/api"
)

func TestUpgradeSealRejectsLaunchAcrossConnectorRestart(t *testing.T) {
	h := newCleanupProcessHarness(t)
	h.start("")
	gate, err := launchgate.Acquire(h.state, true)
	if err != nil {
		t.Fatal(err)
	}
	seal := launchgate.Seal{InstallationID: "installation", OperationID: "operation", Owner: "worker"}
	if err := gate.ClaimSeal(nil, seal); err != nil {
		t.Fatal(err)
	}
	gate.Close()
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	request := api.StartRequest{
		SubmissionKey: api.SubmissionKey{SubmissionID: "sealed-launch", Target: api.SubmissionTarget{OwnerID: "test-owner", RunnerID: "test-runner", FabricID: "test-fabric", MachineID: h.client.Binding.Target, BindingRevision: 1}},
		Profile:       api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: filepath.Dir(marker), Start: api.Command{Argv: []string{"touch", marker}}},
	}
	assertRejected := func() {
		t.Helper()
		result, stream, err := h.client.Start(h.ctx, request)
		if stream != nil {
			stream.Close()
		}
		var failure *api.Error
		if !errors.As(err, &failure) || failure.Code != "UPGRADE_IN_PROGRESS" || result.Admission != api.SubmissionNotAccepted {
			t.Fatalf("unsealed original submission: %+v %v", result, err)
		}
		observed, err := h.client.QuerySubmission(h.ctx, request.SubmissionKey)
		if err != nil || observed.Admission != api.SubmissionNotAccepted || observed.ErrorCode != "UPGRADE_IN_PROGRESS" {
			t.Fatal(observed, err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("rejected launch created a process", err)
		}
	}
	assertRejected()
	h.kill()
	h.start("")
	assertRejected()
	recovery, err := launchgate.Acquire(h.state, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.ClearSeal(seal); err != nil {
		t.Fatal(err)
	}
	recovery.Close()
	assertRejected()
}
