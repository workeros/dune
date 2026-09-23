package runnerupgrade

import (
	"context"
	"os"
	"path/filepath"
	"reflect"

	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/runningprogram"
	"github.com/aiomni/dune/internal/upgradejob"
	"github.com/aiomni/dune/pkg/upgrade"
)

// StartedAttempt is captured once before a connector opens its dependencies.
// A later probe must never turn an already-running process into a new startup.
// Absence or uncertainty withholds evidence; it does not block session recovery.
func StartedAttempt(ctx context.Context, stateDir string) *upgrade.Probe {
	registration, err := installation.Find(stateDir)
	if err != nil {
		return nil
	}
	seal, err := launchgate.ReadSeal(stateDir)
	if err != nil || seal == nil || seal.InstallationID != registration.ID {
		return nil
	}
	jobs, err := upgradejob.OpenReadOnly(ctx, filepath.Join(registration.Root, "upgrades"))
	if err != nil {
		return nil
	}
	defer jobs.Close()
	record, err := jobs.Active(ctx)
	if err != nil || record == nil {
		return nil
	}
	op := record.Operation
	if op.Confirmed || !op.LaunchSealed || op.ID != seal.OperationID || op.Request.InstallationID != seal.InstallationID || op.AttemptID == "" || op.Challenge == "" {
		return nil
	}
	expected := op.Target
	rollback := false
	switch op.Phase {
	case upgrade.Switching, upgrade.Reconnecting, upgrade.Verifying:
	case upgrade.RollingBack, upgrade.RollbackVerifying:
		if op.Source.Installation == nil {
			return nil
		}
		expected, rollback = op.Source.Installation.Release, true
	default:
		return nil
	}
	observed, err := installation.View(ctx, registration.Root)
	if err != nil {
		return nil
	}
	wanted, err := expected.Digest()
	actual, actualErr := observed.Release.Digest()
	if err != nil || actualErr != nil || actual != wanted || (!rollback && !observed.Complete) || (rollback && !reflect.DeepEqual(observed.Components, op.Source.Installation.Components)) {
		return nil
	}
	// Equal content alone is insufficient: an old release inode can still be
	// executing while current selects identical Dune with different helpers.
	image, err := runningprogram.Open()
	if err != nil {
		return nil
	}
	defer image.Close()
	running, err := image.Stat()
	selected, selectedErr := os.Stat(filepath.Join(registration.Root, "current", "dune"))
	if err != nil || selectedErr != nil || !os.SameFile(running, selected) {
		return nil
	}
	latest, err := jobs.Active(ctx)
	// Reconnecting → verifying may commit while startup measures the files.
	// That progress preserves the attempt; rollback or completion does not.
	if err != nil || latest == nil || latest.Operation.Probe() != op.Probe() || latest.Operation.Confirmed || !latest.Operation.LaunchSealed {
		return nil
	}
	probe := op.Probe()
	return &probe
}
